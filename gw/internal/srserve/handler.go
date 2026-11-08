package srserve

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// ContentType is the registry's media type.
const ContentType = "application/vnd.schemaregistry.v1+json"

// maxBody bounds a request body (a schema, or an import bundle).
const maxBody = 64 << 20

// Role of a caller, as the gateway resolves it.
type Role int

const (
	RoleAnonymous Role = iota
	// RoleApp is an app key: it may add and delete the subjects
	// Caller.Owns accepts.
	RoleApp
	// RoleDev may add and delete any subject.
	RoleDev
	// RoleAdmin may also set compatibility levels and import.
	RoleAdmin
)

// Caller is who sent a request.
type Caller struct {
	Role Role
	// Owns reports whether a RoleApp caller may write subject. nil:
	// none.
	Owns func(subject string) bool
}

// HandlerOptions configures NewHandler.
type HandlerOptions struct {
	// Writes allows registration, deletes and config writes (mode
	// READWRITE). Without it the registry is READONLY.
	Writes bool
	// Caller resolves the caller. nil: everyone is anonymous.
	Caller func(r *http.Request) Caller
}

// NewHandler serves the REST API at its root (mount it with
// http.StripPrefix). store returns nil until the store is ready
// (cluster mode waits for JetStream); requests then get 50003.
func NewHandler(store func() *Store, o HandlerOptions) http.Handler {
	return &handler{store: store, o: o}
}

