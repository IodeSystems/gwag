package gat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// outboundIdleConnsPerHost is how many keep-alive connections gat
// holds open per upstream. Go's http.DefaultClient ships
// MaxIdleConnsPerHost=2, which collapses into a fresh TCP connection
// (and handshake) per request the moment concurrency exceeds two —
// the upstream stops being the bottleneck and connection churn takes
// over. gw/ pre-bakes the same default for exactly this reason.
const outboundIdleConnsPerHost = 1024

// outboundHTTP is the client every OpenAPI dispatch forwards through.
// Package-level so all dispatchers share one connection pool: a
// per-dispatcher client would partition keep-alives by operation and
// defeat the point. Adopters needing a custom transport (proxy, mTLS)
// supply their own dispatcher via ServiceRegistration.Dispatchers.
var outboundHTTP = newOutboundHTTPClient()

func newOutboundHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = outboundIdleConnsPerHost * 4
	tr.MaxIdleConnsPerHost = outboundIdleConnsPerHost
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: tr}
}

// openAPIDispatcher implements ir.Dispatcher for one OpenAPI operation.
type openAPIDispatcher struct {
	baseURL   string
	method    string
	pathTmpl  string
	operation *openapi3.Operation
	gateway   *Gateway
	validator *ir.OpenAPIValidator
}

func newOpenAPIDispatcher(baseURL string, svc *ir.Service, op *ir.Operation) *openAPIDispatcher {
	var (
		pathTmpl  string
		method    string
		operation *openapi3.Operation
	)
	// IngestOpenAPI stamps the source operation, method and path on
	// the IR op. Use them when present: this also covers ops without
	// an operationId and OpenAPI 3.2 QUERY / additionalOperations.
	if o, ok := op.Origin.(*openapi3.Operation); ok && o != nil && op.HTTPMethod != "" && op.HTTPPath != "" {
		pathTmpl, method, operation = op.HTTPPath, op.HTTPMethod, o
	} else if doc, ok := svc.Origin.(*openapi3.T); ok && doc != nil && doc.Paths != nil {
		for p, pi := range doc.Paths.Map() {
			for m, opVal := range pi.Operations() {
				if opVal.OperationID == op.Name {
					pathTmpl = p
					method = m
					operation = opVal
					break
				}
			}
			if operation != nil {
				break
			}
		}
	}
	return &openAPIDispatcher{
		baseURL:   baseURL,
		method:    method,
		pathTmpl:  pathTmpl,
		operation: operation,
	}
}

func (d *openAPIDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	req, full, err := d.newRequest(ctx, args, "application/json")
	if err != nil {
		return nil, err
	}
	resp, err := outboundHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gat: %s %s: %w", d.method, full, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gat: %s %s: read body: %w", d.method, full, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gat: %s %s: %s: %s", d.method, full, resp.Status, strings.TrimSpace(string(respBytes)))
	}
	if len(respBytes) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("gat: decode response: %w", err)
	}
	return out, nil
}

// newRequest builds the upstream request for one call: path and
// query params, JSON body, the inbound Authorization header, Accept
// set to accept. It also returns the full URL for error messages.
func (d *openAPIDispatcher) newRequest(ctx context.Context, args map[string]any, accept string) (*http.Request, string, error) {
	if d.operation == nil || d.pathTmpl == "" {
		return nil, "", fmt.Errorf("gat: openapi dispatcher not configured")
	}
	if err := d.validator.Validate(args, d.gateway.validation); err != nil {
		return nil, "", fmt.Errorf("gat: %w", err)
	}

	resolvedPath := d.pathTmpl
	queryArgs := url.Values{}
	for _, paramRef := range d.operation.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		v, ok := args[p.Name]
		if !ok {
			continue
		}
		strVal := fmt.Sprintf("%v", v)
		switch p.In {
		case "path":
			resolvedPath = strings.ReplaceAll(resolvedPath, "{"+p.Name+"}", url.PathEscape(strVal))
		case "query":
			// A list argument is an array param: repeat the key per
			// element (form, explode — OpenAPI's default for query
			// arrays), not one "[a b]" string.
			if list, ok := v.([]any); ok {
				for _, e := range list {
					queryArgs.Add(p.Name, fmt.Sprintf("%v", e))
				}
			} else {
				queryArgs.Add(p.Name, strVal)
			}
		}
	}

	full := d.baseURL + resolvedPath
	if len(queryArgs) > 0 {
		full += "?" + queryArgs.Encode()
	}

	var body io.Reader
	if bv, ok := args["body"]; ok && bv != nil {
		b, err := json.Marshal(bv)
		if err != nil {
			return nil, full, fmt.Errorf("gat: marshal body: %w", err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, d.method, full, body)
	if err != nil {
		return nil, full, fmt.Errorf("gat: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)

	// Forward Authorization header from inbound request.
	if inbound := HTTPRequestFromContext(ctx); inbound != nil {
		if v := inbound.Header.Get("Authorization"); v != "" {
			req.Header.Set("Authorization", v)
		}
	}
	return req, full, nil
}

// openAPIStreamDispatcher serves an OpenAPI operation whose success
// response is a stream (OpenAPI 3.2 itemSchema on text/event-stream,
// application/jsonl, application/x-ndjson or application/json-seq)
// as a GraphQL subscription: one upstream request per subscriber,
// items decoded as they arrive. The upstream body closes when the
// subscriber leaves.
type openAPIStreamDispatcher struct {
	*openAPIDispatcher
	stream *ir.OpenAPIStream
}

// upstreamStatusError is an upstream's HTTP error status. GetStatus
// lets ErrorCode / HTTPStatus classify it and gives the GraphQL error
// status/code extensions.
type upstreamStatusError struct {
	status int
	msg    string
}

func (e *upstreamStatusError) Error() string  { return e.msg }
func (e *upstreamStatusError) GetStatus() int { return e.status }

func (d *openAPIStreamDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, full, err := d.newRequest(ctx, args, d.stream.MediaType)
	if err != nil {
		cancel()
		return nil, withStatusExtensions(err)
	}
	resp, err := outboundHTTP.Do(req)
	if err != nil {
		cancel()
		return nil, withStatusExtensions(&upstreamStatusError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("gat: %s %s: %s", d.method, full, err.Error()),
		})
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		return nil, withStatusExtensions(&upstreamStatusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("gat: %s %s: %s: %s", d.method, full, resp.Status, strings.TrimSpace(string(b))),
		})
	}

	// graphql-go only streams a bidirectional chan interface{}.
	out := make(chan interface{}, 16)
	go func() {
		defer close(out)
		defer cancel()
		defer resp.Body.Close()
		r := ir.NewOpenAPIStreamReader(d.stream, resp.Body)
		r.OnSkip = ir.NewSkipLogger(fmt.Sprintf("gat: %s %s", d.method, full), 10*time.Second)
		for {
			v, err := r.Next()
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					return
				}
				select {
				case out <- withStatusExtensions(fmt.Errorf("gat: %s %s: stream: %w", d.method, full, err)):
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- v:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
