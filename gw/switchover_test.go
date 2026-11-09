package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
)

// ---- sticky hashing ------------------------------------------------

func TestSwitchover_StickyAndMonotone(t *testing.T) {
	keys := make([]string, 2000)
	for i := range keys {
		keys[i] = fmt.Sprintf("client-%d", i)
	}
	green50 := 0
	for _, k := range keys {
		b := switchoverBucket(k)
		if b < 0 || b > 99 {
			t.Fatalf("bucket(%q) = %d", k, b)
		}
		for i := 0; i < 3; i++ {
			if switchoverBucket(k) != b {
				t.Fatalf("bucket(%q) not stable", k)
			}
		}
		// Raising the percent never moves a key from green to blue.
		was := false
		for p := 0; p <= 100; p++ {
			now := switchoverToGreen(k, p)
			if was && !now {
				t.Fatalf("key %q on green at %d%%, blue at %d%%", k, p-1, p)
			}
			was = now
		}
		if switchoverToGreen(k, 0) || !switchoverToGreen(k, 100) {
			t.Fatalf("key %q: 0%% must be blue, 100%% green", k)
		}
		if switchoverToGreen(k, 50) {
			green50++
		}
	}
	if green50 < 900 || green50 > 1100 {
		t.Fatalf("50%%: %d of 2000 keys on green, want about 1000", green50)
	}
}

// ---- rules ---------------------------------------------------------