type handler struct {
	store func() *Store
	o     HandlerOptions
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = errf(codeStore, "%v", err)
	}
	if e.Code == codeUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="schema-registry"`)
	}
	writeJSON(w, e.Status(), map[string]any{"error_code": e.Code, "message": e.Message})
}

// segments splits the escaped path into unescaped segments, so a
// subject may hold "/" as %2F.
func segments(r *http.Request) ([]string, error) {
	p := strings.Trim(r.URL.EscapedPath(), "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, err
		}
		parts[i] = u
	}
	return parts, nil
}

func boolQ(r *http.Request, name string) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return v
}

// parseVersion: "latest" and -1 are -1.
func parseVersion(s string) (int, error) {
	if s == "latest" || s == "-1" {
		return -1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, errf(codeInvalidVersion, "The specified version '%s' is not a valid version id. Allowed values are between [1, 2^31-1] and the string \"latest\"", s)
	}
	return n, nil
}

func readSchema(r *http.Request) (sr.Schema, error) {
	var in sr.Schema
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return in, errf(codeInvalidSchema, "read body: %v", err)
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, errf(codeInvalidSchema, "Invalid schema: %v", err)
	}
	return in, nil
}

// schemaBody is a schema in the format the request asks for.
func schemaBody(rec *schemaRec, format string) string {
	if format == "serialized" && rec.Serialized != "" {
		return rec.Serialized
	}
	return rec.Schema
}

func typeField(rec *schemaRec) string {
	if rec.Type == TypeAvro {
		return "" // Confluent leaves it out for AVRO
	}
	return rec.Type
}

type versionOut struct {
	Subject    string         `json:"subject"`
	Version    int            `json:"version"`
	ID         int            `json:"id"`
	SchemaType string         `json:"schemaType,omitempty"`
	References []sr.Reference `json:"references,omitempty"`
	Schema     string         `json:"schema"`
}

func (h *handler) caller(r *http.Request) Caller {
	if h.o.Caller == nil {
		return Caller{}
	}
	return h.o.Caller(r)
}

func (h *handler) writable() error {
	if !h.o.Writes {
		return errf(codeOperationNotPermitted, "Subject is in read-only mode (gwag serves the registry READONLY; writes need the schema registry writes option)")
	}
	return nil
}

// authorizeWrite applies the mode and the admin role to a config or
// mode write.
func (h *handler) authorizeWrite(r *http.Request) error {
	if err := h.writable(); err != nil {
		return err
	}
	return h.authorizeAdmin(r)
}

// authorizeSubject applies the mode and the role to a write of
// subject (register, delete): dev and admin may write any subject, an
// app key the subjects it owns.
func (h *handler) authorizeSubject(r *http.Request, subject string) error {
	if err := h.writable(); err != nil {
		return err
	}
	c := h.caller(r)
	switch c.Role {
	case RoleAdmin, RoleDev:
		return nil
	case RoleApp:
		if c.Owns != nil && c.Owns(subject) {
			return nil
		}
		return errf(codeForbidden, "User is denied operation on subject '%s': the app key's namespaces own no channel with this subject (other subjects need the gwag dev or admin token)", subject)
	}
	return errf(codeUnauthorized, "Unauthorized: send a gwag token (admin, dev or app key) as the basic-auth password or a bearer token")
}

func (h *handler) authorizeAdmin(r *http.Request) error {
	switch h.caller(r).Role {
	case RoleAdmin:
		return nil
	case RoleAnonymous:
		return errf(codeUnauthorized, "Unauthorized: send the gwag admin token as the basic-auth password or a bearer token")
	}
	return errf(codeForbidden, "User is denied operation: this needs the gwag admin token")
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := h.store()
	if st == nil {
		writeErr(w, errf(codeNotReady, "schema registry store is not ready"))
		return
	}
	seg, err := segments(r)
	if err != nil {
		writeErr(w, errf(40400, "bad path: %v", err))
		return
	}
	if err := h.route(w, r, st, seg); err != nil {
		writeErr(w, err)
	}
}

var errNoRoute = errf(40400, "HTTP 404 Not Found")

func methodNotAllowed() error { return &Error{Code: 40500, Message: "HTTP 405 Method Not Allowed"} }

func (h *handler) route(w http.ResponseWriter, r *http.Request, st *Store, seg []string) error {
	ctx := r.Context()
	q := r.URL.Query()
	n := len(seg)
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	if n == 0 {
		if !get {
			return methodNotAllowed()
		}
		writeJSON(w, 200, map[string]any{})
		return nil
	}
	switch seg[0] {
	case "schemas":
		if !get {
			return methodNotAllowed()
		}
		if n == 2 && seg[1] == "types" {
			writeJSON(w, 200, []string{TypeJSON, TypeProtobuf, TypeAvro})
			return nil
		}
		if n < 3 || seg[1] != "ids" {
			return errNoRoute
		}
		id, err := strconv.Atoi(seg[2])
		if err != nil {
			return errf(codeSchemaNotFound, "Schema %s not found", seg[2])
		}
		rec, err := st.SchemaByID(ctx, id)
		if err != nil {
			return err
		}
		switch {
		case n == 3:
			writeJSON(w, 200, struct {
				SchemaType string         `json:"schemaType,omitempty"`
				Schema     string         `json:"schema"`
				References []sr.Reference `json:"references,omitempty"`
			}{typeField(rec), schemaBody(rec, q.Get("format")), rec.References})
		case n == 4 && seg[3] == "schema":
			w.Header().Set("Content-Type", ContentType)
			_, _ = io.WriteString(w, schemaBody(rec, q.Get("format")))
		case n == 4 && seg[3] == "versions":
			vs, err := st.IDVersions(ctx, id, boolQ(r, "deleted"))
			if err != nil {
				return err
			}
			writeJSON(w, 200, vs)
		case n == 4 && seg[3] == "subjects":
			vs, err := st.IDVersions(ctx, id, boolQ(r, "deleted"))
			if err != nil {
				return err
			}
			out := []string{}
			for _, v := range vs {
				if !containsStr(out, v.Subject) {
					out = append(out, v.Subject)
				}
			}
			writeJSON(w, 200, out)
		default:
			return errNoRoute
		}
		return nil
	case "subjects":
		return h.subjects(w, r, st, seg[1:])
	case "config":
		subject := ""
		if n == 2 {
			subject = seg[1]
		} else if n > 2 {
			return errNoRoute
		}
		switch r.Method {
		case http.MethodGet:
			l, ok, err := st.Config(ctx, subject)
			if err != nil {
				return err
			}
			if !ok && boolQ(r, "defaultToGlobal") {
				l, _, err = st.Config(ctx, "")
				ok = err == nil
			}
			if err != nil {
				return err
			}
			if !ok {
				return errf(codeSubjectLevelNoConfig, "Subject '%s' does not have subject-level compatibility configured", subject)
			}
			writeJSON(w, 200, map[string]string{"compatibilityLevel": l})
			return nil
		case http.MethodPut:
			if err := h.authorizeWrite(r); err != nil {
				return err
			}
			var in struct {
				Compatibility string `json:"compatibility"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
				return errf(codeInvalidCompatibility, "Invalid compatibility level: %v", err)
			}
			if err := st.SetConfig(ctx, subject, in.Compatibility); err != nil {
				return err
			}
			writeJSON(w, 200, map[string]string{"compatibility": strings.ToUpper(in.Compatibility)})
			return nil
		case http.MethodDelete:
			if err := h.authorizeWrite(r); err != nil {
				return err
			}
			return methodNotAllowed()
		}
		return methodNotAllowed()
	case "mode":
		if n > 2 {
			return errNoRoute
		}
		if !get {
			if err := h.authorizeWrite(r); err != nil {
				return err
			}
			return errf(codeOperationNotPermitted, "gwag does not change the mode over the API")
		}
		mode := "READONLY"
		if h.o.Writes {
			mode = "READWRITE"
		}
		writeJSON(w, 200, map[string]string{"mode": mode})
		return nil
	case "compatibility":
		if r.Method != http.MethodPost {
			return methodNotAllowed()
		}
		if n < 4 || seg[1] != "subjects" || seg[3] != "versions" || n > 5 {
			return errNoRoute
		}
		version := 0
		if n == 5 {
			v, err := parseVersion(seg[4])
			if err != nil {
				return err
			}
			version = v
		}
		in, err := readSchema(r)
		if err != nil {
			return err
		}
		ok, msgs, err := st.Compatible(ctx, seg[2], version, in)
		if err != nil {
			return err
		}
		out := map[string]any{"is_compatible": ok}
		if boolQ(r, "verbose") {
			if msgs == nil {
				msgs = []string{}
			}
			out["messages"] = msgs
		}
		writeJSON(w, 200, out)
		return nil
	case "contexts":
		if !get || n != 1 {
			return errNoRoute
		}
		writeJSON(w, 200, []string{"."})
		return nil
	case "gwag":
		if n == 2 && seg[1] == "import" {
			if r.Method != http.MethodPost {
				return methodNotAllowed()
			}
			if err := h.authorizeAdmin(r); err != nil {
				return err
			}
			var b Bundle
			if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&b); err != nil {
				return errf(codeInvalidSchema, "import bundle: %v", err)
			}
			res, err := st.Import(ctx, b)
			if err != nil {
				return err
			}
			writeJSON(w, 200, res)
			return nil
		}
	}
	return errNoRoute
}

