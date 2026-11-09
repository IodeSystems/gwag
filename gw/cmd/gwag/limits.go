package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const limitUsage = `Usage:
  gwag limit set --service NS[:vN] [--op OP] --rps N --burst B
  gwag limit delete --service NS[:vN] [--op OP]
  gwag limit list

A cluster-wide rate limit on calls from the gateway to a service:
every node applies rps and burst divided by the live peer count.
Calls over it wait for a token up to the backpressure wait budget,
then fail 429 with Retry-After. Without --op the limit covers every
operation together; without :vN every version. An operator limit
replaces one set in code (gateway.RateLimit) for the same scope;
delete brings the code value back. list also shows quotas, costs and
this node's upstream 429 pauses. Writes need the admin token.

Common flags: --endpoint URL (default: .gw primary endpoint, else
http://localhost:8080), --bearer TOKEN (default: from .gw),
--context NAME.`

const quotaUsage = `Usage:
  gwag quota set (--caller ID | --default) [--service NS[:vN]] [--op OP] --limit N --per 1s|1m|1h|1d [--units]
  gwag quota delete (--caller ID | --default) [--service NS[:vN]] [--op OP]
  gwag quota list

A quota for one caller id (the caller-id mode the gateway runs), or
the default for every caller without its own quota at that scope;
each caller counts separately. Windows are fixed (1m starts on the
minute, 1d at 00:00 UTC), at most 24h. --units counts operation cost
units (gwag cost set) instead of calls. Without --service the quota
covers every service. Windows under 1m are split across the live
nodes; longer windows use a cluster counter flushed every few
seconds (docs/quota.md). Writes need the admin token.`

const costUsage = `Usage:
  gwag cost set --service NS[:vN] --op OP --units N
  gwag cost delete --service NS[:vN] --op OP
  gwag cost list

The units one call to an operation counts against --units quotas and
in the usage report (default 1). Replaces a cost set in code
(gateway.OperationCost).`

const usageUsage = `Usage:
  gwag usage [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--caller ID] [--service NS[:vN]] [--csv]

Usage per UTC day, caller, service and operation: calls, units,
rate-limit and quota refusals, upstream 429s, errors. Default: the
last 7 days. Needs a gateway started with usage on (gwag up --usage,
gateway.WithUsage).`

func limitCmd(args []string) int { return runLimits("limit", args, os.Stdout, os.Stderr) }
func quotaCmd(args []string) int { return runLimits("quota", args, os.Stdout, os.Stderr) }
func costCmd(args []string) int  { return runLimits("cost", args, os.Stdout, os.Stderr) }
func usageCmd(args []string) int { return runUsage(args, os.Stdout, os.Stderr) }

// limitInfo is the admin API's view of a limit record.
type limitInfo struct {
	Kind      string  `json:"kind"`
	Namespace string  `json:"namespace"`
	Version   string  `json:"version"`
	Operation string  `json:"operation"`
	Caller    string  `json:"caller"`
	Default   bool    `json:"default"`
	RPS       float64 `json:"rps"`
	Burst     int     `json:"burst"`
	Limit     uint64  `json:"limit"`
	PerMs     int64   `json:"perMs"`
	Units     bool    `json:"units"`
	Cost      uint32  `json:"cost"`
	SetBy     string  `json:"setBy"`
	Source    string  `json:"source"`
}

type limitsList struct {
	Peers         int         `json:"peers"`
	QuotaDelegate bool        `json:"quotaDelegate"`
	Usage         bool        `json:"usage"`
	Limits        []limitInfo `json:"limits"`
	Pauses        []struct {
		Namespace   string `json:"namespace"`
		Version     string `json:"version"`
		RemainingMs int64  `json:"remainingMs"`
	} `json:"pauses"`
}

