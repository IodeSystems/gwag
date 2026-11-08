// Package srtest is an in-memory Schema Registry for tests: the subset
// of Confluent's REST API the schemaregistry package and `gwag` use.
// Schemas compare as exact strings; there is no compatibility checking
// unless Compatible is set.
//
// Stability: experimental
package srtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Registry is a fake registry.
type Registry struct {
	*httptest.Server

	// Compatible answers POST /compatibility; nil accepts everything.
	Compatible func(subject, version string, s schemaregistry.Schema) (bool, []string)
	// Requests counts requests by "METHOD path".
	Requests map[string]int

	mu       sync.Mutex
	nextID   int
	ids      map[int]schemaregistry.Schema
	subjects map[string][]schemaregistry.Schema // versions in order
}

// New starts a registry. Close it when done.
func New() *Registry {
	r := &Registry{nextID: 1, ids: map[int]schemaregistry.Schema{}, subjects: map[string][]schemaregistry.Schema{}, Requests: map[string]int{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// Add registers s under subject and returns its id and version.
func (r *Registry) Add(subject string, s schemaregistry.Schema) (id, version int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.add(subject, s)
	return v.ID, v.Version
}

func same(a, b schemaregistry.Schema) bool {
	return a.Schema == b.Schema && reflect.DeepEqual(norm(a.References), norm(b.References))
}

func norm(r []schemaregistry.Reference) []schemaregistry.Reference {
	if len(r) == 0 {
		return nil
	}
	return r
}

func (r *Registry) add(subject string, s schemaregistry.Schema) schemaregistry.Schema {
	for _, v := range r.subjects[subject] {
		if same(v, s) {
			return v
		}
	}
	id := 0
	for i, v := range r.ids {
		if same(v, s) {
			id = i
		}
	}
	if id == 0 {
		id = r.nextID
		r.nextID++
		r.ids[id] = schemaregistry.Schema{Schema: s.Schema, SchemaType: "PROTOBUF", References: s.References}
	}
	v := schemaregistry.Schema{Schema: s.Schema, SchemaType: "PROTOBUF", References: s.References,
		Subject: subject, Version: len(r.subjects[subject]) + 1, ID: id}
	r.subjects[subject] = append(r.subjects[subject], v)
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/vnd.schemaregistry.v1+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error_code": code, "message": msg})
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := strings.Split(strings.Trim(req.URL.EscapedPath(), "/"), "/")
	for i, p := range parts {
		parts[i], _ = url.PathUnescape(p)
	}
	r.Requests[req.Method+" /"+strings.Join(parts, "/")]++
	var body schemaregistry.Schema
	if req.Method == http.MethodPost {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error_code": 42201, "message": err.Error()})
			return
		}
	}
	switch {
	case req.Method == http.MethodGet && len(parts) == 3 && parts[0] == "schemas" && parts[1] == "ids":
		id, _ := strconv.Atoi(parts[2])
		s, ok := r.ids[id]
		if !ok {
			notFound(w, 40403, "Schema not found")
			return
		}
		writeJSON(w, 200, s)
	case req.Method == http.MethodGet && len(parts) == 1 && parts[0] == "subjects":
		out := []string{}
		for s := range r.subjects {
			out = append(out, s)
		}
		writeJSON(w, 200, out)
	case req.Method == http.MethodGet && len(parts) == 4 && parts[0] == "subjects" && parts[2] == "versions":
		vs, ok := r.subjects[parts[1]]
		if !ok {
			notFound(w, 40401, "Subject '"+parts[1]+"' not found.")
			return
		}
		n := len(vs)
		if parts[3] != "latest" {
			n, _ = strconv.Atoi(parts[3])
		}
		if n < 1 || n > len(vs) {
			notFound(w, 40402, "Version "+parts[3]+" not found.")
			return
		}
		writeJSON(w, 200, vs[n-1])
	case req.Method == http.MethodPost && len(parts) == 3 && parts[0] == "subjects" && parts[2] == "versions":
		v := r.add(parts[1], body)
		writeJSON(w, 200, map[string]int{"id": v.ID})
	case req.Method == http.MethodPost && len(parts) == 2 && parts[0] == "subjects":
		vs, ok := r.subjects[parts[1]]
		if !ok {
			notFound(w, 40401, "Subject '"+parts[1]+"' not found.")
			return
		}
		for _, v := range vs {
			if same(v, body) {
				writeJSON(w, 200, v)
				return
			}
		}
		notFound(w, 40403, "Schema not found")
	case req.Method == http.MethodGet && len(parts) == 2 && parts[0] == "config":
		writeJSON(w, 200, map[string]string{"compatibilityLevel": "BACKWARD"})
	case req.Method == http.MethodPost && len(parts) == 5 && parts[0] == "compatibility":
		ok, msgs := true, []string{}
		if r.Compatible != nil {
			ok, msgs = r.Compatible(parts[2], parts[4], body)
		}
		writeJSON(w, 200, map[string]any{"is_compatible": ok, "messages": msgs})
	default:
		notFound(w, 404, "no route "+req.Method+" "+req.URL.Path)
	}
}
