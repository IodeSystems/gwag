// Package schemaregistry speaks to a Confluent Schema Registry and
// implements events.Serde with Confluent's protobuf wire format: magic
// byte 0, a 4-byte big-endian schema id, the message-index array, then
// the proto body. It uses net/http only.
//
// Who produces the topic decides how it is used (docs/events-kafka.md,
// "Who produces the topic?"): own (gwag registers its protos), import (gwag
// reads a pinned copy of another team's schema), shadow (gwag keeps its
// own copy and `gwag schema check` compares it).
//
// Stability: experimental
package schemaregistry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Reference is a schema reference: an import of the schema, registered
// under its own subject.
//
// Stability: experimental
type Reference struct {
	// Name is the import path, e.g. "orders/v1/common.proto".
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Version int    `json:"version"`
}

// Schema is a registered or candidate schema.
//
// Stability: experimental
type Schema struct {
	// Schema is the schema text: .proto source, or base64 of a
	// serialized FileDescriptorProto (format=serialized).
	Schema     string      `json:"schema"`
	SchemaType string      `json:"schemaType,omitempty"`
	References []Reference `json:"references,omitempty"`
	// Set on responses.
	Subject string `json:"subject,omitempty"`
	Version int    `json:"version,omitempty"`
	ID      int    `json:"id,omitempty"`
}

// Config is how to reach a registry.
//
// Stability: experimental
type Config struct {
	// URL is the registry base URL, e.g. "https://sr.example.com:8081".
	// User info in the URL is used for basic auth.
	URL string
	// Username and Password set basic auth.
	Username, Password string
	// Token sets a bearer token. Ignored when Username is set.
	Token string
	// TLS configures HTTPS (custom CA, client certificates). Ignored
	// when HTTPClient is set.
	TLS *tls.Config
	// HTTPClient replaces the default client.
	HTTPClient *http.Client
}

// Client is a Schema Registry client. It caches schemas by id and
// subject versions it has fetched by number; both are immutable in a
// registry.
//
// Stability: experimental
type Client struct {
	base  *url.URL
	cfg   Config
	http  *http.Client
	mu    sync.Mutex
	byID  map[string]*Schema // id + "\x00" + format
	byVer map[string]*Schema // subject + "\x00" + version + "\x00" + format

	files   sync.Map // id -> protoreflect.FileDescriptor
	ensured sync.Map // ensureKey -> *Schema
}

