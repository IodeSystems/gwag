package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// tusVersion is the tus.io core protocol version this gateway speaks.
// Sent in every response as Tus-Resumable; clients pin to it via the
// Tus-Resumable request header and a mismatch returns 412.
const tusVersion = "1.0.0"

// tusExtensions advertises the optional spec sections this server
// implements. creation = POST endpoint; creation-defer-length = an
// upload can be created without declaring its total length up-front;
// termination = DELETE endpoint. Checksum / expiration / concatenation
// are deferred to a follow-on when an adopter pulls.
const tusExtensions = "creation,creation-defer-length,termination"

// tusContentType is the only Content-Type a PATCH request is allowed
// to carry. The spec is strict about this so middleboxes can't
// reinterpret the chunked body as something other than raw bytes.
const tusContentType = "application/offset+octet-stream"

// UploadsTusHandler returns the http.Handler implementing the tus.io
// v1.0 core protocol over the configured UploadStore. Mount it at
// /api/uploads/tus (the chosen convention); the handler itself parses
// the trailing id from the path remainder.
//
// Requires an UploadStore — call WithUploadStore or WithUploadDataDir.
// Without one, every request returns 503 with the explanatory body so
// the operator sees a clear misconfiguration message instead of a
// silent 404.
//
// The handler is public by design: the upload id returned by POST is
// the credential. IDs are cryptographically random (16 bytes hex) so
// brute-forcing another upload's slot is not feasible. Adopters who
// need bearer-level auth in front can wrap the handler.
//
// Stability: stable
func (g *Gateway) UploadsTusHandler() http.Handler {
	return &tusHandler{
		store: g.cfg.uploadStore,
		limit: g.cfg.uploadLimit,
	}
}

// tusHandler implements the tus core protocol over an UploadStore.
// One per gateway; the store + limit are captured at handler build
// time so per-request work stays lock-free.
type tusHandler struct {
	store UploadStore
	limit int64 // 0 = unlimited
}

func (h *tusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Always advertise resumable + version so middleboxes that strip
	// every other header still leak the protocol to the operator.
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Cache-Control", "no-store")

	if h.store == nil {
		// Surfacing this as 503 (not 500) keeps the tus client libs
		// well-behaved — they retry on 503 but treat 500 as fatal.
		// The text body gives the operator the missing-option hint.
		writeTusError(w, http.StatusServiceUnavailable, "tus: no upload store configured; use WithUploadStore / WithUploadDataDir")
		return
	}

	// OPTIONS is the unauthenticated capability probe; clients call it
	// before POST to discover Tus-Max-Size and the extension set.
	if r.Method == http.MethodOptions {
		h.serveOptions(w)
		return
	}

	// Every non-OPTIONS request must declare the protocol version it
	// speaks; the spec is strict so old clients can't quietly hit a
	// new server that changed semantics.
	if v := r.Header.Get("Tus-Resumable"); v != "" && v != tusVersion {
		writeTusError(w, http.StatusPreconditionFailed, fmt.Sprintf("tus: unsupported version %q (server speaks %s)", v, tusVersion))
		return
	}

	// Routing: POST at the collection URL creates; everything else
	// targets a specific upload-id at the resource URL.
	id := strings.TrimPrefix(r.URL.Path, "/")
	id = strings.TrimPrefix(id, "/")
	// Path remainder may have leading separators if the operator
	// mounted at a prefix that the mux stripped already; tolerate both
	// shapes by collapsing leading slashes.
	id = strings.TrimLeft(id, "/")

	if r.Method == http.MethodPost && id == "" {
		h.serveCreate(w, r)
		return
	}

	if id == "" {
		writeTusError(w, http.StatusBadRequest, "tus: missing upload id in path")
		return
	}

	switch r.Method {
	case http.MethodHead:
		h.serveHead(w, r, id)
	case http.MethodPatch:
		h.servePatch(w, r, id)
	case http.MethodDelete:
		h.serveDelete(w, r, id)
	default:
		w.Header().Set("Allow", "OPTIONS, POST, HEAD, PATCH, DELETE")
		writeTusError(w, http.StatusMethodNotAllowed, fmt.Sprintf("tus: method %s not allowed", r.Method))
	}
}

func (h *tusHandler) serveOptions(w http.ResponseWriter) {
	w.Header().Set("Tus-Version", tusVersion)
	w.Header().Set("Tus-Extension", tusExtensions)
	if h.limit > 0 {
		w.Header().Set("Tus-Max-Size", strconv.FormatInt(h.limit, 10))
	}
	// 204 No Content is the spec-blessed status for an OPTIONS probe.
	w.WriteHeader(http.StatusNoContent)
}

