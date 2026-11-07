package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aav1 "github.com/iodesystems/gwag/v2/gw/proto/adminauth/v1"
)

// reviewedBy / updatedBy come from the authenticated caller, not the
// request body.

func adminDoBearer(t *testing.T, g *Gateway, bearer, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := httptest.NewRecorder()
	newAdminRouter(t, g).ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestExternalIdentity_TokenRoles(t *testing.T) {
	sink := newHookSink(t, nil)
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))

	code, body := adminDoBearer(t, g, g.DevTokenHex(), http.MethodPut, "/admin/external-dependents/acme", acmeV1)
	if code != http.StatusOK || !strings.Contains(body, `"updatedBy":"dev"`) {
		t.Fatalf("dev put: %d %s", code, body)
	}
	// A client-supplied "by" is not part of the body any more.
	code, body = adminDoBearer(t, g, "tok", http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2028-01-01T00:00:00Z","by":"mallory"}`)
	if code == http.StatusOK && strings.Contains(body, "mallory") {
		t.Fatalf("client-supplied by recorded: %s", body)
	}
	code, body = adminDoBearer(t, g, "tok", http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2028-01-01T00:00:00Z"}`)
	if code != http.StatusOK || !strings.Contains(body, `"reviewedBy":"admin"`) || !strings.Contains(body, `"updatedBy":"admin"`) {
		t.Fatalf("admin review: %d %s", code, body)
	}
	if code, body := adminDoBearer(t, g, g.DevTokenHex(), http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatalf("dev delete: %d %s", code, body)
	}
	reqs := sink.waitN(t, 3)
	want := []string{"dev", "admin", "dev"}
	for i, r := range reqs {
		var d deltaBody
		if err := json.Unmarshal(r.body, &d); err != nil {
			t.Fatal(err)
		}
		if d.By != want[i] {
			t.Errorf("delta %d (%s): by=%q, want %q", i, d.Op, d.By, want[i])
		}
	}
}

func TestExternalIdentity_DelegateSubject(t *testing.T) {
	g := newExternalGateway(t)
	withDelegate(t, g, &fakeAdminAuthConn{
		handler: func(in *aav1.AuthorizeRequest) (*aav1.AuthorizeResponse, error) {
			switch in.Token {
			case "alice":
				return &aav1.AuthorizeResponse{Code: aav1.AdminAuthCode_ADMIN_AUTH_CODE_OK, Role: aav1.AdminRole_ADMIN_ROLE_DEV, Subject: " alice@example.com\n"}, nil
			case "bob":
				return &aav1.AuthorizeResponse{Code: aav1.AdminAuthCode_ADMIN_AUTH_CODE_OK}, nil // no role, no subject
			}
			return &aav1.AuthorizeResponse{Code: aav1.AdminAuthCode_ADMIN_AUTH_CODE_DENIED}, nil
		},
	})
	code, body := adminDoBearer(t, g, "alice", http.MethodPut, "/admin/external-dependents/acme", acmeV1)
	if code != http.StatusOK || !strings.Contains(body, `"updatedBy":"dev:alice@example.com"`) {
		t.Fatalf("alice put: %d %s", code, body)
	}
	code, body = adminDoBearer(t, g, "bob", http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2028-01-01T00:00:00Z"}`)
	if code != http.StatusOK || !strings.Contains(body, `"reviewedBy":"admin"`) {
		t.Fatalf("bob review: %d %s", code, body)
	}
	code, body = adminDoBearer(t, g, "alice", http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2028-01-01T00:00:00Z"}`)
	if code != http.StatusOK || !strings.Contains(body, `"reviewedBy":"dev:alice@example.com"`) {
		t.Fatalf("alice review: %d %s", code, body)
	}
}

func TestSanitizeSubject(t *testing.T) {
	if got := sanitizeSubject("  a\x00b\tc \n"); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeSubject(strings.Repeat("x", 500)); len(got) != maxSubjectLen {
		t.Errorf("len=%d", len(got))
	}
}
