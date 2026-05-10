// gwag context: a per-project ./.gw/ directory holding named logins
// and per-context runtime state. Subcommands resolve missing flags
// against the primary login (or the first login if none is marked
// primary), with --context NAME as override.
//
// Layout:
//
//	.gw/
//	  credentials.json   array of named logins; one may be primary
//	  contexts/
//	    <name>/
//	      data/          gwag-up runtime state (JetStream, admin token)
//
// credentials.json shape:
//
//	{
//	  "logins": [
//	    {"name": "local", "primary": true,
//	     "gateway": "localhost:50090",
//	     "endpoint": "http://localhost:8080",
//	     "bearer": "deadbeef..."},
//	    {"name": "staging", ...}
//	  ]
//	}
//
// File mode 0600; the bearer is a secret. .gw/ is gitignored.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	contextDir      = ".gw"
	contextsSubdir  = ".gw/contexts"
	credentialsFile = ".gw/credentials.json"
)

type loginEntry struct {
	Name     string `json:"name"`
	Primary  bool   `json:"primary,omitempty"`
	Gateway  string `json:"gateway,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Bearer   string `json:"bearer,omitempty"`
}

type credentials struct {
	Logins []loginEntry `json:"logins"`
}

// loadCredentials reads .gw/credentials.json. Returns the zero value
// with no error when the file (or directory) is missing — operators
// without a context fall back to per-command flags. Malformed JSON
// is a real error.
func loadCredentials() (credentials, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return credentials{}, nil
		}
		return credentials{}, err
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return credentials{}, fmt.Errorf("parse %s: %w", credentialsFile, err)
	}
	return c, nil
}

func saveCredentials(c credentials) error {
	if err := os.MkdirAll(contextDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(credentialsFile, append(b, '\n'), 0600)
}

// resolve picks an entry from the credentials. Order: explicit name
// match > entry marked primary > first entry > zero/false.
func (c credentials) resolve(name string) (loginEntry, bool) {
	if name != "" {
		for _, e := range c.Logins {
			if e.Name == name {
				return e, true
			}
		}
		return loginEntry{}, false
	}
	for _, e := range c.Logins {
		if e.Primary {
			return e, true
		}
	}
	if len(c.Logins) > 0 {
		return c.Logins[0], true
	}
	return loginEntry{}, false
}

// upsert replaces the entry with matching name, or appends. Returns
// true when an existing entry was overwritten.
func (c *credentials) upsert(e loginEntry) bool {
	for i, x := range c.Logins {
		if x.Name == e.Name {
			c.Logins[i] = e
			return true
		}
	}
	c.Logins = append(c.Logins, e)
	return false
}

// remove drops the named entry. Returns true when found.
func (c *credentials) remove(name string) bool {
	for i, x := range c.Logins {
		if x.Name == name {
			c.Logins = append(c.Logins[:i], c.Logins[i+1:]...)
			return true
		}
	}
	return false
}

// setPrimary makes the named entry primary (and demotes any other
// entry with primary=true). Returns true when the named entry exists.
func (c *credentials) setPrimary(name string) bool {
	found := false
	for i := range c.Logins {
		if c.Logins[i].Name == name {
			c.Logins[i].Primary = true
			found = true
		} else {
			c.Logins[i].Primary = false
		}
	}
	return found
}

// contextDataDir returns .gw/contexts/<name>/data — the per-context
// data directory. gwag-up writes admin-token + JetStream here.
func contextDataDir(name string) string {
	return filepath.Join(contextsSubdir, name, "data")
}

// resolveCtx picks the first non-empty of (explicit, fromCtx, fallback).
func resolveCtx(explicit, fromCtx, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if fromCtx != "" {
		return fromCtx
	}
	return fallback
}

// resolveLogin loads .gw/credentials.json and returns the entry for
// the named context (or primary). Empty name + no logins returns the
// zero value — callers fall back to per-command defaults.
func resolveLogin(ctxName string) loginEntry {
	c, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %v (ignoring credentials)\n", err)
		return loginEntry{}
	}
	e, _ := c.resolve(ctxName)
	return e
}

func loginCmd(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	name := fs.String("name", "default", "Login name (used to switch contexts)")
	gw := fs.String("gateway", "", "Control-plane gRPC address (e.g. localhost:50090)")
	endpoint := fs.String("endpoint", "", "HTTP base URL (e.g. http://localhost:8080); empty = derive from --gateway host")
	token := fs.String("token", "", "Admin bearer token (hex); empty = leave unset")
	primary := fs.Bool("primary", false, "Mark this login primary (default: true if it's the first login)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag login --name N --gateway HOST:PORT [--endpoint URL] [--token HEX] [--primary]")
		fmt.Fprintln(fs.Output(), "  Adds or updates a login in .gw/credentials.json. The first login")
		fmt.Fprintln(fs.Output(), "  added is auto-primary; subsequent logins inherit primary=false")
		fmt.Fprintln(fs.Output(), "  unless --primary is passed. Use 'gwag use NAME' to switch later.")
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
		// Default endpoint to http://<gateway-host>:8080. Operators with
		// a different layout pass --endpoint explicitly.
		host := *gw
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		if host == "" {
			host = "localhost"
		}
		ep = "http://" + host + ":8080"
	}
	creds, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load credentials: %v\n", err)
		return 1
	}
	autoPrimary := len(creds.Logins) == 0
	entry := loginEntry{
		Name:     *name,
		Primary:  *primary || autoPrimary,
		Gateway:  *gw,
		Endpoint: ep,
		Bearer:   *token,
	}
	if entry.Primary {
		// Demote any other login that was marked primary.
		for i := range creds.Logins {
			if creds.Logins[i].Name != *name {
				creds.Logins[i].Primary = false
			}
		}
	}
	updated := creds.upsert(entry)
	if err := saveCredentials(creds); err != nil {
		fmt.Fprintf(os.Stderr, "save credentials: %v\n", err)
		return 1
	}
	abs, _ := filepath.Abs(credentialsFile)
	verb := "added"
	if updated {
		verb = "updated"
	}
	fmt.Printf("%s login %q in %s\n", verb, *name, abs)
	fmt.Printf("  gateway:  %s\n", entry.Gateway)
	fmt.Printf("  endpoint: %s\n", entry.Endpoint)
	fmt.Printf("  primary:  %v\n", entry.Primary)
	if entry.Bearer != "" {
		fmt.Println("  bearer:   ✓ (persisted)")
	} else {
		fmt.Println("  bearer:   (none)")
	}
	return 0
}

func logoutCmd(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	all := fs.Bool("all", false, "Remove the entire .gw/ directory (deletes credentials AND every context's data)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag logout [NAME]")
		fmt.Fprintln(fs.Output(), "  No NAME: removes the primary login.")
		fmt.Fprintln(fs.Output(), "  --all:   removes the entire .gw/ directory.")
		fs.PrintDefaults()
	}
	flags, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if *all {
		if err := os.RemoveAll(contextDir); err != nil {
			fmt.Fprintf(os.Stderr, "remove %s: %v\n", contextDir, err)
			return 1
		}
		fmt.Println("removed .gw/")
		return 0
	}
	creds, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load credentials: %v\n", err)
		return 1
	}
	if len(creds.Logins) == 0 {
		fmt.Println("no logins to remove")
		return 0
	}
	target := ""
	if len(positionals) > 0 {
		target = positionals[0]
	}
	if target == "" {
		// Remove the primary, or first if none primary.
		e, ok := creds.resolve("")
		if !ok {
			fmt.Println("no logins to remove")
			return 0
		}
		target = e.Name
	}
	if !creds.remove(target) {
		fmt.Fprintf(os.Stderr, "no login named %q\n", target)
		return 1
	}
	// If the removed entry was primary and others remain, promote the
	// first to primary so resolution stays deterministic.
	if len(creds.Logins) > 0 {
		hasPrimary := false
		for _, e := range creds.Logins {
			if e.Primary {
				hasPrimary = true
				break
			}
		}
		if !hasPrimary {
			creds.Logins[0].Primary = true
		}
	}
	if err := saveCredentials(creds); err != nil {
		fmt.Fprintf(os.Stderr, "save credentials: %v\n", err)
		return 1
	}
	fmt.Printf("removed login %q\n", target)
	return 0
}

func useCmd(args []string) int {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag use NAME")
		fmt.Fprintln(fs.Output(), "  Promotes NAME to primary; demotes every other login.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	target := fs.Arg(0)
	creds, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load credentials: %v\n", err)
		return 1
	}
	if !creds.setPrimary(target) {
		fmt.Fprintf(os.Stderr, "no login named %q\n", target)
		return 1
	}
	if err := saveCredentials(creds); err != nil {
		fmt.Fprintf(os.Stderr, "save credentials: %v\n", err)
		return 1
	}
	fmt.Printf("primary: %s\n", target)
	return 0
}

func contextCmd(args []string) int {
	creds, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load credentials: %v\n", err)
		return 1
	}
	if len(creds.Logins) == 0 {
		fmt.Println("(no logins; run 'gwag login' or 'gwag up')")
		return 0
	}
	fmt.Printf("%-15s %-7s %-25s %s\n", "NAME", "PRIMARY", "GATEWAY", "ENDPOINT")
	for _, e := range creds.Logins {
		mark := ""
		if e.Primary {
			mark = "✓"
		}
		fmt.Printf("%-15s %-7s %-25s %s\n", e.Name, mark, e.Gateway, e.Endpoint)
	}
	return 0
}