// Error is a registry error response.
//
// Stability: experimental
type Error struct {
	Status  int
	Code    int    `json:"error_code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("schema registry: %d %s (error_code %d)", e.Status, e.Message, e.Code)
}

// Registry error codes (Confluent).
const (
	codeSubjectNotFound = 40401
	codeVersionNotFound = 40402
	codeSchemaNotFound  = 40403
	// codeSubjectLevelConfigNotFound: GET /config/{subject} of a
	// subject with no level of its own.
	codeSubjectLevelConfigNotFound = 40408
)

// IsNotFound reports whether err is a registry "not found" (subject,
// version or schema).
//
// Stability: experimental
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusNotFound || e.Code == codeSubjectNotFound || e.Code == codeVersionNotFound || e.Code == codeSchemaNotFound
}

// NewClient returns a client for cfg.
//
// Stability: experimental
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("schema registry url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("schema registry url %q: want http or https", cfg.URL)
	}
	if u.User != nil && cfg.Username == "" {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	u.User = nil
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
		if cfg.TLS != nil {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = cfg.TLS
			hc.Transport = tr
		}
	}
	return &Client{base: u, cfg: cfg, http: hc, byID: map[string]*Schema{}, byVer: map[string]*Schema{}}, nil
}

// URL returns the registry URL without user info.
func (c *Client) URL() string { return c.base.String() }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	// path holds segments escaped with seg.
	target := c.base.String() + path
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.schemaregistry.v1+json, application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/vnd.schemaregistry.v1+json")
	}
	switch {
	case c.cfg.Username != "":
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	case c.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("schema registry: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("schema registry: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		e.Message = method + " " + path + ": " + e.Message
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("schema registry: %s %s: %w", method, path, err)
	}
	return nil
}

// seg escapes one path segment (a subject).
func seg(s string) string { return url.PathEscape(s) }

func fmtQuery(format string) url.Values {
	q := url.Values{}
	if format != "" {
		q.Set("format", format)
	}
	return q
}

// SchemaByID returns the schema with id. format "" gives .proto text,
// "serialized" base64 of a FileDescriptorProto.
//
// Stability: experimental
func (c *Client) SchemaByID(ctx context.Context, id int, format string) (*Schema, error) {
	k := strconv.Itoa(id) + "\x00" + format
	c.mu.Lock()
	s := c.byID[k]
	c.mu.Unlock()
	if s != nil {
		return s, nil
	}
	s = &Schema{}
	if err := c.do(ctx, http.MethodGet, "/schemas/ids/"+strconv.Itoa(id), fmtQuery(format), nil, s); err != nil {
		return nil, err
	}
	s.ID = id
	c.mu.Lock()
	c.byID[k] = s
	c.mu.Unlock()
	return s, nil
}

// SubjectVersion returns version of subject: a number or "latest".
// Numbered versions are cached; "latest" is fetched every call.
//
// Stability: experimental
func (c *Client) SubjectVersion(ctx context.Context, subject, version, format string) (*Schema, error) {
	if version == "" {
		version = "latest"
	}
	k := subject + "\x00" + version + "\x00" + format
	if version != "latest" {
		c.mu.Lock()
		s := c.byVer[k]
		c.mu.Unlock()
		if s != nil {
			return s, nil
		}
	}
	s := &Schema{}
	if err := c.do(ctx, http.MethodGet, "/subjects/"+seg(subject)+"/versions/"+seg(version), fmtQuery(format), nil, s); err != nil {
		return nil, err
	}
	if version != "latest" {
		c.mu.Lock()
		c.byVer[k] = s
		c.mu.Unlock()
	}
	return s, nil
}

// Register registers s under subject and returns its id. Registering
// a schema the subject already holds returns the existing id.
//
// Stability: experimental
func (c *Client) Register(ctx context.Context, subject string, s Schema, normalize bool) (int, error) {
	q := url.Values{}
	if normalize {
		q.Set("normalize", "true")
	}
	var out struct {
		ID int `json:"id"`
	}
	body := Schema{Schema: s.Schema, SchemaType: typeOr(s.SchemaType), References: s.References}
	if err := c.do(ctx, http.MethodPost, "/subjects/"+seg(subject)+"/versions", q, body, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// Lookup returns the registered version of s under subject (with its
// id and version), or an error IsNotFound accepts.
//
// Stability: experimental
func (c *Client) Lookup(ctx context.Context, subject string, s Schema, normalize bool) (*Schema, error) {
	q := url.Values{}
	if normalize {
		q.Set("normalize", "true")
	}
	out := &Schema{}
	body := Schema{Schema: s.Schema, SchemaType: typeOr(s.SchemaType), References: s.References}
	if err := c.do(ctx, http.MethodPost, "/subjects/"+seg(subject), q, body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Compatibility is the registry's answer to a compatibility check.
//
// Stability: experimental
type Compatibility struct {
	IsCompatible bool     `json:"is_compatible"`
	Messages     []string `json:"messages"`
}

// CheckCompatibility asks whether s is compatible with version
// ("latest" or a number) of subject, at the subject's compatibility
// level. Messages explain a failure.
//
// Stability: experimental
func (c *Client) CheckCompatibility(ctx context.Context, subject, version string, s Schema) (*Compatibility, error) {
	if version == "" {
		version = "latest"
	}
	q := url.Values{}
	q.Set("verbose", "true")
	out := &Compatibility{}
	body := Schema{Schema: s.Schema, SchemaType: typeOr(s.SchemaType), References: s.References}
	if err := c.do(ctx, http.MethodPost, "/compatibility/subjects/"+seg(subject)+"/versions/"+seg(version), q, body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CompatibilityLevel returns the subject's compatibility level, or the
// global one when the subject has none.
//
// Stability: experimental
func (c *Client) CompatibilityLevel(ctx context.Context, subject string) (string, error) {
	q := url.Values{}
	q.Set("defaultToGlobal", "true")
	var out struct {
		Level string `json:"compatibilityLevel"`
	}
	if err := c.do(ctx, http.MethodGet, "/config/"+seg(subject), q, nil, &out); err != nil {
		return "", err
	}
	return out.Level, nil
}

func deletedQuery(deleted bool) url.Values {
	q := url.Values{}
	if deleted {
		q.Set("deleted", "true")
	}
	return q
}

// Subjects lists the registry's subjects. deleted includes
// soft-deleted ones.
//
// Stability: experimental
func (c *Client) Subjects(ctx context.Context, deleted bool) ([]string, error) {
	var out []string
	if err := c.do(ctx, http.MethodGet, "/subjects", deletedQuery(deleted), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Versions lists the version numbers of subject. deleted includes
// soft-deleted ones.
//
// Stability: experimental
func (c *Client) Versions(ctx context.Context, subject string, deleted bool) ([]int, error) {
	var out []int
	if err := c.do(ctx, http.MethodGet, "/subjects/"+seg(subject)+"/versions", deletedQuery(deleted), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Version returns version of subject, uncached. deleted also finds a
// soft-deleted version. format as for SchemaByID.
//
// Stability: experimental
func (c *Client) Version(ctx context.Context, subject string, version int, format string, deleted bool) (*Schema, error) {
	q := deletedQuery(deleted)
	if format != "" {
		q.Set("format", format)
	}
	s := &Schema{}
	if err := c.do(ctx, http.MethodGet, "/subjects/"+seg(subject)+"/versions/"+strconv.Itoa(version), q, nil, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Config returns the compatibility level set on subject, or the global
// level when subject is "". A subject with no level of its own gives
// "".
//
// Stability: experimental
func (c *Client) Config(ctx context.Context, subject string) (string, error) {
	path := "/config"
	if subject != "" {
		path += "/" + seg(subject)
	}
	var out struct {
		Level string `json:"compatibilityLevel"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, nil, &out)
	var e *Error
	if subject != "" && errors.As(err, &e) && (e.Code == codeSubjectLevelConfigNotFound || e.Code == codeSubjectNotFound) {
		return "", nil
	}
	return out.Level, err
}

func typeOr(t string) string {
	if t == "" {
		return "PROTOBUF"
	}
	return t
}
