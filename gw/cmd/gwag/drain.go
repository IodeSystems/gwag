package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

const drainUsage = `Usage:
  gwag ctl drain INSTANCE [--wait] [--timeout 5m] [--cancel] [--gateway HOST:PORT] [--context NAME]
  gwag ctl drain --list [--gateway HOST:PORT] [--context NAME]

INSTANCE is a registration_id or an instance_id (see --list).
Without --wait, prints the current drain state and exits 0.
With --wait, polls until safe_to_stop (exit 0) or --timeout (exit 1).
--cancel withdraws the drain; routing to the instance resumes.`

func drainCmd(args []string) int {
	return runDrain(args, os.Stdout, os.Stderr)
}

func runDrain(args []string, stdout, stderr io.Writer) int {
	flags, positionals := splitFlagsAndPositionals(args)
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwAddr := fs.String("gateway", "", "Gateway control-plane address (default: from .gw primary or localhost:50090)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	wait := fs.Bool("wait", false, "Poll until safe_to_stop")
	timeout := fs.Duration("timeout", 5*time.Minute, "With --wait: give up after this long")
	interval := fs.Duration("interval", time.Second, "With --wait: poll interval")
	cancel := fs.Bool("cancel", false, "Withdraw the drain")
	list := fs.Bool("list", false, "List registrations (instances) and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, drainUsage)
		fs.PrintDefaults()
	}
	// --wait / --cancel / --list take no value; splitFlagsAndPositionals
	// may have swallowed the next token as their value.
	flags, positionals = unswallowBoolFlags(flags, positionals, "wait", "cancel", "list")
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if !*list && len(positionals) != 1 {
		fs.Usage()
		return 2
	}
	login := resolveLogin(*ctxName)
	addr := resolveCtx(*gwAddr, login.Gateway, "localhost:50090")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(stderr, "dial %s: %v\n", addr, err)
		return 1
	}
	defer conn.Close()
	client := cpv1.NewControlPlaneClient(conn)

	if *list {
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		ctx = withBearer(ctx, login.Bearer)
		resp, err := client.ListRegistrations(ctx, &cpv1.ListRegistrationsRequest{})
		if err != nil {
			fmt.Fprintf(stderr, "ListRegistrations: %v\n", err)
			return 1
		}
		if len(resp.GetRegistrations()) == 0 {
			fmt.Fprintln(stdout, "(no registrations on this node)")
			return 0
		}
		fmt.Fprintf(stdout, "%-26s %-24s %-24s %s\n", "REGISTRATION_ID", "INSTANCE_ID", "ADDR", "LINES")
		for _, r := range resp.GetRegistrations() {
			fmt.Fprintf(stdout, "%-26s %-24s %-24s %s\n", r.GetRegistrationId(), r.GetInstanceId(), r.GetAddr(), strings.Join(r.GetNamespaces(), ","))
		}
		return 0
	}

	instance := positionals[0]
	deadline := time.Now().Add(*timeout)
	call := func(cancelReq bool) (*cpv1.DrainInstanceResponse, error) {
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		ctx = withBearer(ctx, login.Bearer)
		return client.DrainInstance(ctx, &cpv1.DrainInstanceRequest{Instance: instance, Cancel: cancelReq})
	}
	resp, err := call(*cancel)
	if err != nil {
		fmt.Fprintf(stderr, "DrainInstance: %v\n", err)
		return 1
	}
	printDrain(stdout, resp)
	if !*wait || *cancel {
		return 0
	}
	last := resp.GetState()
	for resp.GetState() != cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP {
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "timeout after %s: state %s\n", *timeout, drainStateName(resp.GetState()))
			return 1
		}
		time.Sleep(*interval)
		if resp, err = call(false); err != nil {
			fmt.Fprintf(stderr, "DrainInstance: %v\n", err)
			return 1
		}
		if resp.GetState() != last {
			printDrain(stdout, resp)
			last = resp.GetState()
		}
	}
	return 0
}

func printDrain(w io.Writer, r *cpv1.DrainInstanceResponse) {
	fmt.Fprintf(w, "%s %s %s inflight=%d lines=%s\n", r.GetRegistrationId(), r.GetInstanceId(),
		drainStateName(r.GetState()), r.GetInflight(), strings.Join(r.GetLines(), ","))
	for _, reason := range r.GetReasons() {
		fmt.Fprintf(w, "  blocked: %s\n", reason)
	}
}

func drainStateName(s cpv1.DrainState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "DRAIN_STATE_"))
}

// unswallowBoolFlags moves a token that splitFlagsAndPositionals
// paired with a boolean flag back to the positionals.
func unswallowBoolFlags(flags, positionals []string, names ...string) ([]string, []string) {
	isBool := map[string]bool{}
	for _, n := range names {
		isBool["-"+n] = true
		isBool["--"+n] = true
	}
	var out []string
	for i := 0; i < len(flags); i++ {
		out = append(out, flags[i])
		if isBool[flags[i]] && i+1 < len(flags) && !strings.HasPrefix(flags[i+1], "-") {
			positionals = append(positionals, flags[i+1])
			i++
		}
	}
	return out, positionals
}
