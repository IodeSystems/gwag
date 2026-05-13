package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UploadStore stages partially-uploaded files. Both the inline
// graphql-multipart-request-spec parser (single-request small files)
// and the tus.io HTTP endpoints (chunked / resumable large files)
// land bytes in a store; the GraphQL Upload scalar opens the assembled
// body when a resolver consumes the *Upload value.
//
// Implementations must be safe for concurrent use. A single upload id
// is not concurrent-safe across multiple Append callers — the tus.io
// spec serialises chunk appends per upload — but distinct ids must
// not interfere.
//
// Stability: stable
type UploadStore interface {
	// Create allocates a new upload slot. Returns an opaque id.
	// length == -1 means "deferred" (tus Upload-Defer-Length); the
	// total is fixed by the isFinal=true Append.
	Create(ctx context.Context, meta UploadMeta) (id string, err error)

	// Append writes chunk bytes starting at the declared offset.
	// Returns the new offset after the write. isFinal=true commits
	// the upload as complete (size matches Length, or, if Length was
	// deferred, fixes the total to the new offset). Mismatched
	// offset returns ErrUploadOffsetConflict; exceeding the declared
	// length returns ErrUploadLengthExceeded.
	Append(ctx context.Context, id string, offset int64, chunk io.Reader, isFinal bool) (newOffset int64, err error)

	// Info returns the stored metadata + current offset for id.
	// Returns ErrUploadNotFound when id is unknown.
	Info(ctx context.Context, id string) (UploadInfo, error)

	// Open returns a reader over the complete upload body. Returns
	// ErrUploadIncomplete if Append has not been called with
	// isFinal=true (or the offset hasn't reached Length). Caller
	// closes.
	Open(ctx context.Context, id string) (io.ReadCloser, error)

	// Delete removes the upload and any backing storage. Idempotent
	// on missing ids — returns nil even if id was never created.
	Delete(ctx context.Context, id string) error
}

// UploadMeta is the metadata supplied at Create time.
//
// Stability: stable
type UploadMeta struct {
	Filename    string
	ContentType string
	Length      int64             // -1 → deferred (tus Upload-Defer-Length)
	Metadata    map[string]string // tus Upload-Metadata key→value
}

// UploadInfo is the runtime state Info returns.
//
// Stability: stable
type UploadInfo struct {
	UploadMeta
	Offset    int64
	Complete  bool
	CreatedAt time.Time
}

// Upload-store sentinel errors. Implementations classify failures
// against these so the tus HTTP layer can map to the right status
// (404 / 409 / 413).
var (
	ErrUploadNotFound       = errors.New("upload: not found")
	ErrUploadIncomplete     = errors.New("upload: incomplete")
	ErrUploadOffsetConflict = errors.New("upload: offset conflict")
	ErrUploadLengthExceeded = errors.New("upload: declared length exceeded")
)

// --- filesystem store -----------------------------------------------

// FilesystemUploadStore is the default UploadStore: each upload lives
// under <root>/<id>/ with `meta.json` + `body.dat`. A background
// goroutine evicts entries older than TTL.
//
// Stability: stable
type FilesystemUploadStore struct {
	root string
	ttl  time.Duration

	mu     sync.Mutex
	closed bool
	stopCh chan struct{}
}

// NewFilesystemUploadStore creates the root directory if missing and
// starts the TTL eviction loop. The store does not delete the root on
// close — adopters running ephemeral gateways should pass a tempdir
// they clean up themselves.
//
// Stability: stable
func NewFilesystemUploadStore(root string, ttl time.Duration) (*FilesystemUploadStore, error) {
	if root == "" {
		return nil, fmt.Errorf("upload store: empty root")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("upload store: mkdir %s: %w", root, err)
	}
	s := &FilesystemUploadStore{
		root:   root,
		ttl:    ttl,
		stopCh: make(chan struct{}),
	}
	go s.evictLoop()
	return s, nil
}

// Close stops the eviction loop. Idempotent.
func (s *FilesystemUploadStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.stopCh)
}

func (s *FilesystemUploadStore) evictLoop() {
	t := time.NewTicker(s.ttl / 4)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.evictExpired()
		}
	}
}

func (s *FilesystemUploadStore) evictExpired() {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-s.ttl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.root, e.Name()))
	}
}

func (s *FilesystemUploadStore) dir(id string) string { return filepath.Join(s.root, id) }

