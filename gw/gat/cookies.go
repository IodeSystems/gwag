package gat

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
)

// GraphQL transport has no per-operation response-header channel: a single
// HTTP response can carry many resolved fields, so a handler's huma output
// headers (e.g. a `Set-Cookie` for login/logout) are normally dropped on the
// GraphQL surface. cookieSink bridges that gap — the GraphQL HTTP handler
// installs one per request, the in-proc dispatcher feeds each handler output's
// Set-Cookie value into it, and the handler emits them onto the real response
// before the body is written. Resolvers can run concurrently, so it's guarded.
type cookieSink struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

type cookieSinkKey struct{}

// withCookieSink returns a context carrying a fresh sink and the sink itself.
func withCookieSink(ctx context.Context) (context.Context, *cookieSink) {
	s := &cookieSink{}
	return context.WithValue(ctx, cookieSinkKey{}, s), s
}

func cookieSinkFrom(ctx context.Context) *cookieSink {
	s, _ := ctx.Value(cookieSinkKey{}).(*cookieSink)
	return s
}

// addFromOutput scans a huma output struct for fields tagged
// `header:"Set-Cookie"` and records their cookie value(s).
func (s *cookieSink) addFromOutput(out any) {
	for _, c := range extractSetCookies(out) {
		s.mu.Lock()
		s.cookies = append(s.cookies, c)
		s.mu.Unlock()
	}
}

// emit writes the collected cookies onto w. Must be called before the response
// status/body is written. Cookies with an invalid (empty) name are skipped by
// http.SetCookie, so an output that didn't set its cookie is a no-op.
func (s *cookieSink) emit(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cookies {
		http.SetCookie(w, c)
	}
}

// extractSetCookies pulls cookies off any output struct field tagged
// `header:"Set-Cookie"`. Supports http.Cookie / *http.Cookie and slices of
// either, matching huma's response-header convention.
func extractSetCookies(out any) []*http.Cookie {
	v := reflect.ValueOf(out)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	var cookies []*http.Cookie
	for i := 0; i < t.NumField(); i++ {
		tag, ok := t.Field(i).Tag.Lookup("header")
		if !ok {
			continue
		}
		if name := strings.SplitN(tag, ",", 2)[0]; !strings.EqualFold(name, "Set-Cookie") {
			continue
		}
		cookies = append(cookies, cookiesFromValue(v.Field(i))...)
	}
	return cookies
}

func cookiesFromValue(fv reflect.Value) []*http.Cookie {
	switch c := fv.Interface().(type) {
	case http.Cookie:
		return []*http.Cookie{&c}
	case *http.Cookie:
		if c != nil {
			return []*http.Cookie{c}
		}
	case []http.Cookie:
		out := make([]*http.Cookie, 0, len(c))
		for i := range c {
			cc := c[i]
			out = append(out, &cc)
		}
		return out
	case []*http.Cookie:
		return c
	}
	return nil
}
