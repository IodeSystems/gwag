package gateway

import (
	"encoding/json"
	"strings"
)

// Per-field validation errors from an OpenAPI upstream. A 4xx body in
// the huma / RFC 9457 problem shape with an errors list,
//
//	{"errors": [{"location": "body.items[1].name", "message": "..."}]}
//
// becomes GraphQL extensions.fields = [{path, message}], the shape gat
// gives its own huma ops (gat/huma_input.go), so a client maps both
// the same way. The location is rendered in GraphQL argument terms:
// the request body is the "body" arg (ir.IngestOpenAPI), query, path,
// header and cookie parameters are args named after the parameter
// (their location prefix drops), and list indexes become dot
// segments: "body.items.1.name". A body without such a list adds
// nothing.

type upstreamFieldError struct {
	Path    string
	Message string
}

func upstreamFieldErrors(body []byte) []upstreamFieldError {
	var problem struct {
		Errors []struct {
			Location string `json:"location"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &problem) != nil {
		return nil
	}
	var out []upstreamFieldError
	for _, e := range problem.Errors {
		if e.Location == "" || e.Message == "" {
			continue
		}
		out = append(out, upstreamFieldError{Path: upstreamArgPath(e.Location), Message: e.Message})
	}
	return out
}

func upstreamArgPath(loc string) string {
	head, rest, ok := strings.Cut(loc, ".")
	switch head {
	case "query", "path", "header", "cookie":
		if ok {
			loc = rest
		}
	}
	return strings.NewReplacer("[", ".", "]", "").Replace(loc)
}

// fieldsExtension renders r.fields for GraphQL extensions; nil when
// there are none.
func (r *rejection) fieldsExtension() []any {
	if len(r.fields) == 0 {
		return nil
	}
	fs := make([]any, len(r.fields))
	for i, f := range r.fields {
		fs[i] = map[string]any{"path": f.Path, "message": f.Message}
	}
	return fs
}