func TestSwitchover_Rules(t *testing.T) {
	h := http.Header{}
	h.Set("X-Canary", "1")
	c, err := buildSwitchoverConfig(switchoverRecord{Percent: 20, Rules: []SwitchoverRule{
		{OperationName: "CreateOrder", Exclude: true},
		{Header: "X-Canary", HeaderValue: "1", Percent: 100},
		{OperationName: "ListOrders"},
	}}, "http://green/graphql", false)
	if err != nil {
		t.Fatal(err)
	}
	if !c.needsOp || !c.active {
		t.Fatalf("needsOp=%v active=%v", c.needsOp, c.active)
	}
	cases := []struct {
		op   string
		h    http.Header
		want int
	}{
		{"CreateOrder", h, 0},             // excluded before the canary rule
		{"Other", h, 100},                 // canary rule
		{"", h, 100},                      // WebSocket upgrade: no op name
		{"ListOrders", http.Header{}, 20}, // rule without percent: the global one
		{"Other", http.Header{}, 0},       // no rule matches: blue
	}
	for _, tc := range cases {
		if got := c.share(tc.op, tc.h); got != tc.want {
			t.Errorf("share(%q, %v) = %d, want %d", tc.op, tc.h, got, tc.want)
		}
	}
	wrong := http.Header{}
	wrong.Set("X-Canary", "2")
	if got := c.share("Other", wrong); got != 0 {
		t.Errorf("wrong header value: share %d", got)
	}

	noRules, _ := buildSwitchoverConfig(switchoverRecord{Percent: 7}, "http://green/graphql", false)
	if noRules.share("Anything", http.Header{}) != 7 {
		t.Errorf("no rules: every request at the global percent")
	}
	if off, _ := buildSwitchoverConfig(switchoverRecord{Percent: 50}, "", false); off.active {
		t.Errorf("no target must not be active")
	}
	if off, _ := buildSwitchoverConfig(switchoverRecord{Percent: 0}, "http://green/graphql", false); off.active {
		t.Errorf("0%% must not be active")
	}

	for _, bad := range []switchoverRecord{
		{Percent: 101},
		{Percent: -1},
		{Rules: []SwitchoverRule{{Percent: 200}}},
		{Rules: []SwitchoverRule{{HeaderValue: "x"}}},
		{Target: "ftp://green/x"},
		{Target: "green:8080"},
	} {
		if _, err := buildSwitchoverConfig(bad, "", true); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestSwitchover_IsMutation(t *testing.T) {
	q := func(query, op string) []byte {
		b, _ := json.Marshal(map[string]any{"query": query, "operationName": op})
		return b
	}
	cases := []struct {
		body []byte
		want bool
	}{
		{q(`{ a }`, ""), false},
		{q(`query Q { a }`, ""), false},
		{q(`subscription S { a }`, ""), false},
		{q(`mutation M { a }`, ""), true},
		{q(`query Q { a } mutation M { a }`, "Q"), false},
		{q(`query Q { a } mutation M { a }`, "M"), true},
		{q(`query Q { a } mutation M { a }`, ""), true}, // cannot tell
		{q(`query Q { a }`, "Nope"), true},
		{[]byte(`{"extensions":{"persistedQuery":{"sha256Hash":"abc"}}}`), true}, // no text
		{q(`{ broken`, ""), true},
	}
	for _, tc := range cases {
		if got := switchoverIsMutation(tc.body); got != tc.want {
			t.Errorf("isMutation(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// ---- fallback per error class --------------------------------------

// switchoverUnit is a blue gateway's switchover handler in front of a
// fake "blue" handler, at 100% to the given green URL.
type switchoverUnit struct {
	g     *Gateway
	h     http.Handler
	blueN atomic.Int32
}

func newSwitchoverUnit(t *testing.T, target string, timeout time.Duration, opts ...Option) *switchoverUnit {
	t.Helper()
	u := &switchoverUnit{}
	u.g = New(append([]Option{WithoutBackpressure(), WithSwitchover(SwitchoverOptions{Target: target, Percent: 100, Timeout: timeout})}, opts...)...)
	t.Cleanup(u.g.Close)
	u.h = u.g.switchoverHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.blueN.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"side":"blue","len":%d}}`, len(b))
	}))
	return u
}

func (u *switchoverUnit) post(query string, header http.Header) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	return rec
}

func (u *switchoverUnit) outcome(name string) int64 { return u.g.switchover.outcomes[name].Load() }
func (u *switchoverUnit) reason(name string) int64  { return u.g.switchover.reasons[name].Load() }

func TestSwitchover_FallbackClasses(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var greenN atomic.Int32
	green := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		greenN.Add(1)
		_, _ = io.ReadAll(r.Body)
		switch r.URL.Query().Get("mode") {
		case "slow":
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "404":
			http.NotFound(w, r)
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>login</html>"))
		case "reset":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		case "graphql-errors":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errors":[{"message":"resolver failed"}],"data":null}`))
		case "400":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"parse error"}]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"side":"green"}}`))
		}
	}))
	t.Cleanup(green.Close)

	const query, mutation = `{ a }`, `mutation { a }`
	cases := []struct {
		name     string
		target   string
		reason   string // "" = green's answer
		mutation string // outcome for a mutation: fallback or error
	}{
		{"connect", closedURL + "/graphql", SwitchoverReasonConnect, SwitchoverOutcomeFallback},
		{"timeout", green.URL + "/graphql?mode=slow", SwitchoverReasonTimeout, SwitchoverOutcomeError},
		{"status_500", green.URL + "/graphql?mode=500", SwitchoverReasonStatus, SwitchoverOutcomeError},
		{"status_404", green.URL + "/graphql?mode=404", SwitchoverReasonStatus, SwitchoverOutcomeError},
		{"bad_response", green.URL + "/graphql?mode=html", SwitchoverReasonBadResponse, SwitchoverOutcomeError},
		{"transport", green.URL + "/graphql?mode=reset", SwitchoverReasonTransport, SwitchoverOutcomeError},
		{"graphql_errors", green.URL + "/graphql?mode=graphql-errors", "", ""},
		{"status_400", green.URL + "/graphql?mode=400", "", ""},
		{"ok", green.URL + "/graphql", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newSwitchoverUnit(t, tc.target, 300*time.Millisecond)
			for _, doc := range []string{query, mutation} {
				before := greenN.Load()
				rec := u.post(doc, nil)
				body := rec.Body.String()
				if tc.reason == "" {
					// Green's answer, passed through: GraphQL errors
					// from green's resolvers are not a failure.
					if u.blueN.Load() != 0 || strings.Contains(body, `"side":"blue"`) {
						t.Fatalf("%s: blue served it: %d %s", doc, rec.Code, body)
					}
					if greenN.Load() != before+1 {
						t.Fatalf("%s: green not called once", doc)
					}
					continue
				}
				want := SwitchoverOutcomeFallback
				if doc == mutation {
					want = tc.mutation
				}
				switch want {
				case SwitchoverOutcomeFallback:
					if !strings.Contains(body, `"side":"blue"`) || rec.Code != http.StatusOK {
						t.Fatalf("%s: want blue's answer, got %d %s", doc, rec.Code, body)
					}
					// The body reaches blue whole.
					if !strings.Contains(body, fmt.Sprintf(`"len":%d`, len(mustJSON(map[string]any{"query": doc})))) {
						t.Fatalf("%s: blue got a different body: %s", doc, body)
					}
				case SwitchoverOutcomeError:
					if rec.Code != http.StatusBadGateway || !strings.Contains(body, `"switchoverReason":"`+tc.reason+`"`) || strings.Contains(body, `"side"`) {
						t.Fatalf("%s: want 502 with the reason, got %d %s", doc, rec.Code, body)
					}
				}
			}
			if tc.reason == "" {
				if u.outcome(SwitchoverOutcomeGreen) != 2 {
					t.Fatalf("green outcomes = %d, want 2", u.outcome(SwitchoverOutcomeGreen))
				}
				return
			}
			if got := u.reason(tc.reason); got != 2 {
				t.Fatalf("reason %s = %d, want 2", tc.reason, got)
			}
			wantFallback, wantErr := int64(2), int64(0)
			if tc.mutation == SwitchoverOutcomeError {
				wantFallback, wantErr = 1, 1
			}
			if u.outcome(SwitchoverOutcomeFallback) != wantFallback || u.outcome(SwitchoverOutcomeError) != wantErr {
				t.Fatalf("fallback=%d error=%d, want %d %d", u.outcome(SwitchoverOutcomeFallback), u.outcome(SwitchoverOutcomeError), wantFallback, wantErr)
			}
			if n := len(u.g.switchover.recentErrors()); n != 2 {
				t.Fatalf("recent errors = %d, want 2", n)
			}
		})
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestSwitchover_StaysOnBlue covers requests switchover does not move:
// marked as already forwarded, a batch, a GET, a body over MaxBytes.
func TestSwitchover_StaysOnBlue(t *testing.T) {
	var greenN atomic.Int32
	green := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		greenN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(green.Close)
	u := newSwitchoverUnit(t, green.URL, time.Second)
	u.g.switchover.opts.MaxBytes = 100

	h := http.Header{}
	h.Set(SwitchoverHeader, "1")
	u.post(`{ a }`, h)

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`[{"query":"{ a }"}]`))
	req.Header.Set("Content-Type", "application/json")
	u.h.ServeHTTP(httptest.NewRecorder(), req)

	u.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/graphql?query={a}", nil))

	big := `{ ` + strings.Repeat("a ", 100) + `}`
	rec := u.post(big, nil)
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"len":%d`, len(mustJSON(map[string]any{"query": big})))) { // whole body reached blue
		t.Fatalf("large body: %s", rec.Body.String())
	}
	if greenN.Load() != 0 || u.blueN.Load() != 4 {
		t.Fatalf("green=%d blue=%d, want 0 and 4", greenN.Load(), u.blueN.Load())
	}
}

