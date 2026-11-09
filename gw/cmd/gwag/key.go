package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const keyUsage = `Usage:
  gwag key create --ns NAMESPACE [--ns ...] --ttl 30d [--note TEXT]
  gwag key create --system --ttl 30d [--note TEXT]
  gwag key list
  gwag key revoke ID

App keys let a service register, heartbeat, deregister and drain
bindings in the listed namespaces (on a gateway with
--require-app-keys) and write the Schema Registry subjects of their
channels. A system key (--system, gws_...) is the cluster-to-cluster
credential of a cutover: it may call only replay verify and the
cluster state export and import (docs/cutover.md). create prints the
key on stdout, alone, once; the details go to stderr. list shows both
kinds; revoke takes either id. Every command needs the admin token.

Common flags: --endpoint URL (default: .gw primary endpoint, else
http://localhost:8080), --bearer TOKEN (default: from .gw),
--context NAME.`

func keyCmd(args []string) int {
	return runKey(args, os.Stdout, os.Stderr)
}

// appKeyInfo is the admin API's view of a key.
type appKeyInfo struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Namespaces  []string `json:"namespaces"`
	Note        string   `json:"note"`
	CreatedAt   string   `json:"createdAt"`
	CreatedBy   string   `json:"createdBy"`
	ExpiresAt   string   `json:"expiresAt"`
	Expired     bool     `json:"expired"`
	ExpiresSoon bool     `json:"expiresSoon"`
}

func runKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, keyUsage)
		return 2
	}
	verb := args[0]
	flags, positionals := splitFlagsAndPositionals(args[1:])
	fs := flag.NewFlagSet("key "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "Gateway HTTP endpoint (default: from .gw primary, else http://localhost:8080)")
	bearer := fs.String("bearer", "", "Admin token (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	ns := &stringListValue{}
	fs.Var(ns, "ns", "Namespace the key covers (repeatable; create)")
	ttl := fs.String("ttl", "", "Lifetime: days (30d) or a Go duration (720h) (create)")
	note := fs.String("note", "", "Free text shown in the list (create)")
	system := fs.Bool("system", false, "Mint a system key (cluster-to-cluster; no --ns) (create)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, keyUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	login := resolveLogin(*ctxName)
	root := strings.TrimRight(resolveCtx(*endpoint, login.Endpoint, "http://localhost:8080"), "/") + "/api/admin"
	base := root + "/app-keys"
	tok := resolveCtx(*bearer, login.Bearer, "")

	switch verb {
	case "create":
		if *system {
			if len(ns.values) > 0 || *ttl == "" || len(positionals) > 0 {
				fs.Usage()
				return 2
			}
			body, _ := json.Marshal(map[string]any{"ttl": *ttl, "note": *note})
			var out struct {
				Key    string     `json:"key"`
				AppKey appKeyInfo `json:"appKey"`
			}
			if err := keyCall(http.MethodPost, root+"/system-keys", tok, body, &out); err != nil {
				fmt.Fprintf(stderr, "key create: %v\n", err)
				return 1
			}
			fmt.Fprintln(stdout, out.Key)
			fmt.Fprintf(stderr, "system key id %s, expires %s. The key is shown once; store it now.\n", out.AppKey.ID, out.AppKey.ExpiresAt)
			return 0
		}
		if len(ns.values) == 0 || *ttl == "" || len(positionals) > 0 {
			fs.Usage()
			return 2
		}
		var nss []string
		for _, v := range ns.values {
			nss = append(nss, strings.Split(v, ",")...)
		}
		body, _ := json.Marshal(map[string]any{"namespaces": nss, "ttl": *ttl, "note": *note})
		var out struct {
			Key    string     `json:"key"`
			AppKey appKeyInfo `json:"appKey"`
		}
		if err := keyCall(http.MethodPost, base, tok, body, &out); err != nil {
			fmt.Fprintf(stderr, "key create: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, out.Key)
		fmt.Fprintf(stderr, "id %s, namespaces %s, expires %s. The key is shown once; store it now.\n",
			out.AppKey.ID, strings.Join(out.AppKey.Namespaces, ","), out.AppKey.ExpiresAt)
		return 0
	case "list":
		var out struct {
			AppKeys []appKeyInfo `json:"appKeys"`
		}
		if err := keyCall(http.MethodGet, base, tok, nil, &out); err != nil {
			fmt.Fprintf(stderr, "key list: %v\n", err)
			return 1
		}
		if len(out.AppKeys) == 0 {
			fmt.Fprintln(stdout, "(no app keys)")
			return 0
		}
		fmt.Fprintf(stdout, "%-16s %-6s %-26s %-21s %-13s %s\n", "ID", "KIND", "NAMESPACES", "EXPIRES", "STATE", "NOTE")
		for _, k := range out.AppKeys {
			state := "valid"
			switch {
			case k.Expired:
				state = "expired"
			case k.ExpiresSoon:
				state = "expires-soon"
			}
			nss := strings.Join(k.Namespaces, ",")
			if nss == "" {
				nss = "-"
			}
			fmt.Fprintf(stdout, "%-16s %-6s %-26s %-21s %-13s %s\n", k.ID, k.Kind, nss, k.ExpiresAt, state, k.Note)
		}
		return 0
	case "revoke":
		if len(positionals) != 1 {
			fs.Usage()
			return 2
		}
		if err := keyCall(http.MethodDelete, base+"/"+url.PathEscape(positionals[0]), tok, nil, nil); err != nil {
			fmt.Fprintf(stderr, "key revoke: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "revoked %s\n", positionals[0])
		return 0
	}
	fs.Usage()
	return 2
}

// keyCall sends one admin API request and decodes the JSON answer
// into out (nil: ignore it).
func keyCall(method, u, tok string, body []byte, out any) error {
	return adminCall(method, u, tok, body, out, 15*time.Second)
}
