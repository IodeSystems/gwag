package ir

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPI 3.2 sequential media types an OpenAPI operation can stream
// its success response in. Each one paired with `itemSchema` on the
// response's Media Type Object makes the operation a subscription.
//
// Stability: experimental
const (
	StreamSSE     = "text/event-stream"
	StreamJSONL   = "application/jsonl"
	StreamNDJSON  = "application/x-ndjson"
	StreamJSONSeq = "application/json-seq"
)

// streamMediaTypes is the preference order when one response offers
// more than one sequential media type.
var streamMediaTypes = []string{StreamSSE, StreamJSONL, StreamNDJSON, StreamJSONSeq}

// OpenAPIStream describes an OpenAPI operation whose success response
// is a stream of items (OpenAPI 3.2 `itemSchema` on a sequential media
// type).
//
// Stability: experimental
type OpenAPIStream struct {
	// MediaType is one of StreamSSE, StreamJSONL, StreamNDJSON,
	// StreamJSONSeq.
	MediaType string

	// Item is the schema of one emitted event.
	Item *openapi3.SchemaRef

	// SSEData is set for StreamSSE when an event is the JSON value in
	// each SSE event's data field (itemSchema's `data` carries
	// contentMediaType application/json + contentSchema, or itemSchema
	// has no `data` property). When unset, an SSE event is the
	// envelope {data, event, id, retry} itemSchema describes.
	SSEData bool

	// sseDataJSON is set in envelope mode when the `data` property is
	// typed as something other than a string, so data is decoded as
	// JSON rather than kept as text.
	sseDataJSON bool
}

// OpenAPIStreamOf reports whether op streams its success response,
// and how. It returns nil when op has a unary JSON success response
// (application/json with a schema on 200, 201 or default): an
// operation offering both stays a unary Query/Mutation, so adding a
// stream alternative to an existing operation does not change its
// GraphQL shape. Otherwise the first of 200, 201, default that has a
// sequential media type with itemSchema decides.
//
// Stability: experimental
func OpenAPIStreamOf(op *openapi3.Operation) *OpenAPIStream {
	if op == nil || op.Responses == nil {
		return nil
	}
	var resps []*openapi3.Response
	for _, code := range []int{200, 201} {
		if r := op.Responses.Status(code); r != nil && r.Value != nil {
			resps = append(resps, r.Value)
		}
	}
	if d := op.Responses.Default(); d != nil && d.Value != nil {
		resps = append(resps, d.Value)
	}
	for _, r := range resps {
		if mt, ok := r.Content["application/json"]; ok && mt != nil && mt.Schema != nil {
			return nil
		}
	}
	for _, r := range resps {
		if s := streamOfContent(r.Content); s != nil {
			return s
		}
	}
	return nil
}

func streamOfContent(content openapi3.Content) *OpenAPIStream {
	byType := map[string]*openapi3.MediaType{}
	for k, mt := range content {
		if mt == nil || mt.ItemSchema == nil || mt.ItemSchema.Value == nil {
			continue
		}
		base, _, err := mime.ParseMediaType(k)
		if err != nil {
			continue
		}
		byType[base] = mt
	}
	for _, t := range streamMediaTypes {
		mt, ok := byType[t]
		if !ok {
			continue
		}
		s := &OpenAPIStream{MediaType: t, Item: mt.ItemSchema}
		if t == StreamSSE {
			sseItem(s)
		}
		return s
	}
	return nil
}

// sseItem decides what one SSE event becomes. With an envelope
// itemSchema (a `data` property, or a oneOf/anyOf whose variants all
// have one) the event is the envelope, unless `data` is JSON with a
// contentSchema, which then is the event. An itemSchema with no `data`
// property is read as the schema of the JSON in data.
func sseItem(s *OpenAPIStream) {
	item := s.Item.Value
	data := sseDataProp(item)
	if data == nil {
		if isSSEEnvelopeUnion(item) {
			return
		}
		s.SSEData = true
		return
	}
	if d := data.Value; d != nil {
		if d.ContentSchema != nil && d.ContentSchema.Value != nil && isJSONMediaType(d.ContentMediaType) {
			s.Item = d.ContentSchema
			s.SSEData = true
			return
		}
		if pt := primaryOpenAPIType(d); pt != "" && pt != "string" {
			s.sseDataJSON = true
		}
	}
}

func sseDataProp(s *openapi3.Schema) *openapi3.SchemaRef {
	if s == nil {
		return nil
	}
	if d, ok := s.Properties["data"]; ok && d != nil {
		return d
	}
	return nil
}

func isSSEEnvelopeUnion(s *openapi3.Schema) bool {
	variants := s.OneOf
	if len(variants) == 0 {
		variants = s.AnyOf
	}
	if len(variants) == 0 {
		return false
	}
	for _, v := range variants {
		if v == nil || sseDataProp(v.Value) == nil {
			return false
		}
	}
	return true
}

// isJSONMediaType: an empty contentMediaType with a contentSchema is
// read as JSON too.
func isJSONMediaType(t string) bool {
	if t == "" {
		return true
	}
	base, _, err := mime.ParseMediaType(t)
	if err != nil {
		return false
	}
	return base == "application/json" || strings.HasSuffix(base, "+json")
}