func (h *tusHandler) serveCreate(w http.ResponseWriter, r *http.Request) {
	// Either Upload-Length or Upload-Defer-Length: 1 must be present.
	// Both present → bad request. Neither present → bad request.
	lengthHdr := r.Header.Get("Upload-Length")
	deferred := r.Header.Get("Upload-Defer-Length") == "1"
	var length int64 = -1
	if lengthHdr != "" {
		n, err := strconv.ParseInt(lengthHdr, 10, 64)
		if err != nil || n < 0 {
			writeTusError(w, http.StatusBadRequest, "tus: invalid Upload-Length")
			return
		}
		length = n
	}
	switch {
	case length < 0 && !deferred:
		writeTusError(w, http.StatusBadRequest, "tus: Upload-Length or Upload-Defer-Length: 1 required")
		return
	case length >= 0 && deferred:
		writeTusError(w, http.StatusBadRequest, "tus: Upload-Length and Upload-Defer-Length are mutually exclusive")
		return
	}
	if h.limit > 0 && length > h.limit {
		writeTusError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("tus: Upload-Length %d exceeds Tus-Max-Size %d", length, h.limit))
		return
	}

	meta := UploadMeta{
		Length:   length,
		Metadata: parseTusMetadata(r.Header.Get("Upload-Metadata")),
	}
	// tus puts user-meaningful fields (filename, content-type) into
	// Upload-Metadata; we surface the two well-known keys back as
	// first-class UploadMeta fields so resolvers don't have to grovel
	// through the map.
	if v, ok := meta.Metadata["filename"]; ok {
		meta.Filename = v
	}
	if v, ok := meta.Metadata["content-type"]; ok {
		meta.ContentType = v
	}
	if v, ok := meta.Metadata["filetype"]; ok && meta.ContentType == "" {
		// `filetype` is the tus-js-client default; accept it as an
		// alias so the most common client wires through.
		meta.ContentType = v
	}

	id, err := h.store.Create(r.Context(), meta)
	if err != nil {
		writeTusError(w, http.StatusInternalServerError, fmt.Sprintf("tus: store create: %v", err))
		return
	}
	// Build the Location relative to the request path so the response
	// works behind any mount point / reverse proxy without the gateway
	// having to know its public URL.
	loc := strings.TrimRight(r.URL.Path, "/") + "/" + id
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusCreated)
}

