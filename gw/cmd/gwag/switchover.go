package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const switchoverUsage = `Usage:
  gwag ctl cluster switchover [--gateway URL]                       show the setting
  gwag ctl cluster switchover --percent N [--rule SPEC]... [--target URL] [--gateway URL]
  gwag ctl cluster switchover --clear [--gateway URL]

Sets the share of GraphQL requests blue (the gateway at --gateway, a
node started with WithSwitchover) forwards to green. In a cluster the
setting is stored cluster-wide; every blue node applies it within about
a second. It replaces the static Percent and Rules; --clear goes back
to them. Raise in steps (5, 50, 100): a client on green stays on green
when the percent goes up.

--rule SPEC (repeatable, first match decides; a request no rule matches
stays on blue): comma-separated op=NAME, header=NAME, value=VALUE,
percent=N (else --percent), exclude (stay on blue). Examples:
  --rule op=CreateOrder,exclude --rule percent=50
  --rule header=X-Canary,value=1,percent=100

--gateway is blue's HTTP endpoint here (default: .gw primary endpoint,
else http://localhost:8080), not the control-plane address. Needs the
admin token to set or clear; the dev token can read.`

// switchoverView is the admin API's view (GET/PUT /admin/switchover).
type switchoverView struct {
	Enabled      bool             `json:"enabled"`
	Active       bool             `json:"active"`
	Source       string           `json:"source"`
	Target       string           `json:"target"`
	Percent      int              `json:"percent"`
	Rules        []switchoverRule `json:"rules"`
	StickyHeader string           `json:"stickyHeader"`
	SetBy        string           `json:"setBy"`
	SetAtUnixMs  int64            `json:"setAtUnixMs"`
	Counts       map[string]int64 `json:"counts"`
	GreenErrors  map[string]int64 `json:"greenErrors"`
	Recent       []struct {
		AtUnixMs      int64  `json:"atUnixMs"`
		Reason        string `json:"reason"`
		Outcome       string `json:"outcome"`
		OperationName string `json:"operationName"`
		Error         string `json:"error"`
	} `json:"recent"`
}

type switchoverRule struct {
	OperationName string `json:"operationName,omitempty"`
	Header        string `json:"header,omitempty"`
	HeaderValue   string `json:"headerValue,omitempty"`
	Percent       int    `json:"percent,omitempty"`
	Exclude       bool   `json:"exclude,omitempty"`
}

func (r switchoverRule) String() string {
	var parts []string
	if r.OperationName != "" {
		parts = append(parts, "op="+r.OperationName)
	}
	if r.Header != "" {
		parts = append(parts, "header="+r.Header)
	}
	if r.HeaderValue != "" {
		parts = append(parts, "value="+r.HeaderValue)
	}
	if r.Percent > 0 {
		parts = append(parts, "percent="+strconv.Itoa(r.Percent))
	}
	if r.Exclude {
		parts = append(parts, "exclude")
	}
	if len(parts) == 0 {
		return "(every request)"
	}
	return strings.Join(parts, ",")
}

// parseSwitchoverRule reads one --rule SPEC.
func parseSwitchoverRule(spec string) (switchoverRule, error) {
	var r switchoverRule
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, hasV := strings.Cut(item, "=")
		switch {
		case k == "exclude" && !hasV:
			r.Exclude = true
		case k == "op" && hasV:
			r.OperationName = v
		case k == "header" && hasV:
			r.Header = v
		case k == "value" && hasV:
			r.HeaderValue = v
		case k == "percent" && hasV:
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return r, fmt.Errorf("rule %q: percent %q is not in 1..100", spec, v)
			}
			r.Percent = n
		default:
			return r, fmt.Errorf("rule %q: unknown item %q (want op=, header=, value=, percent=, exclude)", spec, item)
		}
	}
	return r, nil
}

