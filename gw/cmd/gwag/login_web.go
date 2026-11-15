package main

// gwag login --web / gwag service login --web: sign the admin UI in a
// browser in with the CLI's bearer. The CLI mints a one-time code
// (POST /api/admin/sessions/code) and prints http://HOST/#code=CODE;
// the UI trades it for a session cookie (POST /auth/session). The code
// rides in the fragment, so it reaches no server or proxy log. Gateway
// side: gw/admin_session_code.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type webCode struct {
	Code             string `json:"code"`
	Role             string `json:"role"`
	Subject          string `json:"subject"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// webLogin mints a code at endpoint with bearer and prints (and, with
// browser, opens) the sign-in URL on base (endpoint when empty).
func webLogin(ctx context.Context, endpoint, bearer, base string, browser bool, out io.Writer) error {
	if bearer == "" {
		return errors.New("no bearer to sign the browser in with")
	}
	if base == "" {
		base = endpoint
	}
	u, err := webSignInURL(base, "")
	if err != nil {
		return err
	}
	c, err := mintWebCode(ctx, endpoint, bearer)
	if err != nil {
		return err
	}
	u += url.PathEscape(c.Code)
	fmt.Fprintf(out, "Open in the browser within %ds (one use; signs in as %s, role %s):\n  %s\n", c.ExpiresInSeconds, c.Subject, c.Role, u)
	if browser {
		if err := openBrowser(u); err != nil {
			fmt.Fprintf(out, "could not open a browser (%v); open the URL above\n", err)
		}
	}
	return nil
}

// webSignInURL is base/#code=code. base must be an http(s) URL with a
// host; its path, query and fragment are dropped (the UI is at /).
func webSignInURL(base, code string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("sign-in URL base %q: want http://HOST:PORT or https://HOST (--web-url)", base)
	}
	return u.Scheme + "://" + u.Host + "/#code=" + code, nil
}

func mintWebCode(ctx context.Context, endpoint, bearer string) (webCode, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/admin/sessions/code", nil)
	if err != nil {
		return webCode{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := ssoHTTP.Do(req)
	if err != nil {
		return webCode{}, fmt.Errorf("reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return webCode{}, fmt.Errorf("%s has no /api/admin/sessions/code; the gateway is older than gwag login --web", endpoint)
	case http.StatusUnauthorized:
		return webCode{}, fmt.Errorf("%s refused the bearer (HTTP 401); sign in again (gwag login --token)", endpoint)
	default:
		var e struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(b, &e) == nil && e.Detail != "" {
			return webCode{}, fmt.Errorf("mint sign-in code: %s (HTTP %d)", e.Detail, resp.StatusCode)
		}
		return webCode{}, fmt.Errorf("mint sign-in code: HTTP %d", resp.StatusCode)
	}
	var c webCode
	if err := json.Unmarshal(b, &c); err != nil || c.Code == "" {
		return webCode{}, errors.New("mint sign-in code: no code in the answer")
	}
	return c, nil
}
