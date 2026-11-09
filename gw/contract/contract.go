// Package contract runs golden request/response cases against an
// http.Handler. A case file holds one case or an array of cases:
//
//	{"name": "get user", "method": "GET", "path": "/v1/users/1",
//	 "headers": {"Authorization": "Bearer x"},
//	 "want": {"status": 200, "body": {"id": 1, "name": "ada"}}}
//
// JSON bodies compare ignoring key order; paths listed in "ignore"
// (slash-separated, e.g. "/id" or "/items/0/at"; "*" matches every
// array element) are dropped from both sides first. Non-JSON bodies
// compare as exact text. A GraphQL case is a POST to the GraphQL path
// with {"query": ...} as the body.
//
// Run with -contract.update to rewrite every case's "want" from the
// handler under test. Record fills "want" from a live service over HTTP.
package contract

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("contract.update", false, "rewrite the want of every contract case from the handler under test")

// Case is one request and its expected answer.
type Case struct {
	Name    string            `json:"name"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	// Ignore lists body paths excluded from the comparison.
	Ignore []string `json:"ignore,omitempty"`
	Want   Want     `json:"want"`
}

// Want is the expected answer. Headers are checked only when listed.
type Want struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Load reads every *.json file in dir, sorted by file name. The second
// result names the file of each case.
func Load(dir string) ([]Case, []string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var cases []Case
	var from []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		var cs []Case
		if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
			err = json.Unmarshal(b, &cs)
		} else {
			var c Case
			err = json.Unmarshal(b, &c)
			cs = []Case{c}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, c := range cs {
			if c.Name == "" {
				c.Name = strings.TrimSuffix(filepath.Base(f), ".json")
			}
			cases = append(cases, c)
			from = append(from, f)
		}
	}
	return cases, from, nil
}

// Run replays the cases in dir against h, one subtest per case. With
// -contract.update it rewrites the case files from h instead.
func Run(t *testing.T, h http.Handler, dir string) {
	t.Helper()
	cases, files, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatalf("no *.json cases in %s", dir)
	}
	if *update {
		for i := range cases {
			w, err := send(handlerDoer{h}, "http://contract.test", cases[i])
			if err != nil {
				t.Fatalf("%s: %v", cases[i].Name, err)
			}
			cases[i].Want = w
		}
		if err := writeBack(cases, files); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			w, err := send(handlerDoer{h}, "http://contract.test", c)
			if err != nil {
				t.Fatal(err)
			}
			if diffs := Compare(c, w); len(diffs) > 0 {
				t.Errorf("%s %s differs from the golden:\n  %s", method(c), c.Path, strings.Join(diffs, "\n  "))
			}
		})
	}
}

// Record sends each case to a live service at baseURL and writes it to
// dir/<name>.json with Want set to the answer. The request part of the
// case is kept as given; headers listed in Want.Headers are recorded.
func Record(baseURL, dir string, cases []Case) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, c := range cases {
		w, err := send(http.DefaultClient, baseURL, c)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		c.Want = w
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, fileName(c.Name)+".json"), append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fileName(n string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, n)
}

func writeBack(cases []Case, files []string) error {
	byFile := map[string][]Case{}
	var order []string
	for i, c := range cases {
		if _, ok := byFile[files[i]]; !ok {
			order = append(order, files[i])
		}
		byFile[files[i]] = append(byFile[files[i]], c)
	}
	for _, f := range order {
		var v any = byFile[f]
		if orig, err := os.ReadFile(f); err == nil && !bytes.HasPrefix(bytes.TrimSpace(orig), []byte("[")) {
			v = byFile[f][0]
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(f, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type doer interface {
	Do(*http.Request) (*http.Response, error)
}

type handlerDoer struct{ h http.Handler }

func (d handlerDoer) Do(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func method(c Case) string {
	if c.Method == "" {
		return http.MethodGet
	}
	return strings.ToUpper(c.Method)
}

func send(d doer, base string, c Case) (Want, error) {
	var body io.Reader
	if len(c.Body) > 0 {
		body = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequest(method(c), strings.TrimRight(base, "/")+c.Path, body)
	if err != nil {
		return Want{}, err
	}
	if len(c.Body) > 0 && !hasHeader(c.Headers, "Content-Type") {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.Do(req)
	if err != nil {
		return Want{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Want{}, err
	}
	w := Want{Status: resp.StatusCode}
	if t := bytes.TrimSpace(raw); len(t) > 0 {
		if json.Valid(t) {
			w.Body = json.RawMessage(t)
		} else {
			w.Body, _ = json.Marshal(string(raw))
		}
	}
	if len(c.Want.Headers) > 0 {
		w.Headers = map[string]string{}
		for k := range c.Want.Headers {
			w.Headers[k] = resp.Header.Get(k)
		}
	}
	return w, nil
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// Compare returns the differences between c.Want and the answer, empty
// when they match.
func Compare(c Case, answer Want) []string {
	var out []string
	if c.Want.Status != answer.Status {
		out = append(out, fmt.Sprintf("status: want %d, have %d", c.Want.Status, answer.Status))
	}
	for k, v := range c.Want.Headers {
		if g := answer.Headers[k]; g != v {
			out = append(out, fmt.Sprintf("header %s: want %q, have %q", k, v, g))
		}
	}
	wb, wok := decode(c.Want.Body)
	ab, aok := decode(answer.Body)
	switch {
	case !wok && !aok:
	case wok != aok:
		out = append(out, fmt.Sprintf("body: want %s, have %s", show(c.Want.Body), show(answer.Body)))
	default:
		for _, p := range c.Ignore {
			wb = drop(wb, splitPath(p))
			ab = drop(ab, splitPath(p))
		}
		diffJSON("", wb, ab, &out)
	}
	return out
}

func show(b json.RawMessage) string {
	if len(b) == 0 {
		return "(empty)"
	}
	return string(b)
}

func decode(b json.RawMessage) (any, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return string(b), true
	}
	return v, true
}

func splitPath(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

// drop removes the value at path, copying maps and slices on the way.
func drop(v any, path []string) any {
	if len(path) == 0 || path[0] == "" {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[path[0]]; !ok {
			return v
		}
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = e
		}
		if len(path) == 1 {
			delete(m, path[0])
		} else {
			m[path[0]] = drop(m[path[0]], path[1:])
		}
		return m
	case []any:
		if path[0] == "*" {
			s := make([]any, len(x))
			for j, e := range x {
				s[j] = drop(e, path[1:])
			}
			return s
		}
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i >= len(x) || len(path) == 1 {
			return v
		}
		s := append([]any(nil), x...)
		s[i] = drop(s[i], path[1:])
		return s
	}
	return v
}

func diffJSON(path string, want, have any, out *[]string) {
	p := path
	if p == "" {
		p = "body"
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := have.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: want object, have %s", p, kind(have)))
			return
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !gok:
				*out = append(*out, fmt.Sprintf("%s/%s: missing, want %s", path, k, compact(wv)))
			case !wok:
				*out = append(*out, fmt.Sprintf("%s/%s: unexpected, have %s", path, k, compact(gv)))
			default:
				diffJSON(path+"/"+k, wv, gv, out)
			}
		}
	case []any:
		g, ok := have.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: want array, have %s", p, kind(have)))
			return
		}
		if len(w) != len(g) {
			*out = append(*out, fmt.Sprintf("%s: want %d elements, have %d", p, len(w), len(g)))
		}
		for i := 0; i < len(w) && i < len(g); i++ {
			diffJSON(fmt.Sprintf("%s/%d", path, i), w[i], g[i], out)
		}
	default:
		if !reflect.DeepEqual(want, have) {
			*out = append(*out, fmt.Sprintf("%s: want %s, have %s", p, compact(want), compact(have)))
		}
	}
}

func kind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