// ---- two gateways --------------------------------------------------

type switchoverSide struct {
	gw      *Gateway
	srv     *httptest.Server
	greeter *fakeGreeterServer
	lastHdr atomic.Pointer[http.Header]
	reqs    atomic.Int32
	upgrade atomic.Int32
}

// newSwitchoverSide is a gateway whose greeter answers "<name>: hello
// <x>", served at /graphql. Every request's headers are kept.
func newSwitchoverSide(t *testing.T, name string, opts ...Option) *switchoverSide {
	t.Helper()
	s := &switchoverSide{greeter: &fakeGreeterServer{}}
	s.greeter.helloFn = func(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		return &greeterv1.HelloResponse{Greeting: name + ": hello " + req.GetName()}, nil
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(gs, s.greeter)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	s.gw = New(append([]Option{WithoutBackpressure(), WithCallerIDPublic()}, opts...)...)
	t.Cleanup(s.gw.Close)
	if err := s.gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(lis.Addr().String()), As("greeter")); err != nil {
		t.Fatal(err)
	}
	h := s.gw.Handler()
	mux := http.NewServeMux()
	mux.Handle("/graphql", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := r.Header.Clone()
		s.lastHdr.Store(&hd)
		s.reqs.Add(1)
		if isWebSocketUpgrade(r) {
			s.upgrade.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// hello posts the greeter query as caller and returns the side that
// answered ("blue" or "green").
func switchoverHello(t *testing.T, url, caller string, extra http.Header) string {
	t.Helper()
	body := mustJSON(map[string]any{"query": `{ greeter { hello(name:"x") { greeting } } }`})
	req, _ := http.NewRequest(http.MethodPost, url+"/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if caller != "" {
		req.Header.Set(PublicCallerIDHeader, caller)
	}
	for k, v := range extra {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Greeter struct {
				Hello struct {
					Greeting string `json:"greeting"`
				} `json:"hello"`
			} `json:"greeter"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Data.Greeter.Hello.Greeting == "" {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	side, _, _ := strings.Cut(out.Data.Greeter.Hello.Greeting, ":")
	return side
}

func TestSwitchover_TwoGateways(t *testing.T) {
	green := newSwitchoverSide(t, "green")
	blue := newSwitchoverSide(t, "blue", WithSwitchover(SwitchoverOptions{Target: green.srv.URL + "/graphql", StickyHeader: "X-Client-Id"}))
	s := blue.gw.switchover
	set := func(p int) {
		t.Helper()
		if _, err := s.setRuntime(context.Background(), &switchoverRecord{Percent: p}); err != nil {
			t.Fatal(err)
		}
	}
	clients := make([]string, 40)
	for i := range clients {
		clients[i] = fmt.Sprintf("caller-%d", i)
	}

	// 0%: everything on blue.
	for _, c := range clients {
		if side := switchoverHello(t, blue.srv.URL, c, nil); side != "blue" {
			t.Fatalf("0%%: %s on %s", c, side)
		}
	}
	if green.reqs.Load() != 0 {
		t.Fatalf("0%%: green saw %d requests", green.reqs.Load())
	}

	// 50%: split by caller id, the same side on every request.
	set(50)
	onGreen := map[string]bool{}
	for _, c := range clients {
		want := "blue"
		if switchoverToGreen(c, 50) {
			want = "green"
			onGreen[c] = true
		}
		for i := 0; i < 2; i++ {
			if side := switchoverHello(t, blue.srv.URL, c, nil); side != want {
				t.Fatalf("50%%: %s on %s, want %s", c, side, want)
			}
		}
	}
	if len(onGreen) == 0 || len(onGreen) == len(clients) {
		t.Fatalf("50%%: %d of %d on green", len(onGreen), len(clients))
	}

	// No caller id: the configured header is the key; then the source
	// address.
	if side := switchoverHello(t, blue.srv.URL, "", http.Header{"X-Client-Id": {"caller-0"}}); (side == "green") != onGreen["caller-0"] {
		t.Fatalf("sticky header: %s", side)
	}
	if side := switchoverHello(t, blue.srv.URL, "", nil); (side == "green") != switchoverToGreen("127.0.0.1", 50) {
		t.Fatalf("source address: %s", side)
	}

	// 100%: everything on green; the client's headers reach green.
	set(100)
	for _, c := range clients {
		if side := switchoverHello(t, blue.srv.URL, c, nil); side != "green" {
			t.Fatalf("100%%: %s on %s", c, side)
		}
	}
	auth := http.Header{"Authorization": {"Bearer client-token"}, "X-Tenant": {"t1"}, "X-Forwarded-For": {"10.0.0.1"}}
	switchoverHello(t, blue.srv.URL, "caller-1", auth)
	h := *green.lastHdr.Load()
	if h.Get("Authorization") != "Bearer client-token" || h.Get("X-Tenant") != "t1" || h.Get(PublicCallerIDHeader) != "caller-1" {
		t.Fatalf("headers on green: %v", h)
	}
	if h.Get(SwitchoverHeader) != "1" || h.Get("X-Forwarded-For") != "10.0.0.1, 127.0.0.1" {
		t.Fatalf("forwarding headers on green: %v", h)
	}

	// Green down: blue answers, green's error is reported.
	green.srv.Close()
	fallbackBefore := s.outcomes[SwitchoverOutcomeFallback].Load()
	if side := switchoverHello(t, blue.srv.URL, "caller-1", nil); side != "blue" {
		t.Fatalf("green down: answered by %s", side)
	}
	if s.outcomes[SwitchoverOutcomeFallback].Load() != fallbackBefore+1 || s.reasons[SwitchoverReasonConnect].Load() != 1 {
		t.Fatalf("fallback not counted: %+v", switchoverViewOf(blue.gw).Counts)
	}

	mr := httptest.NewRecorder()
	blue.gw.MetricsHandler().ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	m := mr.Body.String()
	for _, want := range []string{
		`switchover_total{outcome="fallback"} 1`,
		`switchover_green_errors_total{reason="connect"} 1`,
		fmt.Sprintf(`switchover_total{outcome="green"} %d`, 2*len(onGreen)+int64Bool(onGreen["caller-0"])+int64Bool(switchoverToGreen("127.0.0.1", 50))+len(clients)+1),
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("metrics missing %s:\n%s", want, grepLines(m, "switchover"))
		}
	}

	// The admin view lists the error.
	adminMux, _, err := blue.gw.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	ar := httptest.NewRecorder()
	areq := httptest.NewRequest(http.MethodGet, "/admin/switchover", nil)
	areq.Header.Set("Authorization", "Bearer "+blue.gw.AdminTokenHex())
	blue.gw.AdminMiddleware(adminMux).ServeHTTP(ar, areq)
	var view switchoverView
	if err := json.Unmarshal(ar.Body.Bytes(), &view); err != nil || ar.Code != http.StatusOK {
		t.Fatalf("getSwitchover: %d %s", ar.Code, ar.Body.String())
	}
	if !view.Enabled || view.Source != "runtime" || view.Percent != 100 || len(view.Recent) != 1 || view.Recent[0].Reason != SwitchoverReasonConnect || view.Recent[0].Outcome != SwitchoverOutcomeFallback {
		t.Fatalf("view: %+v", view)
	}
}

func int64Bool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestSwitchover_AdminOps sets, reads and clears the share through the
// admin API, with the role checks.
func TestSwitchover_AdminOps(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithSwitchover(SwitchoverOptions{Target: "http://green.invalid/graphql", Percent: 5}))
	t.Cleanup(g.Close)
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	h := g.AdminMiddleware(adminMux)
	call := func(method, tok, body string) (int, switchoverView) {
		req := httptest.NewRequest(method, "/admin/switchover", strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var v switchoverView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	admin, dev := g.AdminTokenHex(), g.DevTokenHex()
	if code, v := call(http.MethodGet, dev, ""); code != 200 || v.Source != "static" || v.Percent != 5 {
		t.Fatalf("get: %d %+v", code, v)
	}
	if code, _ := call(http.MethodPut, dev, `{"percent":50}`); code != http.StatusForbidden {
		t.Fatalf("dev put: %d", code)
	}
	if code, _ := call(http.MethodPut, admin, `{"percent":101}`); code/100 != 4 {
		t.Fatalf("percent 101: %d", code)
	}
	if code, _ := call(http.MethodPut, admin, `{"percent":10,"target":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("bad target: %d", code)
	}
	code, v := call(http.MethodPut, admin, `{"percent":50,"rules":[{"operationName":"A","exclude":true},{"header":"X-Canary","percent":100}]}`)
	if code != 200 || v.Source != "runtime" || v.Percent != 50 || len(v.Rules) != 2 || v.Target != "http://green.invalid/graphql" || v.SetBy != "standalone" {
		t.Fatalf("put: %d %+v", code, v)
	}
	if code, v := call(http.MethodDelete, admin, ""); code != 200 || v.Source != "static" || v.Percent != 5 || len(v.Rules) != 0 {
		t.Fatalf("delete: %d %+v", code, v)
	}

	off := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(off.Close)
	offMux, _, _ := off.AdminHumaRouter()
	req := httptest.NewRequest(http.MethodPut, "/admin/switchover", strings.NewReader(`{"percent":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+off.AdminTokenHex())
	rec := httptest.NewRecorder()
	off.AdminMiddleware(offMux).ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("without WithSwitchover: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSwitchover_WebSocket: a new graphql-transport-ws connection
// follows the rule; with green down it falls back to blue.
func TestSwitchover_WebSocket(t *testing.T) {
	green := newSwitchoverSide(t, "green")
	blue := newSwitchoverSide(t, "blue", WithSwitchover(SwitchoverOptions{Target: green.srv.URL + "/graphql", Percent: 100}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(blue.srv.URL, "http") + "/graphql"

	conn := dialWS(t, ctx, wsURL)
	if green.upgrade.Load() != 1 {
		t.Fatalf("green upgrades = %d, want 1", green.upgrade.Load())
	}
	// The proxied connection is registered for Drain on blue.
	blue.gw.wsMu.Lock()
	n := len(blue.gw.wsConns)
	blue.gw.wsMu.Unlock()
	if n != 1 {
		t.Fatalf("blue ws registry = %d, want 1", n)
	}
	_ = conn.CloseNow()

	green.srv.Close()
	conn = dialWS(t, ctx, wsURL)
	_ = conn.CloseNow()
	s := blue.gw.switchover
	if s.outcomes[SwitchoverOutcomeGreen].Load() != 1 || s.outcomes[SwitchoverOutcomeFallback].Load() != 1 || s.reasons[SwitchoverReasonConnect].Load() != 1 {
		t.Fatalf("counts: %+v %+v", switchoverViewOf(blue.gw).Counts, switchoverViewOf(blue.gw).GreenErrors)
	}
}

// TestSwitchover_Cluster: a setting made on one node is applied by
// the other through the gwag-cluster bucket; clearing it too.
func TestSwitchover_Cluster(t *testing.T) {
	ca, cb := getSharedCluster(t)
	resetSharedCluster(t)
	opts := SwitchoverOptions{Target: "http://green.invalid/graphql", Percent: 5}
	start := func(cl *Cluster) *Gateway {
		g := New(WithCluster(cl), WithClusterReplicas(ClusterReplicasAuto), WithoutMetrics(), WithoutBackpressure(), WithSwitchover(opts))
		t.Cleanup(g.Close)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := g.startClusterTracking(ctx); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("startClusterTracking: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return g
	}
	ga, gb := start(ca), start(cb)

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout: %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if _, err := ga.switchover.setRuntime(context.Background(), &switchoverRecord{Percent: 50, Rules: []SwitchoverRule{{Header: "X-Canary", Percent: 100}}}); err != nil {
		t.Fatal(err)
	}
	waitFor("B applies 50%", func() bool {
		c := gb.switchover.cur.Load()
		return c.runtime && c.rec.Percent == 50 && len(c.rec.Rules) == 1 && c.rec.SetBy == ca.Server.Name()
	})
	if _, err := gb.switchover.setRuntime(context.Background(), &switchoverRecord{Percent: 100}); err != nil {
		t.Fatal(err)
	}
	waitFor("A applies 100%", func() bool {
		c := ga.switchover.cur.Load()
		return c.runtime && c.rec.Percent == 100 && len(c.rec.Rules) == 0
	})
	if _, err := ga.switchover.setRuntime(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	waitFor("B back to static", func() bool {
		c := gb.switchover.cur.Load()
		return !c.runtime && c.rec.Percent == 5
	})
}

// TestSwitchover_ConcurrentErrors: concurrent green failures all fall
// back (run with -race for the recent list).
func TestSwitchover_ConcurrentErrors(t *testing.T) {
	u := newSwitchoverUnit(t, "http://127.0.0.1:1/graphql", time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.post(`{ a }`, nil)
		}()
	}
	wg.Wait()
	if u.outcome(SwitchoverOutcomeFallback) != 20 || u.blueN.Load() != 20 {
		t.Fatalf("fallback=%d blue=%d", u.outcome(SwitchoverOutcomeFallback), u.blueN.Load())
	}
}
