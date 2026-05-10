// gwag login / logout: persist a per-project gateway context to
// ./.gw so subcommands don't have to re-take --gateway / --bearer /
// --endpoint on every invocation. Resolution order in subcommands is
// explicit flag > context file > built-in default.
//
// Format is JSON with three optional fields: gateway (control-plane
// gRPC addr), endpoint (HTTP base URL), bearer (admin token hex).
// 0600 perms because the bearer is a secret. .gw is in .gitignore.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const contextFile = ".gw"

type gwContext struct {
	Gateway  string `json:"gateway,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Bearer   string `json:"bearer,omitempty"`
}

// loadContext reads ./.gw if present. Returns the zero value with no
// error when the file is missing — operators without a context just
// rely on per-command flags. Malformed JSON is a real error.
func loadContext() (gwContext, error) {
	b, err := os.ReadFile(contextFile)
	if err != nil {
		if os.IsNotExist(err) {
			return gwContext{}, nil
		}
		return gwContext{}, err
	}
	var ctx gwContext
	if err := json.Unmarshal(b, &ctx); err != nil {
		return gwContext{}, fmt.Errorf("parse %s: %w", contextFile, err)
	}
	return ctx, nil
}

func saveContext(ctx gwContext) error {
	b, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(contextFile, append(b, '\n'), 0600)
}

// resolveCtx picks the first non-empty of (explicit flag, context
// field, fallback). Used by every subcommand for gateway / endpoint
// / bearer flags. Empty fallback means "no default" (the caller
// should validate downstream).
func resolveCtx(explicit, fromCtx, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if fromCtx != "" {
		return fromCtx
	}
	return fallback
}

func loginCmd(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	gw := fs.String("gateway", "", "Control-plane gRPC address (e.g. localhost:50090)")
	endpoint := fs.String("endpoint", "", "HTTP base URL (e.g. http://localhost:8080); empty = derive from --gateway host")
	token := fs.String("token", "", "Admin bearer token (hex); empty = leave unset")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag login --gateway HOST:PORT [--endpoint URL] [--token HEX]")
		fmt.Fprintln(fs.Output(), "  Writes ./.gw (mode 0600). Subcommands resolve missing flags from it.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *gw == "" {
		fs.Usage()
		return 2
	}
	ep := *endpoint
	if ep == "" {
		// Default endpoint to http://<gateway-host>:8080 — matches the
		// example gateway's default HTTP port. Operators with a
		// different layout pass --endpoint explicitly.
		host := *gw
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		if host == "" {
			host = "localhost"
		}
		ep = "http://" + host + ":8080"
	}
	ctx := gwContext{Gateway: *gw, Endpoint: ep, Bearer: *token}
	if err := saveContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "save context: %v\n", err)
		return 1
	}
	abs, _ := filepath.Abs(contextFile)
	fmt.Printf("logged in: %s\n", abs)
	fmt.Printf("  gateway:  %s\n", ctx.Gateway)
	fmt.Printf("  endpoint: %s\n", ctx.Endpoint)
	if *token != "" {
		fmt.Println("  bearer:   ✓ (persisted)")
	} else {
		fmt.Println("  bearer:   (none)")
	}
	return 0
}

func logoutCmd(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag logout")
		fmt.Fprintln(fs.Output(), "  Removes ./.gw if present.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.Remove(contextFile); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no context to remove")
			return 0
		}
		fmt.Fprintf(os.Stderr, "remove context: %v\n", err)
		return 1
	}
	fmt.Println("logged out")
	return 0
}
