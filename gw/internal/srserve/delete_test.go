package srserve_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

// srDo sends one request with tok as bearer ("" = none). Returns the
// HTTP status, the Confluent error code (0 on success) and the body.
func srDo(t *testing.T, method, u, tok, body string) (int, int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", srserve.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Code int `json:"error_code"`
	}
	_ = json.Unmarshal(b, &e)
	return resp.StatusCode, e.Code, strings.TrimSpace(string(b))
}

// Confluent's delete semantics: soft first, permanent=true after;
// refused while another schema references the version; dev may
// delete any subject, an app key only the ones it owns.
func TestDeleteSubjectAndVersion(t *testing.T) {
	ctx := context.Background()
	st := srserve.NewStore(srserve.NewMemKV(), 1)
	s := serve(t, st, true)
	base := s.URL + "/sr"
	dev := client(t, base, sr.Config{Token: "dev"})

	// The dev token registers: orders-value references options.proto.
	got, err := dev.EnsureFile(ctx, "orders-value", ordersv1.File_orders_v1_orders_proto, sr.RegisterOptions{Register: true})
	if err != nil {
		t.Fatal(err)
	}
	opts := base + "/subjects/" + url.PathEscape("gw/proto/ps/v1/options.proto")

	type step struct {
		what         string
		method, path string
		tok          string
		status, code int
		body         string
	}
	steps := []step{
		{"referenced version", "DELETE", opts + "/versions/1", "dev", 422, 42206, ""},
		{"referenced subject", "DELETE", opts, "dev", 422, 42206, ""},
		{"permanent before soft", "DELETE", base + "/subjects/orders-value?permanent=true", "dev", 404, 40405, ""},
		{"anonymous", "DELETE", base + "/subjects/orders-value", "", 401, 40101, ""},
		{"app key, not its subject", "DELETE", base + "/subjects/orders-value", "app", 403, 40301, ""},
		{"missing subject", "DELETE", base + "/subjects/nope", "dev", 404, 40401, ""},
		{"soft delete", "DELETE", base + "/subjects/orders-value", "dev", 200, 0, "[1]"},
		{"soft delete again", "DELETE", base + "/subjects/orders-value", "dev", 404, 40404, ""},
		{"soft-deleted readable", "GET", base + "/subjects/orders-value/versions/1?deleted=true", "", 200, 0, ""},
		{"soft-deleted hidden", "GET", base + "/subjects/orders-value/versions/1", "", 404, 40401, ""},
		// A soft-deleted referrer still holds the reference.
		{"still referenced", "DELETE", opts + "/versions/1", "dev", 422, 42206, ""},
		{"permanent", "DELETE", base + "/subjects/orders-value?permanent=true", "dev", 200, 0, "[1]"},
		{"id gone", "GET", base + "/schemas/ids/" + itoa(got.ID), "", 404, 40403, ""},
		{"options now free", "DELETE", opts + "/versions/1", "dev", 200, 0, "1"},
		{"version soft deleted", "DELETE", opts + "/versions/1", "dev", 404, 40406, ""},
		{"version permanent", "DELETE", opts + "/versions/1?permanent=true", "dev", 200, 0, "1"},
		{"subjects empty", "GET", base + "/subjects?deleted=true", "", 200, 0, "[]"},

		// App key on a subject it owns.
		{"app registers v1", "POST", base + "/subjects/app-x/versions", "app", 200, 0, ""},
		{"app registers v2", "POST", base + "/subjects/app-x/versions", "app", 200, 0, ""},
		{"app soft deletes latest", "DELETE", base + "/subjects/app-x/versions/latest", "app", 200, 0, "2"},
		{"app permanent v1 before soft", "DELETE", base + "/subjects/app-x/versions/1?permanent=true", "app", 404, 40407, ""},
		{"app permanent v2", "DELETE", base + "/subjects/app-x/versions/2?permanent=true", "app", 200, 0, "2"},
		{"missing version", "DELETE", base + "/subjects/app-x/versions/2", "app", 404, 40402, ""},
		{"versions left", "GET", base + "/subjects/app-x/versions?deleted=true", "", 200, 0, "[1]"},
	}
	n := 0
	for _, c := range steps {
		body := ""
		if c.method == "POST" {
			n++
			body = `{"schemaType":"JSON","schema":"{\"type\":\"object\",\"title\":\"v` + itoa(n) + `\"}"}`
		}
		status, code, out := srDo(t, c.method, c.path, c.tok, body)
		if status != c.status || code != c.code || (c.body != "" && out != c.body) {
			t.Fatalf("%s: %d %d %s; want %d %d %s", c.what, status, code, out, c.status, c.code, c.body)
		}
	}

	// The next registration takes the number after the highest version
	// left: a permanently deleted number can come back.
	if status, _, out := srDo(t, "POST", base+"/subjects/app-x/versions", "app", `{"schemaType":"JSON","schema":"{\"title\":\"v3\"}"}`); status != 200 {
		t.Fatalf("register after delete: %d %s", status, out)
	}
	if _, _, out := srDo(t, "GET", base+"/subjects/app-x/versions", "", ""); out != "[1,2]" {
		t.Fatalf("versions after re-register: %s", out)
	}
}

func TestDeleteReadOnly(t *testing.T) {
	s := serve(t, srserve.NewStore(srserve.NewMemKV(), 1), false)
	if status, code, _ := srDo(t, "DELETE", s.URL+"/sr/subjects/x", token, ""); status != 422 || code != 42205 {
		t.Fatalf("delete on READONLY: %d %d", status, code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
