package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// TestFilesystemUploadStore_HappyPath pins the canonical lifecycle:
// Create → Append (final) → Info reports Complete → Open returns the
// assembled bytes → Delete removes.
func TestFilesystemUploadStore_HappyPath(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	id, err := s.Create(ctx, UploadMeta{Filename: "hello.txt", ContentType: "text/plain", Length: 11})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatalf("Create: empty id")
	}

	off, err := s.Append(ctx, id, 0, bytes.NewReader([]byte("hello world")), true)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off != 11 {
		t.Errorf("offset = %d; want 11", off)
	}

	info, err := s.Info(ctx, id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.Complete {
		t.Errorf("Info.Complete = false; want true (offset=%d length=%d)", info.Offset, info.Length)
	}

	rc, err := s.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "hello world" {
		t.Errorf("body = %q; want %q", string(got), "hello world")
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := s.Info(ctx, id); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("Info after Delete = %v; want ErrUploadNotFound", err)
	}
}

// TestFilesystemUploadStore_ChunkedAppend pins the tus-shaped flow:
// multiple Append calls with growing offsets, isFinal only on the
// last; Open is forbidden until isFinal lands.
func TestFilesystemUploadStore_ChunkedAppend(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	id, err := s.Create(ctx, UploadMeta{Length: 10})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if off, err := s.Append(ctx, id, 0, bytes.NewReader([]byte("hello")), false); err != nil || off != 5 {
		t.Fatalf("Append chunk 1: off=%d err=%v", off, err)
	}
	if _, err := s.Open(ctx, id); !errors.Is(err, ErrUploadIncomplete) {
		t.Errorf("Open mid-upload = %v; want ErrUploadIncomplete", err)
	}
	if off, err := s.Append(ctx, id, 5, bytes.NewReader([]byte("world")), false); err != nil || off != 10 {
		t.Fatalf("Append chunk 2: off=%d err=%v", off, err)
	}
	// Reached declared length: store auto-completes even without
	// isFinal — the tus PATCH path relies on this when the final
	// chunk happens to fill the slot.
	info, _ := s.Info(ctx, id)
	if !info.Complete {
		t.Errorf("auto-complete at length: Complete=false (offset=%d length=%d)", info.Offset, info.Length)
	}
}

// TestFilesystemUploadStore_OffsetConflict — wrong offset rejects
// without disturbing the in-flight upload, matching tus.io 409 shape.
func TestFilesystemUploadStore_OffsetConflict(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	id, _ := s.Create(ctx, UploadMeta{Length: 10})
	_, _ = s.Append(ctx, id, 0, bytes.NewReader([]byte("hello")), false)
	if _, err := s.Append(ctx, id, 0, bytes.NewReader([]byte("X")), false); !errors.Is(err, ErrUploadOffsetConflict) {
		t.Errorf("wrong-offset Append = %v; want ErrUploadOffsetConflict", err)
	}
	info, _ := s.Info(ctx, id)
	if info.Offset != 5 {
		t.Errorf("offset after conflict = %d; want 5 (unchanged)", info.Offset)
	}
}

// TestFilesystemUploadStore_LengthExceeded — overshooting the declared
// length is rejected and the body is trimmed back to the declared cap.
func TestFilesystemUploadStore_LengthExceeded(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	id, _ := s.Create(ctx, UploadMeta{Length: 5})
	_, err := s.Append(ctx, id, 0, bytes.NewReader([]byte("hello world")), true)
	if !errors.Is(err, ErrUploadLengthExceeded) {
		t.Fatalf("Append overshoot = %v; want ErrUploadLengthExceeded", err)
	}
	info, _ := s.Info(ctx, id)
	if info.Offset != 5 {
		t.Errorf("offset after overshoot = %d; want 5 (trimmed)", info.Offset)
	}
}

// TestFilesystemUploadStore_DeferredLength — tus creation-defer-length:
// total fixed when isFinal=true Append lands.
func TestFilesystemUploadStore_DeferredLength(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	id, _ := s.Create(ctx, UploadMeta{Length: -1})
	off, err := s.Append(ctx, id, 0, bytes.NewReader([]byte("abc")), false)
	if err != nil || off != 3 {
		t.Fatalf("Append deferred: off=%d err=%v", off, err)
	}
	info, _ := s.Info(ctx, id)
	if info.Complete {
		t.Errorf("deferred non-final Append marked Complete prematurely")
	}
	off, err = s.Append(ctx, id, 3, bytes.NewReader([]byte("def")), true)
	if err != nil || off != 6 {
		t.Fatalf("Append final deferred: off=%d err=%v", off, err)
	}
	info, _ = s.Info(ctx, id)
	if !info.Complete || info.Length != 6 {
		t.Errorf("after final: Complete=%v Length=%d; want true, 6", info.Complete, info.Length)
	}
}

// TestFilesystemUploadStore_DeleteIdempotent — delete on unknown id is
// a no-op for crash recovery / tus client retries.
func TestFilesystemUploadStore_DeleteIdempotent(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if err := s.Delete(context.Background(), "does-not-exist"); err != nil {
		t.Errorf("Delete missing id = %v; want nil", err)
	}
}

// newTestStore builds a store rooted under t.TempDir with a short TTL
// (irrelevant to most tests but lets the eviction goroutine run).
func newTestStore(t *testing.T) *FilesystemUploadStore {
	t.Helper()
	root := filepath.Join(t.TempDir(), "uploads")
	s, err := NewFilesystemUploadStore(root, time.Hour)
	if err != nil {
		t.Fatalf("NewFilesystemUploadStore: %v", err)
	}
	return s
}