func (h *handler) subjects(w http.ResponseWriter, r *http.Request, st *Store, seg []string) error {
	ctx := r.Context()
	q := r.URL.Query()
	deleted := boolQ(r, "deleted")
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch len(seg) {
	case 0:
		if !get {
			return methodNotAllowed()
		}
		subs, err := st.Subjects(ctx, deleted)
		if err != nil {
			return err
		}
		if p := q.Get("subjectPrefix"); p != "" && p != ":*:" {
			out := []string{}
			for _, s := range subs {
				if strings.HasPrefix(s, p) {
					out = append(out, s)
				}
			}
			subs = out
		}
		writeJSON(w, 200, subs)
		return nil
	case 1:
		switch r.Method {
		case http.MethodPost:
			in, err := readSchema(r)
			if err != nil {
				return err
			}
			v, rec, err := st.Lookup(ctx, seg[0], in, deleted)
			if err != nil {
				return err
			}
			writeJSON(w, 200, versionOut{seg[0], v.Version, v.ID, typeField(rec), rec.References, rec.Schema})
			return nil
		case http.MethodDelete:
			if err := h.authorizeSubject(r, seg[0]); err != nil {
				return err
			}
			vs, err := st.DeleteSubject(ctx, seg[0], boolQ(r, "permanent"))
			if err != nil {
				return err
			}
			writeJSON(w, 200, vs)
			return nil
		}
		return methodNotAllowed()
	}
	subject := seg[0]
	if seg[1] != "versions" {
		return errNoRoute
	}
	if len(seg) == 2 {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			vs, err := st.Versions(ctx, subject, deleted)
			if err != nil {
				return err
			}
			writeJSON(w, 200, vs)
			return nil
		case http.MethodPost:
			if err := h.authorizeSubject(r, subject); err != nil {
				return err
			}
			in, err := readSchema(r)
			if err != nil {
				return err
			}
			v, err := st.Register(ctx, subject, in)
			if err != nil {
				return err
			}
			writeJSON(w, 200, map[string]int{"id": v.ID})
			return nil
		}
		return methodNotAllowed()
	}
	version, err := parseVersion(seg[2])
	if err != nil {
		return err
	}
	if r.Method == http.MethodDelete {
		if len(seg) != 3 {
			return methodNotAllowed()
		}
		if err := h.authorizeSubject(r, subject); err != nil {
			return err
		}
		v, err := st.DeleteVersion(ctx, subject, version, boolQ(r, "permanent"))
		if err != nil {
			return err
		}
		writeJSON(w, 200, v)
		return nil
	}
	if !get {
		return methodNotAllowed()
	}
	switch {
	case len(seg) == 3:
		v, rec, err := st.Version(ctx, subject, version, deleted)
		if err != nil {
			return err
		}
		writeJSON(w, 200, versionOut{subject, v.Version, v.ID, typeField(rec), rec.References, schemaBody(rec, q.Get("format"))})
	case len(seg) == 4 && seg[3] == "schema":
		_, rec, err := st.Version(ctx, subject, version, deleted)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", ContentType)
		_, _ = io.WriteString(w, schemaBody(rec, q.Get("format")))
	case len(seg) == 4 && seg[3] == "referencedby":
		ids, err := st.ReferencedBy(ctx, subject, version)
		if err != nil {
			return err
		}
		writeJSON(w, 200, ids)
	default:
		return errNoRoute
	}
	return nil
}
