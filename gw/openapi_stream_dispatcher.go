package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// openAPIStreamDispatcher implements ir.Dispatcher for one OpenAPI
// operation whose success response is a stream (OpenAPI 3.2
// itemSchema on text/event-stream, application/jsonl,
// application/x-ndjson or application/json-seq). Each subscriber gets
// its own upstream HTTP request; items are decoded as they arrive and
// handed to graphql-go's executor on a chan any. No pub/sub.
//
// Stream caps follow protoDirectSubscriptionDispatcher: the source's
// streamSem (MaxStreams) and the gateway's existing streamGlobalSem
// (MaxStreamsTotal), both try-acquire — a subscriber over the cap is
// rejected with ResourceExhausted instead of queued. The unary sem /
// per-instance sem are not taken: they bound request/response calls,
// not stream lifetimes. backpressureMiddleware is not wrapped around
// this dispatcher.
//
// The upstream timeout (policy.timeout) bounds the wait for response
// headers only; the stream itself runs until upstream EOF or the
// subscriber leaves.
type openAPIStreamDispatcher struct {
	g               *Gateway
	src             *openAPISource
	op              *openapi3.Operation
	stream          *ir.OpenAPIStream
	method          string
	pathTemplate    string
	headerInjectors []headerInjector
	metrics         Metrics
	ns, ver, label  string
	validator       *ir.OpenAPIValidator
}

func newOpenAPIStreamDispatcher(g *Gateway, src *openAPISource, op *openapi3.Operation, stream *ir.OpenAPIStream, method, pathTemplate string, headers []headerInjector, metrics Metrics) *openAPIStreamDispatcher {
	label := method + " " + pathTemplate
	return &openAPIStreamDispatcher{
		g:               g,
		src:             src,
		op:              op,
		stream:          stream,
		method:          method,
		pathTemplate:    pathTemplate,
		headerInjectors: headers,
		metrics:         metrics,
		ns:              src.namespace,
		ver:             src.version,
		label:           label,
		validator:       ir.NewOpenAPIValidator(op, src.namespace+"/"+src.version+" "+label),
	}
}

// acquireStreamSlots try-acquires the gateway-wide and per-source
// stream caps. release is non-nil when err is nil.
func (d *openAPIStreamDispatcher) acquireStreamSlots() (release func(), err error) {
	global := d.g.streamGlobalSem
	if global != nil {
		select {
		case global <- struct{}{}:
		default:
			return nil, Reject(CodeResourceExhausted, "gateway stream cap reached")
		}
	}
	if d.src.streamSem != nil {
		select {
		case d.src.streamSem <- struct{}{}:
		default:
			if global != nil {
				<-global
			}
			return nil, Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: stream cap reached", d.ns, d.ver))
		}
	}
	return func() {
		if d.src.streamSem != nil {
			<-d.src.streamSem
		}
		if global != nil {
			<-global
		}
	}, nil
}

// Dispatch opens the upstream stream and returns a chan any of decoded
// items. An upstream error status, transport failure or header
// timeout is returned as a rejection before the channel exists. The
// channel closes on upstream EOF or when ctx ends (which also closes
// the upstream body). A decode or read failure mid-stream is sent as
// an error value, then the channel closes.
func (d *openAPIStreamDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	tr := tracerFromContext(ctx)
	ctx, span := tr.startDispatchSpan(ctx, "gateway.dispatch.openapi.subscription",
		namespaceAttr(d.ns),
		versionAttr(d.ver),
		methodAttr(d.label),
		httpMethodAttr(d.method),
		httpRouteAttr(d.pathTemplate),
	)
	// The span covers opening the stream, not its lifetime (same as
	// the proto subscription dispatcher).
	defer span.End()

	start := time.Now()
	ch, err := d.open(ctx, args)
	d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, time.Since(start), err)
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (d *openAPIStreamDispatcher) open(ctx context.Context, args map[string]any) (chan any, error) {
	if err := d.validator.Validate(args, d.g.cfg.validation); err != nil {
		return nil, Reject(CodeInvalidArgument, "openapi: "+err.Error())
	}
	release, err := d.acquireStreamSlots()
	if err != nil {
		return nil, err
	}
	r := d.src.pickReplica()
	if r == nil {
		release()
		return nil, Reject(CodeInternal, fmt.Sprintf("openapi: no live replicas for %s/%s", d.ns, d.ver))
	}

	streamCtx, cancel := context.WithCancel(ctx)
	fail := func(err error) (chan any, error) {
		cancel()
		release()
		return nil, err
	}
	req, full, closeups, err := buildOpenAPIRequest(streamCtx, d.method, r.baseURL, d.pathTemplate, d.op, args, d.src.forwardHeaders, d.headerInjectors, d.src.uploadStore, d.stream.MediaType)
	if err != nil {
		return fail(err)
	}
	policy := &d.src.policy
	var headerTimer *time.Timer
	if policy.timeout > 0 {
		headerTimer = time.AfterFunc(policy.timeout, cancel)
	}
	client := r.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	closeUploads(closeups)
	if headerTimer != nil && !headerTimer.Stop() {
		// The timer fired: the upstream did not answer in time.
		if resp != nil {
			resp.Body.Close()
		}
		if ctx.Err() == nil {
			r.health.failure(policy)
		}
		return fail(Reject(CodeInternal, fmt.Sprintf("openapi: %s/%s %s: upstream timeout after %s", d.ns, d.ver, d.label, policy.timeout)))
	}
	if err != nil {
		if ctx.Err() == nil {
			r.health.failure(policy)
		}
		return fail(rejectUpstream(upstreamClassTransport, CodeInternal, fmt.Sprintf("openapi: %s %s: %s", d.method, full, err.Error())))
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			r.health.failure(policy)
		}
		return fail(rejectUpstreamStatus(resp.StatusCode,
			fmt.Sprintf("openapi: %s %s: %s: %s", d.method, full, resp.Status, strings.TrimSpace(string(body)))))
	}
	r.health.success()

	poolN := int(d.src.streamInflight.Add(1))
	d.metrics.SetStreamsInflight(d.ns, d.ver, poolN)
	globalN := int(d.g.streamGlobal.Add(1))
	d.metrics.SetStreamsInflightTotal(globalN)

	ch := make(chan any, 32)
	go func() {
		defer func() {
			resp.Body.Close()
			cancel()
			d.metrics.SetStreamsInflight(d.ns, d.ver, int(d.src.streamInflight.Add(-1)))
			d.metrics.SetStreamsInflightTotal(int(d.g.streamGlobal.Add(-1)))
			release()
			close(ch)
		}()
		reader := ir.NewOpenAPIStreamReader(d.stream, resp.Body)
		for {
			v, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) || streamCtx.Err() != nil {
					return
				}
				ev := Reject(CodeInternal, fmt.Sprintf("openapi: %s %s: stream: %s", d.method, full, err.Error()))
				select {
				case ch <- ev:
				case <-streamCtx.Done():
				}
				return
			}
			select {
			case ch <- v:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return ch, nil
}

var _ ir.Dispatcher = (*openAPIStreamDispatcher)(nil)