func (s *FilesystemUploadStore) Create(_ context.Context, meta UploadMeta) (string, error) {
	id, err := newUploadID()
	if err != nil {
		return "", err
	}
	d := s.dir(id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("upload store: mkdir: %w", err)
	}
	info := UploadInfo{
		UploadMeta: meta,
		Offset:     0,
		Complete:   meta.Length == 0, // zero-length declared = already complete
		CreatedAt:  time.Now(),
	}
	if err := writeMeta(d, info); err != nil {
		_ = os.RemoveAll(d)
		return "", err
	}
	// Touch body file so subsequent Append can OpenFile O_APPEND.
	bf, err := os.OpenFile(filepath.Join(d, "body.dat"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.RemoveAll(d)
		return "", fmt.Errorf("upload store: create body: %w", err)
	}
	_ = bf.Close()
	return id, nil
}

func (s *FilesystemUploadStore) Append(ctx context.Context, id string, offset int64, chunk io.Reader, isFinal bool) (int64, error) {
	d := s.dir(id)
	info, err := readMeta(d)
	if err != nil {
		return 0, err
	}
	if info.Complete {
		return info.Offset, ErrUploadOffsetConflict
	}
	if offset != info.Offset {
		return info.Offset, ErrUploadOffsetConflict
	}
	f, err := os.OpenFile(filepath.Join(d, "body.dat"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return info.Offset, fmt.Errorf("upload store: open body: %w", err)
	}
	defer f.Close()

	// Cap the read to the declared total when known so a misbehaving
	// client can't overshoot Upload-Length.
	src := chunk
	if info.Length >= 0 {
		remain := info.Length - info.Offset
		if remain <= 0 {
			return info.Offset, ErrUploadLengthExceeded
		}
		src = io.LimitReader(chunk, remain+1)
	}

	written, err := io.Copy(f, src)
	if err != nil {
		// Truncate any partial bytes we managed to write so the
		// offset stays consistent with what readers will see.
		_ = f.Truncate(info.Offset)
		return info.Offset, fmt.Errorf("upload store: write body: %w", err)
	}
	newOffset := info.Offset + written
	if info.Length >= 0 && newOffset > info.Length {
		// Overshoot: trim and surface.
		_ = f.Truncate(info.Length)
		info.Offset = info.Length
		_ = writeMeta(d, info)
		return info.Length, ErrUploadLengthExceeded
	}
	info.Offset = newOffset
	if isFinal {
		if info.Length < 0 {
			info.Length = newOffset
		}
		if info.Offset == info.Length {
			info.Complete = true
		}
	} else if info.Length >= 0 && info.Offset == info.Length {
		info.Complete = true
	}
	if err := writeMeta(d, info); err != nil {
		return info.Offset, err
	}
	_ = ctx
	return info.Offset, nil
}

func (s *FilesystemUploadStore) Info(_ context.Context, id string) (UploadInfo, error) {
	return readMeta(s.dir(id))
}

func (s *FilesystemUploadStore) Open(_ context.Context, id string) (io.ReadCloser, error) {
	d := s.dir(id)
	info, err := readMeta(d)
	if err != nil {
		return nil, err
	}
	if !info.Complete {
		return nil, ErrUploadIncomplete
	}
	return os.Open(filepath.Join(d, "body.dat"))
}

func (s *FilesystemUploadStore) Delete(_ context.Context, id string) error {
	if id == "" {
		return nil
	}
	err := os.RemoveAll(s.dir(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("upload store: delete: %w", err)
	}
	return nil
}

func writeMeta(d string, info UploadInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("upload store: marshal meta: %w", err)
	}
	// Write to tmp + rename so a crash mid-write can't corrupt the
	// stored metadata.
	tmp := filepath.Join(d, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("upload store: write meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(d, "meta.json")); err != nil {
		return fmt.Errorf("upload store: rename meta: %w", err)
	}
	return nil
}

func readMeta(d string) (UploadInfo, error) {
	b, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return UploadInfo{}, ErrUploadNotFound
		}
		return UploadInfo{}, fmt.Errorf("upload store: read meta: %w", err)
	}
	var info UploadInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return UploadInfo{}, fmt.Errorf("upload store: parse meta: %w", err)
	}
	return info, nil
}

// newUploadID returns a URL-safe random 32-char hex id. Cryptographically
// random because the id is the only thing standing between an
// authenticated tus client and a peer's upload — predictable ids would
// let one client query / append to another's slot.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("upload store: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
