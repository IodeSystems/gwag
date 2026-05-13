package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestTusHandler_OptionsAdvertisesCapabilities — clients probe OPTIONS
// before POST to learn Tus-Max-Size and the extension set. The handler
// must answer with 204 + Tus-Version + Tus-Extension regardless of
// path remainder.
func TestTusHandler_OptionsAdvertisesCapabilities(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()

	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.StatusCode)
	}
	if resp.Header.Get("Tus-Resumable") != tusVersion {
		t.Errorf("Tus-Resumable = %q; want %s", resp.Header.Get("Tus-Resumable"), tusVersion)
	}
	if resp.Header.Get("Tus-Version") != tusVersion {
		t.Errorf("Tus-Version missing")
	}
	if ext := resp.Header.Get("Tus-Extension"); !strings.Contains(ext, "creation") {
		t.Errorf("Tus-Extension = %q; missing `creation`", ext)
	}
}

// TestTusHandler_CreateThenAppend covers the canonical happy path: a
// client POSTs with Upload-Length, gets back 201 + Location, then
// PATCHes the body in two chunks, then HEADs to confirm completion.
func TestTusHandler_CreateThenAppend(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()

	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	// POST: create.
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", "11")
	req.Header.Set("Upload-Metadata", "filename "+b64("greeting.txt")+",content-type "+b64("text/plain"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; want 201", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("POST missing Location header")
	}
	uploadURL := srv.URL + loc

	// PATCH: first chunk.
	req, _ = http.NewRequest(http.MethodPatch, uploadURL, bytes.NewReader([]byte("hello ")))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", tusContentType)
	req.Header.Set("Upload-Offset", "0")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH 1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH 1 status = %d; want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "6" {
		t.Errorf("Upload-Offset after chunk 1 = %q; want 6", got)
	}

	// PATCH: second chunk completes the upload.
	req, _ = http.NewRequest(http.MethodPatch, uploadURL, bytes.NewReader([]byte("world")))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", tusContentType)
	req.Header.Set("Upload-Offset", "6")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH 2: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH 2 status = %d; want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "11" {
		t.Errorf("Upload-Offset after chunk 2 = %q; want 11", got)
	}

	// HEAD: verify completed.
	req, _ = http.NewRequest(http.MethodHead, uploadURL, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d; want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "11" {
		t.Errorf("HEAD Upload-Offset = %q; want 11", got)
	}
	if got := resp.Header.Get("Upload-Length"); got != "11" {
		t.Errorf("HEAD Upload-Length = %q; want 11", got)
	}

	// Open via store to verify the body landed verbatim — the
	// HTTP surface intentionally exposes no GET for the assembled
	// body, but the resolver path uses uploadFromStore.
	id := strings.TrimPrefix(loc, "/")
	up, err := uploadFromStore(context.Background(), gw.cfg.uploadStore, id)
	if err != nil {
		t.Fatalf("uploadFromStore: %v", err)
	}
	got, _ := io.ReadAll(up.File)
	_ = up.File.Close()
	if string(got) != "hello world" {
		t.Errorf("assembled body = %q; want %q", string(got), "hello world")
	}
	if up.Filename != "greeting.txt" {
		t.Errorf("filename = %q; want greeting.txt", up.Filename)
	}
	if up.ContentType != "text/plain" {
		t.Errorf("content-type = %q; want text/plain", up.ContentType)
	}
}

// TestTusHandler_CreateRequiresLengthOrDefer — POST without either
// header is a client error per the spec.
func TestTusHandler_CreateRequiresLengthOrDefer(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

// TestTusHandler_VersionMismatch — Tus-Resumable header with the wrong
// version returns 412 so old clients can't unknowingly hit a future
// server that changed semantics.
func TestTusHandler_VersionMismatch(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set("Tus-Resumable", "0.2.1")
	req.Header.Set("Upload-Length", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d; want 412", resp.StatusCode)
	}
}

// TestTusHandler_PatchWrongContentType — 415 keeps middleboxes from
// reinterpreting raw bytes.
func TestTusHandler_PatchWrongContentType(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	loc := tusCreate(t, srv.URL, 5)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+loc, bytes.NewReader([]byte("hello")))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Upload-Offset", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d; want 415", resp.StatusCode)
	}
}

// TestTusHandler_OffsetMismatch — wrong Upload-Offset is 409 + the
// server's actual offset in the response so the client can resync.
func TestTusHandler_OffsetMismatch(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	loc := tusCreate(t, srv.URL, 10)
	tusPatch(t, srv.URL+loc, 0, "hello")

	// Replay offset 0 — should fail with 409, response reports 5.
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+loc, bytes.NewReader([]byte("HELLO")))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", tusContentType)
	req.Header.Set("Upload-Offset", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d; want 409", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "5" {
		t.Errorf("Upload-Offset on 409 = %q; want 5", got)
	}
}

// TestTusHandler_NoStoreReturns503 — surfacing the misconfiguration as
// 503 (vs 500) keeps tus client libs honest about retries.
func TestTusHandler_NoStoreReturns503(t *testing.T) {
	gw := New() // no upload store option
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

// TestTusHandler_Delete — clients should be able to abandon uploads.
func TestTusHandler_Delete(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	loc := tusCreate(t, srv.URL, 10)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+loc, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d; want 204", resp.StatusCode)
	}

	// HEAD now → 404.
	req, _ = http.NewRequest(http.MethodHead, srv.URL+loc, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD after DELETE status = %d; want 404", resp2.StatusCode)
	}
}

// TestTusHandler_LengthExceeded — client overshooting declared length
// gets 413 with the trimmed offset.
func TestTusHandler_LengthExceeded(t *testing.T) {
	gw := newTusTestGateway(t)
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	loc := tusCreate(t, srv.URL, 5)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+loc, bytes.NewReader([]byte("hello world")))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", tusContentType)
	req.Header.Set("Upload-Offset", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d; want 413", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "5" {
		t.Errorf("Upload-Offset on 413 = %q; want 5", got)
	}
}

// TestTusHandler_TusMaxSizeAdvertisedWhenLimitSet — OPTIONS exposes
// the configured WithUploadLimit so well-behaved clients refuse
// oversized POSTs locally.
func TestTusHandler_TusMaxSizeAdvertisedWhenLimitSet(t *testing.T) {
	dir := t.TempDir()
	gw := New(WithUploadDataDir(dir), WithUploadLimit(1<<20))
	defer gw.Close()
	srv := httptest.NewServer(gw.UploadsTusHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Tus-Max-Size"); got != "1048576" {
		t.Errorf("Tus-Max-Size = %q; want 1048576", got)
	}
}

// --- helpers ---------------------------------------------------------

func newTusTestGateway(t *testing.T) *Gateway {
	t.Helper()
	return New(WithUploadDataDir(t.TempDir()))
}

func tusCreate(t *testing.T, base string, length int64) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base, nil)
	req.Header.Set("Tus-Resumable", tusVersion)
	if length >= 0 {
		req.Header.Set("Upload-Length", fmtInt(length))
	} else {
		req.Header.Set("Upload-Defer-Length", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tusCreate: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tusCreate status = %d; want 201", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

func tusPatch(t *testing.T, url string, offset int64, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", tusContentType)
	req.Header.Set("Upload-Offset", fmtInt(offset))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tusPatch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("tusPatch status = %d; want 204", resp.StatusCode)
	}
}

func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }

// b64 wraps the inline base64 encoding so test bodies stay legible.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
