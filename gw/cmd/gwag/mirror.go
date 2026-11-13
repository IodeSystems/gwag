package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

const mirrorUsage = `Usage:
  gwag ctl cluster mirror [--from BLUE] [--channel NAME]...   start
  gwag ctl cluster mirror --status
  gwag ctl cluster mirror --stop [--channel NAME]...

Event stream mirror of a cutover (docs/cutover.md), run against green
(--gateway). Green's nodes run with --mirror-from (blue's
--mirror-listen) and a system key of blue; that is the connection.
This command starts or stops the mirroring per event stream.

Start: green's stream of each of blue's event streams (EV_*), or of
each --channel (a stream name or a channel subject), sources blue's.
Green creates a stream it does not have. --from, when given, must be
green's configured source. Idempotent.

--status: per stream, whether it is mirrored, blue's and green's last
sequence and message count, and the lag NATS reports (messages on blue
not yet on green).

--stop: the streams stop following blue and stay, with their
messages, as normal streams.

--gateway is green's HTTP endpoint (default: .gw primary endpoint, else
http://localhost:8080). --bearer: green's admin token or a system key
(status also takes the dev token).`

type mirrorStream struct {
	Stream       string   `json:"stream"`
	Subjects     []string `json:"subjects"`
	Mirrored     bool     `json:"mirrored"`
	OnGreen      bool     `json:"onGreen"`
	OnBlue       bool     `json:"onBlue"`
	BlueLastSeq  uint64   `json:"blueLastSeq"`
	BlueMsgs     uint64   `json:"blueMsgs"`
	GreenLastSeq uint64   `json:"greenLastSeq"`
	GreenMsgs    uint64   `json:"greenMsgs"`
	Lag          uint64   `json:"lag"`
	ActiveMs     int64    `json:"activeMs"`
	Error        string   `json:"error"`
}

type mirrorStatusView struct {
	Configured bool           `json:"configured"`
	From       string         `json:"from"`
	Leafnodes  int            `json:"leafnodes"`
	BlueError  string         `json:"blueError"`
	Streams    []mirrorStream `json:"streams"`
}

func runMirror(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cluster mirror", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwURL := fs.String("gateway", "", "Green's HTTP endpoint (default: from .gw primary endpoint, else http://localhost:8080)")
	endpoint := fs.String("endpoint", "", "Same as --gateway")
	bearer := fs.String("bearer", "", "Green's admin token or a system key (default: from .gw)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	from := fs.String("from", "", "Blue's mirror endpoint; must be green's configured --mirror-from")
	status := fs.Bool("status", false, "Show the mirror status; change nothing")
	stop := fs.Bool("stop", false, "Stop mirroring (every mirrored stream, or each --channel)")
	channels := &stringListValue{}
	fs.Var(channels, "channel", "Stream name (EV_...) or channel subject; repeatable. Default: every event stream")
	fs.Usage = func() {
		fmt.Fprintln(stderr, mirrorUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (*status && (*stop || *from != "" || len(channels.values) > 0)) || (*stop && *from != "") {
		fs.Usage()
		return 2
	}
	login := resolveLogin(*ctxName)
	u := *gwURL
	if u == "" {
		u = *endpoint
	}
	u = resolveCtx(u, login.Endpoint, "http://localhost:8080")
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	root := strings.TrimRight(u, "/") + "/api/admin/cluster/mirror"
	tok := resolveCtx(*bearer, login.Bearer, "")

	var view mirrorStatusView
	var err error
	switch {
	case *status:
		err = adminCall(http.MethodGet, root, tok, nil, &view, 30*time.Second)
	case *stop:
		body, _ := json.Marshal(map[string]any{"streams": channels.values})
		err = adminCall(http.MethodPost, root+"/stop", tok, body, &view, 2*time.Minute)
	default:
		body, _ := json.Marshal(map[string]any{"from": *from, "streams": channels.values})
		err = adminCall(http.MethodPost, root, tok, body, &view, 2*time.Minute)
	}
	if err != nil {
		fmt.Fprintf(stderr, "cluster mirror: %v\n", err)
		return 1
	}
	printMirror(stdout, view)
	return 0
}

func printMirror(w io.Writer, v mirrorStatusView) {
	fmt.Fprintf(w, "source=%s leafnodes=%d\n", v.From, v.Leafnodes)
	if v.BlueError != "" {
		fmt.Fprintf(w, "blue not readable: %s\n", v.BlueError)
	}
	fmt.Fprintf(w, "%-40s %-8s %10s %10s %8s %8s %s\n", "STREAM", "MIRRORED", "BLUE_SEQ", "GREEN_SEQ", "LAG", "ACTIVE", "ERROR")
	for _, s := range v.Streams {
		blue, green, active := "-", "-", "-"
		if s.OnBlue {
			blue = fmt.Sprint(s.BlueLastSeq)
		}
		if s.OnGreen {
			green = fmt.Sprint(s.GreenLastSeq)
		}
		if s.Mirrored && s.ActiveMs >= 0 {
			active = (time.Duration(s.ActiveMs) * time.Millisecond).Round(time.Millisecond).String()
		}
		lag := "-"
		if s.Mirrored {
			lag = fmt.Sprint(s.Lag)
		}
		fmt.Fprintf(w, "%-40s %-8v %10s %10s %8s %8s %s\n", s.Stream, s.Mirrored, blue, green, lag, active, s.Error)
	}
}

// mirrorClusterOptions applies the gwag up --mirror-* flags.
func mirrorClusterOptions(o *gateway.ClusterOptions, listen, from, keyFile, cert, key, ca string) error {
	o.MirrorListen = listen
	o.MirrorFrom = from
	if from != "" {
		raw := os.Getenv("GWAG_MIRROR_KEY")
		if keyFile != "" {
			b, err := os.ReadFile(keyFile)
			if err != nil {
				return fmt.Errorf("--mirror-key-file: %w", err)
			}
			raw = string(b)
		}
		o.MirrorKey = strings.TrimSpace(raw)
		if o.MirrorKey == "" {
			return errors.New("--mirror-from needs --mirror-key-file or GWAG_MIRROR_KEY (a system key minted on blue)")
		}
	} else if keyFile != "" {
		return errors.New("--mirror-key-file needs --mirror-from")
	}
	if cert != "" || key != "" || ca != "" {
		if listen == "" && from == "" {
			return errors.New("--mirror-tls-* need --mirror-listen or --mirror-from")
		}
		cfg, err := gateway.LoadMTLSConfig(cert, key, ca)
		if err != nil {
			return fmt.Errorf("--mirror-tls-*: %w", err)
		}
		o.MirrorTLS = cfg
	}
	return nil
}