// maxStreamItemBytes caps one item (an SSE line, a JSON line, a
// json-seq record) so a stream without separators cannot grow the
// buffer without bound.
const maxStreamItemBytes = 16 << 20

// OpenAPIStreamReader reads the items of one streamed response body.
//
// Stability: experimental
type OpenAPIStreamReader struct {
	s  *OpenAPIStream
	sc *bufio.Scanner

	// OnSkip, when set, is called with the decode error of each item
	// Next skipped. An item that does not decode is dropped and the
	// stream continues (RFC 7464 §2.4 for json-seq; the same for the
	// other framings, whose framing still resyncs at the next line or
	// blank line). Errors from the body or an over-long item still end
	// the stream through Next.
	OnSkip func(error)

	// SSE parse state (the text/event-stream interpretation rules).
	lastID string
}

func (r *OpenAPIStreamReader) skip(err error) {
	if r.OnSkip != nil {
		r.OnSkip(err)
	}
}

// NewSkipLogger returns an OnSkip func that logs the first skipped
// item and then at most one line per interval, with the count
// suppressed in between. Safe for one reader goroutine.
//
// Stability: experimental
func NewSkipLogger(prefix string, interval time.Duration) func(error) {
	var (
		last       time.Time
		suppressed int
	)
	return func(err error) {
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < interval {
			suppressed++
			return
		}
		if suppressed > 0 {
			log.Printf("%s: skipped malformed stream item: %v (%d more skipped since last log)", prefix, err, suppressed)
		} else {
			log.Printf("%s: skipped malformed stream item: %v", prefix, err)
		}
		last, suppressed = now, 0
	}
}

// NewOpenAPIStreamReader reads the items of body, framed per
// s.MediaType.
//
// Stability: experimental
func NewOpenAPIStreamReader(s *OpenAPIStream, body io.Reader) *OpenAPIStreamReader {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), maxStreamItemBytes)
	switch s.MediaType {
	case StreamSSE:
		sc.Split(scanSSELines)
	case StreamJSONSeq:
		sc.Split(scanJSONSeq)
	default:
		sc.Split(bufio.ScanLines)
	}
	return &OpenAPIStreamReader{s: s, sc: sc}
}

// Next returns the next item, decoded as encoding/json decodes into
// `any`. Items that fail to decode are skipped (see OnSkip). It returns io.EOF when the stream ends; an SSE event left
// unterminated at the end is dropped, as text/event-stream has it.
func (r *OpenAPIStreamReader) Next() (any, error) {
	if r.s.MediaType == StreamSSE {
		return r.nextSSE()
	}
	for r.sc.Scan() {
		b := bytes.TrimSpace(r.sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			r.skip(fmt.Errorf("decode %s item: %w", r.s.MediaType, err))
			continue
		}
		return v, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (r *OpenAPIStreamReader) nextSSE() (any, error) {
	var (
		data     strings.Builder
		hasData  bool
		event    string
		retry    int64
		hasRetry bool
	)
	for r.sc.Scan() {
		line := r.sc.Text()
		if line == "" {
			// Dispatch. No data field: no event (the parse rules
			// reset and wait for the next one).
			if !hasData {
				event, hasRetry = "", false
				continue
			}
			payload := strings.TrimSuffix(data.String(), "\n")
			v, err := r.sseEvent(payload, event, retry, hasRetry)
			if err != nil {
				r.skip(err)
				data.Reset()
				hasData, event, hasRetry = false, "", false
				continue
			}
			return v, nil
		}
		if line[0] == ':' {
			continue // comment / heartbeat
		}
		field, value := line, ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		case "event":
			event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				r.lastID = value
			}
		case "retry":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 && isASCIIDigits(value) {
				retry, hasRetry = n, true
			}
		}
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (r *OpenAPIStreamReader) sseEvent(payload, event string, retry int64, hasRetry bool) (any, error) {
	if r.s.SSEData {
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, fmt.Errorf("decode %s data: %w", StreamSSE, err)
		}
		return v, nil
	}
	env := map[string]any{"data": payload}
	if r.s.sseDataJSON {
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, fmt.Errorf("decode %s data: %w", StreamSSE, err)
		}
		env["data"] = v
	}
	if event != "" {
		env["event"] = event
	}
	if r.lastID != "" {
		env["id"] = r.lastID
	}
	if hasRetry {
		env["retry"] = float64(retry) // encoding/json's number shape
	}
	return env, nil
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// scanSSELines splits on CRLF, LF or CR, the three line endings
// text/event-stream allows.
func scanSSELines(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		switch c {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil // need one more byte to tell CR from CRLF
		}
	}
	if atEOF && len(data) > 0 {
		// Unterminated last line: hand it back so the field is read,
		// but no blank line follows, so the event is not dispatched.
		return len(data), data, nil
	}
	return 0, nil, nil
}

// scanJSONSeq splits RFC 7464 records: each starts with RS (0x1E).
// Bytes before the first RS are skipped.
func scanJSONSeq(data []byte, atEOF bool) (int, []byte, error) {
	start := bytes.IndexByte(data, 0x1E)
	if start < 0 {
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
	if next := bytes.IndexByte(data[start+1:], 0x1E); next >= 0 {
		return start + 1 + next, data[start+1 : start+1+next], nil
	}
	if atEOF {
		return len(data), data[start+1:], nil
	}
	return start, nil, nil
}
