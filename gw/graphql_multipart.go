package gateway

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// graphqlMultipartMaxMemory caps the in-memory portion of the
// multipart parse; parts larger than this overflow to a tempfile
// (multipart.Reader handles this transparently). The cap is per-part
// metadata, NOT the per-upload size limit — the latter lands with
// WithUploadLimit in a follow-on commit.
const graphqlMultipartMaxMemory = 32 << 20 // 32 MiB

// parseGraphqlMultipart implements the graphql-multipart-request-spec
// (https://github.com/jaydenseric/graphql-multipart-request-spec) on
// an inbound multipart/form-data POST.
//
// Wire shape:
//   - operations: JSON-encoded { query, variables, operationName }
//     with `null` placeholders where files go.
//   - map: JSON-encoded mapping of file-part name → []string variable
//     paths, e.g. {"0": ["variables.file"], "1": ["variables.files.0"]}.
//   - file parts: one form file part per entry in `map`, keyed by the
//     map's outer key ("0", "1", …).
//
// Batched operations (operations as a JSON array) and map paths
// prefixed with the batch index (e.g. "0.variables.file") are
// rejected with an error — batching can land later if an adopter
// pulls on it; supporting only single-op keeps the path readable.
func parseGraphqlMultipart(r *http.Request, boundary string) (*graphqlRequestOptions, error) {
	mr := multipart.NewReader(r.Body, boundary)
	form, err := mr.ReadForm(graphqlMultipartMaxMemory)
	if err != nil {
		return nil, fmt.Errorf("graphql multipart: read form: %w", err)
	}

	opsRaw, ok := form.Value["operations"]
	if !ok || len(opsRaw) == 0 {
		_ = form.RemoveAll()
		return nil, fmt.Errorf("graphql multipart: missing `operations` part")
	}
	mapRaw, ok := form.Value["map"]
	if !ok || len(mapRaw) == 0 {
		_ = form.RemoveAll()
		return nil, fmt.Errorf("graphql multipart: missing `map` part")
	}

	// `operations` may legally be an array (batched ops) per the spec;
	// we reject it explicitly — keeps the substitution code shallow,
	// and a hard error beats silently dropping every file.
	if t := strings.TrimLeft(opsRaw[0], " \t\n\r"); strings.HasPrefix(t, "[") {
		_ = form.RemoveAll()
		return nil, fmt.Errorf("graphql multipart: batched `operations` (JSON array) not supported")
	}

	var opts graphqlRequestOptions
	if err := json.Unmarshal([]byte(opsRaw[0]), &opts); err != nil {
		_ = form.RemoveAll()
		return nil, fmt.Errorf("graphql multipart: parse `operations`: %w", err)
	}

	var paths map[string][]string
	if err := json.Unmarshal([]byte(mapRaw[0]), &paths); err != nil {
		_ = form.RemoveAll()
		return nil, fmt.Errorf("graphql multipart: parse `map`: %w", err)
	}

	if opts.Variables == nil && len(paths) > 0 {
		opts.Variables = map[string]any{}
	}

	// We hand back io.ReadClosers that the underlying multipart.File
	// pool owns; multipart.Form.RemoveAll() would invalidate them.
	// Each Upload's File.Close() releases its tempfile (or no-ops on
	// in-memory parts).
	for fileKey, varPaths := range paths {
		files := form.File[fileKey]
		if len(files) == 0 {
			return nil, fmt.Errorf("graphql multipart: map references file part %q but none present", fileKey)
		}
		fh := files[0]
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("graphql multipart: open file part %q: %w", fileKey, err)
		}
		up := &Upload{
			Filename:    fh.Filename,
			ContentType: fh.Header.Get("Content-Type"),
			Size:        fh.Size,
			File:        f,
		}
		for _, p := range varPaths {
			if err := substituteUpload(opts.Variables, p, up); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("graphql multipart: substitute %q: %w", p, err)
			}
		}
	}
	return &opts, nil
}

// substituteUpload walks a dotted path (e.g. "variables.file" or
// "variables.files.0") into the variables map and writes `up` at the
// leaf. The leading "variables" segment is the spec convention —
// nothing else is supported at this entry. Numeric segments index
// into []any slices; non-numeric segments index into map[string]any.
func substituteUpload(variables map[string]any, path string, up *Upload) error {
	segs := strings.Split(path, ".")
	if len(segs) < 2 || segs[0] != "variables" {
		return fmt.Errorf("path must start with \"variables.\"")
	}
	segs = segs[1:]
	var parent any = variables
	for i, seg := range segs {
		last := i == len(segs)-1
		switch p := parent.(type) {
		case map[string]any:
			if last {
				p[seg] = up
				return nil
			}
			next, ok := p[seg]
			if !ok {
				return fmt.Errorf("variables.%s not present", strings.Join(segs[:i+1], "."))
			}
			parent = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return fmt.Errorf("expected numeric index at variables.%s, got %q", strings.Join(segs[:i+1], "."), seg)
			}
			if idx < 0 || idx >= len(p) {
				return fmt.Errorf("variables.%s index out of range (len=%d)", strings.Join(segs[:i+1], "."), len(p))
			}
			if last {
				p[idx] = up
				return nil
			}
			parent = p[idx]
		default:
			return fmt.Errorf("variables.%s is not a map or list", strings.Join(segs[:i], "."))
		}
	}
	return fmt.Errorf("path %q produced no leaf", path)
}

// drainMultipartUploads closes any *Upload File readers reachable from
// `v`. Called by the GraphQL ingress when execution fails before the
// dispatcher takes ownership, so the multipart tempfiles don't leak.
func drainMultipartUploads(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, vv := range x {
			drainMultipartUploads(vv)
		}
	case []any:
		for _, vv := range x {
			drainMultipartUploads(vv)
		}
	case *Upload:
		if x != nil && x.File != nil {
			_ = x.File.Close()
		}
	}
}