func runSwitchover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cluster switchover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwURL := fs.String("gateway", "", "Blue's HTTP endpoint (default: from .gw primary endpoint, else http://localhost:8080)")
	endpoint := fs.String("endpoint", "", "Same as --gateway")
	bearer := fs.String("bearer", "", "Admin token (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	percent := fs.Int("percent", 0, "Share (0..100) of matching requests sent to green")
	target := fs.String("target", "", "Green's GraphQL URL (default: the gateway's WithSwitchover Target)")
	clear := fs.Bool("clear", false, "Remove the runtime setting; back to the static configuration")
	rules := &stringListValue{}
	fs.Var(rules, "rule", "Rule SPEC (repeatable): op=NAME,header=NAME,value=VALUE,percent=N,exclude")
	fs.Usage = func() {
		fmt.Fprintln(stderr, switchoverUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *clear && (set["percent"] || set["rule"] || set["target"]) {
		fmt.Fprintln(stderr, "--clear takes no --percent, --rule or --target")
		return 2
	}
	if !set["percent"] && (set["rule"] || set["target"]) {
		fmt.Fprintln(stderr, "--rule and --target need --percent (the setting is replaced as a whole)")
		return 2
	}
	if *percent < 0 || *percent > 100 {
		fmt.Fprintln(stderr, "--percent must be in 0..100")
		return 2
	}
	login := resolveLogin(*ctxName)
	base := *gwURL
	if base == "" {
		base = *endpoint
	}
	u := strings.TrimRight(resolveCtx(base, login.Endpoint, "http://localhost:8080"), "/") + "/api/admin/switchover"
	tok := resolveCtx(*bearer, login.Bearer, "")

	var view switchoverView
	var err error
	switch {
	case *clear:
		err = keyCall(http.MethodDelete, u, tok, nil, &view)
	case set["percent"]:
		body := map[string]any{"percent": *percent, "target": *target}
		var rs []switchoverRule
		for _, spec := range rules.values {
			r, perr := parseSwitchoverRule(spec)
			if perr != nil {
				fmt.Fprintln(stderr, perr)
				return 2
			}
			rs = append(rs, r)
		}
		if len(rs) > 0 {
			body["rules"] = rs
		}
		b, _ := json.Marshal(body)
		err = keyCall(http.MethodPut, u, tok, b, &view)
	default:
		err = keyCall(http.MethodGet, u, tok, nil, &view)
	}
	if err != nil {
		fmt.Fprintf(stderr, "cluster switchover: %v\n", err)
		return 1
	}
	printSwitchover(stdout, view)
	return 0
}

func printSwitchover(w io.Writer, v switchoverView) {
	if !v.Enabled {
		fmt.Fprintln(w, "switchover is not enabled on this gateway (WithSwitchover)")
		return
	}
	target := v.Target
	if target == "" {
		target = "(none)"
	}
	fmt.Fprintf(w, "source=%s active=%v target=%s percent=%d\n", v.Source, v.Active, target, v.Percent)
	if v.SetBy != "" {
		fmt.Fprintf(w, "set by %s at %s\n", v.SetBy, time.UnixMilli(v.SetAtUnixMs).UTC().Format(time.RFC3339))
	}
	for i, r := range v.Rules {
		fmt.Fprintf(w, "rule %d: %s\n", i+1, r)
	}
	if v.StickyHeader != "" {
		fmt.Fprintf(w, "sticky key: caller id, else header %s, else source address\n", v.StickyHeader)
	}
	c, e := v.Counts, v.GreenErrors
	fmt.Fprintf(w, "this node: green=%d fallback=%d error=%d blue=%d\n", c["green"], c["fallback"], c["error"], c["blue"])
	fmt.Fprintf(w, "green errors: connect=%d timeout=%d transport=%d status=%d bad_response=%d\n",
		e["connect"], e["timeout"], e["transport"], e["status"], e["badResponse"])
	for _, r := range v.Recent {
		fmt.Fprintf(w, "  %s %s %s op=%q %s\n", time.UnixMilli(r.AtUnixMs).UTC().Format(time.RFC3339), r.Reason, r.Outcome, r.OperationName, r.Error)
	}
}