func (h *tusHandler) serveHead(w http.ResponseWriter, r *http.Request, id string) {
	info, err := h.store.Info(r.Context(), id)
	if err != nil {
		writeTusStoreError(w, err)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(info.Offset, 10))
	if info.Length >= 0 {
		w.Header().Set("Upload-Length", strconv.FormatInt(info.Length, 10))
	} else {
		w.Header().Set("Upload-Defer-Length", "1")
	}
	if md := encodeTusMetadata(info.Metadata); md != "" {
		w.Header().Set("Upload-Metadata", md)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *tusHandler) servePatch(w http.ResponseWriter, r *http.Request, id string) {
	if ct := r.Header.Get("Content-Type"); ct != tusContentType {
		writeTusError(w, http.StatusUnsupportedMediaType, fmt.Sprintf("tus: PATCH requires Content-Type: %s", tusContentType))
		return
	}
	offHdr := r.Header.Get("Upload-Offset")
	off, err := strconv.ParseInt(offHdr, 10, 64)
	if err != nil || off < 0 {
		writeTusError(w, http.StatusBadRequest, "tus: invalid Upload-Offset")
		return
	}

	// If the client switches a deferred-length upload to fixed-length
	// via Upload-Length on PATCH, that's spec-compliant; resolve it
	// before delegating to the store so Append's length-cap logic
	// applies on this very PATCH.
	if lh := r.Header.Get("Upload-Length"); lh != "" {
		info, err := h.store.Info(r.Context(), id)
		if err != nil {
			writeTusStoreError(w, err)
			return
		}
		if info.Length < 0 {
			n, perr := strconv.ParseInt(lh, 10, 64)
			if perr != nil || n < 0 {
				writeTusError(w, http.StatusBadRequest, "tus: invalid Upload-Length on PATCH")
				return
			}
			if h.limit > 0 && n > h.limit {
				writeTusError(w, http.StatusRequestEntityTooLarge, "tus: Upload-Length exceeds Tus-Max-Size")
				return
			}
			// Re-create the upload would lose the bytes already written;
			// instead we'd want a SetLength method on the store. Until
			// that lands, reject — clients can still resolve by
			// supplying Upload-Length at POST. (tus-js-client always
			// declares Upload-Length on create, so this path is rare.)
			writeTusError(w, http.StatusNotImplemented, "tus: upload-length finalisation on PATCH not supported; declare Upload-Length at POST")
			_ = info
			return
		}
	}

	// isFinal is true when the request body completes the upload.
	// The spec says clients SHOULD send Upload-Length-Final or, in
	// the absence of it, the server infers final from offset+length
	// reaching the declared total. Our store already auto-completes
	// when offset reaches the declared length, so passing isFinal=false
	// to Append is safe — Info().Complete will flip when the math
	// works out.
	body := io.Reader(r.Body)
	if h.limit > 0 {
		body = io.LimitReader(body, h.limit+1)
	}

	newOff, err := h.store.Append(r.Context(), id, off, body, false)
	// Headers must be set before WriteHeader fires. Upload-Offset is
	// emitted even on 409 / 413 so tus-js-client can resync to the
	// store's actual position rather than restart from zero.
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOff, 10))
	if err != nil {
		writeTusStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *tusHandler) serveDelete(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.store.Delete(r.Context(), id); err != nil {
		writeTusError(w, http.StatusInternalServerError, fmt.Sprintf("tus: store delete: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeTusStoreError maps the store's sentinel errors onto tus's
// response codes. Anything unrecognised is a 500.
func writeTusStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUploadNotFound):
		writeTusError(w, http.StatusNotFound, "tus: upload not found")
	case errors.Is(err, ErrUploadOffsetConflict):
		writeTusError(w, http.StatusConflict, "tus: offset mismatch")
	case errors.Is(err, ErrUploadLengthExceeded):
		writeTusError(w, http.StatusRequestEntityTooLarge, "tus: declared length exceeded")
	case errors.Is(err, ErrUploadIncomplete):
		// Open() on an in-progress upload — Internal-server-error
		// keeps the tus surface honest (HEAD reports the offset; the
		// client should not be opening the body yet).
		writeTusError(w, http.StatusInternalServerError, "tus: upload incomplete")
	default:
		writeTusError(w, http.StatusInternalServerError, fmt.Sprintf("tus: %v", err))
	}
}

func writeTusError(w http.ResponseWriter, status int, msg string) {
	// Body is plain text per the spec example responses; tus client
	// libs surface it back to the application unchanged.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

// parseTusMetadata decodes a tus Upload-Metadata header value into a
// key→value map. Wire shape: `key1 base64val1,key2 base64val2`.
// Values are base64-encoded (RFC 4648); keys are ASCII printable
// except whitespace + comma. Malformed entries are skipped — the spec
// errs on the side of partial parsing.
func parseTusMetadata(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, encV, _ := strings.Cut(pair, " ")
		if k == "" {
			continue
		}
		if encV == "" {
			// Keyless metadata is legal; tus uses bare keys for boolean
			// flags. Store as empty string.
			out[k] = ""
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(encV)
		if err != nil {
			continue
		}
		out[k] = string(dec)
	}
	return out
}

// encodeTusMetadata is the inverse of parseTusMetadata: builds a
// canonical (sorted-key) Upload-Metadata value for HEAD responses.
func encodeTusMetadata(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sort so HEAD responses round-trip deterministically (helps
	// integration tests + caches).
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if v == "" {
			parts = append(parts, k)
			continue
		}
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

// sortStrings is a tiny dependency-free wrapper to avoid importing
// "sort" in this file (the rest of the package already pulls it in,
// but the linter flagged the singular use). Insertion sort is fine
// for the handful of tus metadata keys typical clients send.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// uploadFromStore returns an *Upload backed by the store's reader for
// id, with metadata populated from store.Info. Callers Close() the
// returned File when finished. Returns nil + ErrUploadNotFound /
// ErrUploadIncomplete from the underlying store.
//
// Used by the dual-mode Upload scalar when a GraphQL variable carries
// a tus upload-id instead of an inline multipart file part.
func uploadFromStore(ctx context.Context, store UploadStore, id string) (*Upload, error) {
	info, err := store.Info(ctx, id)
	if err != nil {
		return nil, err
	}
	if !info.Complete {
		return nil, ErrUploadIncomplete
	}
	rc, err := store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Upload{
		Filename:    info.Filename,
		ContentType: info.ContentType,
		Size:        info.Length,
		File:        rc,
	}, nil
}
