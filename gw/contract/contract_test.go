package contract

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func api() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"ada","id":1,"at":"2026-01-01T00:00:00Z","tags":["a","b"]}`))
	})
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /text", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	return mux
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunPasses(t *testing.T) {
	dir := t.TempDir()
	// Key order differs, "at" is ignored.
	write(t, dir, "a.json", `{"name":"get","path":"/users/1","ignore":["/at"],
		"want":{"status":200,"body":{"id":1,"tags":["a","b"],"name":"ada","at":"x"}}}`)
	write(t, dir, "b.json", `[
		{"name":"create","method":"POST","path":"/users","body":{"n":1},"want":{"status":201,"body":{"ok":true}}},
		{"name":"text","path":"/text","want":{"status":200,"body":"hello"}}]`)
	Run(t, api(), dir)
}

func TestCompareReportsDiffs(t *testing.T) {
	c := Case{Want: Want{Status: 200, Body: []byte(`{"id":1,"name":"ada","tags":["a","b"],"gone":1}`)}}
	got := Want{Status: 404, Body: []byte(`{"id":2,"name":"ada","tags":["a"],"new":true}`)}
	diffs := strings.Join(Compare(c, got), "\n")
	for _, w := range []string{
		"status: want 200, have 404",
		"/id: want 1, have 2",
		"/gone: missing, want 1",
		"/new: unexpected, have true",
		"/tags: want 2 elements, have 1",
	} {
		if !strings.Contains(diffs, w) {
			t.Errorf("missing %q in:\n%s", w, diffs)
		}
	}
	if len(Compare(c, Want{Status: 200, Body: c.Want.Body})) != 0 {
		t.Error("equal bodies differ")
	}
}

func TestRecordThenRun(t *testing.T) {
	srv := httptest.NewServer(api())
	defer srv.Close()
	dir := t.TempDir()
	err := Record(srv.URL, dir, []Case{
		{Name: "get user", Path: "/users/1", Ignore: []string{"/at"}},
		{Name: "create", Method: "POST", Path: "/users", Body: []byte(`{"n":1}`)},
		{Name: "ct", Path: "/users/1", Want: Want{Headers: map[string]string{"Content-Type": ""}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "get_user.json"))
	if !strings.Contains(string(b), `"status": 200`) {
		t.Fatalf("recorded: %s", b)
	}
	// Recorded goldens replay clean in process.
	Run(t, api(), dir)

	// A changed handler fails the same cases.
	changed := http.NewServeMux()
	changed.HandleFunc("GET /users/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"full_name":"ada","tags":["a","b"]}`))
	})
	cases, _, _ := Load(dir)
	for _, c := range cases {
		if c.Name != "get user" {
			continue
		}
		w, _ := send(handlerDoer{changed}, "http://x", c)
		if len(Compare(c, w)) == 0 {
			t.Fatal("changed handler passed")
		}
		return
	}
	t.Fatal("get user not recorded")
}
