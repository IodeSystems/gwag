package gat

import (
	"io"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
)

// adapterRequest converts a huma.Context into an *http.Request so the
// existing net/http handlers (graphql endpoint, schema views) can run
// without a parallel huma-Context-shaped reimplementation.
//
// Headers and the body reader are populated; the URL is reconstructed
// from Host + URL. Adopter-side middleware is irrelevant at this layer
// since huma is the outer adapter and has already run its own.
func adapterRequest(ctx huma.Context) *http.Request {
	body := io.NopCloser(ctx.BodyReader())
	u := ctx.URL()
	r, _ := http.NewRequestWithContext(ctx.Context(), ctx.Method(), u.String(), body)
	r.Host = ctx.Host()
	r.RemoteAddr = ctx.RemoteAddr()
	r.Header = http.Header{}
	ctx.EachHeader(func(name, value string) {
		r.Header.Add(name, value)
	})
	if u.RawQuery != "" {
		r.URL.RawQuery = u.RawQuery
	}
	if r.URL == nil {
		r.URL = &url.URL{Path: ctx.URL().Path}
	}
	return r
}

// adapterResponseWriter wraps huma.Context as an http.ResponseWriter.
// http.HandlerFunc-style handlers can write to it transparently; the
// adapter forwards status, headers, and body bytes through huma's
// abstraction.
type adapterResponseWriter struct {
	ctx     huma.Context
	headers http.Header
	written bool
}

func newAdapterResponseWriter(ctx huma.Context) *adapterResponseWriter {
	return &adapterResponseWriter{ctx: ctx, headers: http.Header{}}
}

func (w *adapterResponseWriter) Header() http.Header {
	return w.headers
}

func (w *adapterResponseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	for name, vals := range w.headers {
		for _, v := range vals {
			w.ctx.AppendHeader(name, v)
		}
	}
	w.ctx.SetStatus(status)
	w.written = true
}

func (w *adapterResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ctx.BodyWriter().Write(b)
}