func (l limitInfo) service() string {
	s := l.Namespace
	if s == "" {
		s = "*"
	}
	if l.Version != "" {
		s += ":" + l.Version
	}
	return s
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runLimits(cmd string, args []string, stdout, stderr io.Writer) int {
	usage := map[string]string{"limit": limitUsage, "quota": quotaUsage, "cost": costUsage}[cmd]
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	verb := args[0]
	flags, positionals := splitFlagsAndPositionals(args[1:])
	fs := flag.NewFlagSet(cmd+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "Gateway HTTP endpoint (default: from .gw primary, else http://localhost:8080)")
	bearer := fs.String("bearer", "", "Admin token (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	service := fs.String("service", "", "NS or NS:vN")
	op := fs.String("op", "", "Operation name")
	rps := fs.Float64("rps", 0, "Calls per second, cluster-wide (limit set)")
	burst := fs.Int("burst", 0, "Bucket size, cluster-wide (limit set; default: max(1, rps))")
	caller := fs.String("caller", "", "Caller id (quota)")
	def := fs.Bool("default", false, "The default quota of every caller (quota)")
	limit := fs.Uint64("limit", 0, "Calls or units per window (quota set)")
	per := fs.String("per", "", "Window: 1s, 1m, 1h, 1d (quota set)")
	units, costUnits := new(bool), new(uint)
	if cmd == "cost" {
		fs.UintVar(costUnits, "units", 0, "Units per call (cost set)")
	} else {
		fs.BoolVar(units, "units", false, "Count cost units instead of calls (quota set)")
	}
	fs.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positionals) > 0 {
		fs.Usage()
		return 2
	}
	login := resolveLogin(*ctxName)
	root := strings.TrimRight(resolveCtx(*endpoint, login.Endpoint, "http://localhost:8080"), "/") + "/api/admin"
	tok := resolveCtx(*bearer, login.Bearer, "")
	path := map[string]string{"limit": "/limits/rate", "quota": "/limits/quota", "cost": "/limits/cost"}[cmd]

	switch verb {
	case "list":
		var out limitsList
		if err := adminCall(http.MethodGet, root+"/limits", tok, nil, &out, 15*time.Second); err != nil {
			fmt.Fprintf(stderr, "%s list: %v\n", cmd, err)
			return 1
		}
		printLimits(stdout, cmd, out)
		return 0
	case "set":
		body := map[string]any{"service": *service, "operation": *op}
		switch cmd {
		case "limit":
			if *service == "" || *rps <= 0 {
				fs.Usage()
				return 2
			}
			b := *burst
			if b == 0 {
				b = int(*rps)
				if b < 1 {
					b = 1
				}
			}
			body["rps"], body["burst"] = *rps, b
		case "quota":
			if (*caller == "") == !*def || *limit == 0 || *per == "" {
				fs.Usage()
				return 2
			}
			body["caller"], body["default"], body["limit"], body["per"], body["units"] = *caller, *def, *limit, *per, *units
		case "cost":
			if *service == "" || *op == "" || *costUnits == 0 {
				fs.Usage()
				return 2
			}
			body["units"] = *costUnits
		}
		b, _ := json.Marshal(body)
		var rec limitInfo
		if err := adminCall(http.MethodPut, root+path, tok, b, &rec, 15*time.Second); err != nil {
			fmt.Fprintf(stderr, "%s set: %v\n", cmd, err)
			return 1
		}
		fmt.Fprintf(stdout, "set %s\n", describeLimit(rec))
		return 0
	case "delete":
		q := url.Values{}
		if *service != "" {
			q.Set("service", *service)
		}
		if *op != "" {
			q.Set("operation", *op)
		}
		if cmd == "quota" {
			if (*caller == "") == !*def {
				fs.Usage()
				return 2
			}
			if *caller != "" {
				q.Set("caller", *caller)
			}
			if *def {
				q.Set("default", "true")
			}
		}
		var out struct {
			Deleted bool `json:"deleted"`
		}
		if err := adminCall(http.MethodDelete, root+path+"?"+q.Encode(), tok, nil, &out, 15*time.Second); err != nil {
			fmt.Fprintf(stderr, "%s delete: %v\n", cmd, err)
			return 1
		}
		if !out.Deleted {
			fmt.Fprintln(stdout, "nothing to delete")
			return 0
		}
		fmt.Fprintln(stdout, "deleted")
		return 0
	}
	fs.Usage()
	return 2
}

func describeLimit(r limitInfo) string {
	scope := r.service()
	if r.Operation != "" {
		scope += " " + r.Operation
	}
	switch r.Kind {
	case "rate":
		return fmt.Sprintf("rate limit %s: %s/s burst %d", scope, strconv.FormatFloat(r.RPS, 'f', -1, 64), r.Burst)
	case "quota":
		who := r.Caller
		if r.Default {
			who = "default"
		}
		unit := "calls"
		if r.Units {
			unit = "units"
		}
		return fmt.Sprintf("quota %s for %s: %d %s per %s", scope, who, r.Limit, unit, formatPer(time.Duration(r.PerMs)*time.Millisecond))
	case "cost":
		return fmt.Sprintf("cost %s: %d units", scope, r.Cost)
	}
	return r.Kind + " " + scope
}

func printLimits(w io.Writer, cmd string, out limitsList) {
	want := map[string]string{"limit": "rate", "quota": "quota", "cost": "cost"}[cmd]
	fmt.Fprintf(w, "split peers: %d\n", out.Peers)
	if cmd == "quota" && out.QuotaDelegate {
		fmt.Fprintln(w, "WithQuota is configured: the _quota_auth/v1 delegate decides; these quotas are not checked")
	}
	n := 0
	for _, r := range out.Limits {
		if r.Kind != want {
			continue
		}
		if n == 0 {
			switch want {
			case "rate":
				fmt.Fprintf(w, "%-24s %-20s %10s %7s %-6s %s\n", "SERVICE", "OPERATION", "RPS", "BURST", "SOURCE", "SET BY")
			case "quota":
				fmt.Fprintf(w, "%-24s %-20s %-20s %10s %-6s %-6s %-6s %s\n", "SERVICE", "OPERATION", "CALLER", "LIMIT", "PER", "COUNTS", "SOURCE", "SET BY")
			case "cost":
				fmt.Fprintf(w, "%-24s %-20s %6s %-6s %s\n", "SERVICE", "OPERATION", "UNITS", "SOURCE", "SET BY")
			}
		}
		n++
		switch want {
		case "rate":
			fmt.Fprintf(w, "%-24s %-20s %10s %7d %-6s %s\n", r.service(), dashIfEmpty(r.Operation), strconv.FormatFloat(r.RPS, 'f', -1, 64), r.Burst, r.Source, dashIfEmpty(r.SetBy))
		case "quota":
			who := r.Caller
			if r.Default {
				who = "(default)"
			}
			counts := "calls"
			if r.Units {
				counts = "units"
			}
			fmt.Fprintf(w, "%-24s %-20s %-20s %10d %-6s %-6s %-6s %s\n", r.service(), dashIfEmpty(r.Operation), who, r.Limit, formatPer(time.Duration(r.PerMs)*time.Millisecond), counts, r.Source, dashIfEmpty(r.SetBy))
		case "cost":
			fmt.Fprintf(w, "%-24s %-20s %6d %-6s %s\n", r.service(), dashIfEmpty(r.Operation), r.Cost, r.Source, dashIfEmpty(r.SetBy))
		}
	}
	if n == 0 {
		fmt.Fprintf(w, "(no %s records)\n", want)
	}
	if cmd == "limit" {
		for _, p := range out.Pauses {
			fmt.Fprintf(w, "paused after upstream 429: %s:%s for %s\n", p.Namespace, p.Version, (time.Duration(p.RemainingMs) * time.Millisecond).Round(time.Second))
		}
	}
}

type usageRowOut struct {
	Day         string `json:"day"`
	Caller      string `json:"caller"`
	Namespace   string `json:"namespace"`
	Version     string `json:"version"`
	Operation   string `json:"operation"`
	Calls       uint64 `json:"calls"`
	Units       uint64 `json:"units"`
	RateLimited uint64 `json:"rateLimited"`
	Quota       uint64 `json:"quotaExceeded"`
	Upstream429 uint64 `json:"upstream429"`
	Errors      uint64 `json:"errors"`
}

func runUsage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "Gateway HTTP endpoint (default: from .gw primary, else http://localhost:8080)")
	bearer := fs.String("bearer", "", "Dev or admin token (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	from := fs.String("from", "", "First UTC day, YYYY-MM-DD (default: 6 days before --to)")
	to := fs.String("to", "", "Last UTC day, YYYY-MM-DD (default: today)")
	caller := fs.String("caller", "", "Only this caller id")
	service := fs.String("service", "", "Only this service: NS or NS:vN")
	asCSV := fs.Bool("csv", false, "Print CSV (header row, one row per line)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, usageUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	login := resolveLogin(*ctxName)
	root := strings.TrimRight(resolveCtx(*endpoint, login.Endpoint, "http://localhost:8080"), "/") + "/api/admin"
	tok := resolveCtx(*bearer, login.Bearer, "")
	q := url.Values{}
	for k, v := range map[string]string{"from": *from, "to": *to, "caller": *caller, "service": *service} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *asCSV {
		q.Set("format", "csv")
		body, err := adminGetRaw(root+"/usage?"+q.Encode(), tok, 60*time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "usage: %v\n", err)
			return 1
		}
		_, _ = stdout.Write(body)
		return 0
	}
	var out struct {
		From string        `json:"from"`
		To   string        `json:"to"`
		Rows []usageRowOut `json:"rows"`
	}
	if err := adminCall(http.MethodGet, root+"/usage?"+q.Encode(), tok, nil, &out, 60*time.Second); err != nil {
		fmt.Fprintf(stderr, "usage: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s .. %s\n", out.From, out.To)
	if len(out.Rows) == 0 {
		fmt.Fprintln(stdout, "(no usage)")
		return 0
	}
	fmt.Fprintf(stdout, "%-10s %-20s %-20s %-20s %8s %8s %8s %8s %8s %8s\n", "DAY", "CALLER", "SERVICE", "OPERATION", "CALLS", "UNITS", "LIMITED", "QUOTA", "UP429", "ERRORS")
	for _, r := range out.Rows {
		fmt.Fprintf(stdout, "%-10s %-20s %-20s %-20s %8d %8d %8d %8d %8d %8d\n", r.Day, r.Caller, r.Namespace+":"+r.Version, r.Operation, r.Calls, r.Units, r.RateLimited, r.Quota, r.Upstream429, r.Errors)
	}
	return 0
}

// formatPer prints a quota window the way it is set: 1d, 1h, 1m, 30s.
func formatPer(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

// adminGetRaw GETs u and returns the body as is.
func adminGetRaw(u, tok string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %d %s", u, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
