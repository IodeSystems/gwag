package profile

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// MaxSpecBytes caps a fetched spec or profile.
const MaxSpecBytes = 16 << 20

// Reachable opens a TCP connection to addr (host:port or URL) and
// closes it.
func Reachable(ctx context.Context, addr string) error {
	hp, err := DialAddr(addr)
	if err != nil {
		return err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", hp)
	if err != nil {
		return fmt.Errorf("address %s not reachable: %w", hp, err)
	}
	_ = c.Close()
	return nil
}

// Health GETs u and wants 200.
func Health(ctx context.Context, c *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("health %s: %w", u, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health %s: status %d, want 200", u, resp.StatusCode)
	}
	return nil
}

// Fetch GETs u and returns the body (at most MaxSpecBytes). Anything
// but 200 is an error.
func Fetch(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml, application/protobuf, text/plain, */*")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d, want 200", u, resp.StatusCode)
	}
	if len(b) > MaxSpecBytes {
		return nil, fmt.Errorf("fetch %s: larger than %d bytes", u, MaxSpecBytes)
	}
	return b, nil
}

// SpecMatches fetches the spec at u and compares it with the
// registered one, in canonical form (Canonical). The error says which
// line differs.
func SpecMatches(ctx context.Context, c *http.Client, u, kind string, registered []byte, imports map[string][]byte) error {
	want, pkg, err := Canonical(kind, registered, imports, "")
	if err != nil {
		return fmt.Errorf("registered spec: %w", err)
	}
	got, err := Fetch(ctx, c, u)
	if err != nil {
		return err
	}
	have, _, err := Canonical(kind, got, imports, pkg)
	if err != nil {
		return fmt.Errorf("spec %s: %w", u, err)
	}
	if d := Diff(want, have); d != "" {
		return fmt.Errorf("spec %s differs from the registered spec (%s)", u, d)
	}
	return nil
}

// NoRedirects is an http.Client CheckRedirect that refuses redirects,
// so a probe cannot be sent to another host.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Origin returns scheme://host of an http(s) URL.
func Origin(u string) (string, error) {
	b, err := BaseURL(u)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("%q: want an http(s) URL", u)
	}
	return b.String(), nil
}
