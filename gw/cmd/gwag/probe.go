// gwag probe: exit 0 when an HTTP GET answers 200. For container
// health checks in images with no shell or curl (distroless).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func probeCmd(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	live := fs.Bool("live", false, "Succeed on any HTTP response, not only 200 (liveness: a node that reports 503 starting is alive)")
	timeout := fs.Duration("timeout", 3*time.Second, "Request timeout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag probe [--live] URL")
		fmt.Fprintln(fs.Output(), "  GET URL; exit 0 on 200 (or any response with --live), else 1.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	c := &http.Client{Timeout: *timeout}
	resp, err := c.Get(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || *live {
		return 0
	}
	fmt.Fprintf(os.Stderr, "status %d\n", resp.StatusCode)
	return 1
}
