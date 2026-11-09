package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// printPeers writes the peer table and the cluster lines under it.
func printPeers(w io.Writer, resp *cpv1.ListPeersResponse) {
	fmt.Fprintf(w, "%-58s %-10s %-26s %-7s %s\n", "NODE_ID", "NAME", "JOINED", "LEVELS", "VERSION")
	for _, p := range resp.GetPeers() {
		joined := time.UnixMilli(p.GetJoinedUnixMs()).Format(time.RFC3339)
		levels := fmt.Sprintf("%d-%d", p.GetMinLevel(), p.GetMaxLevel())
		version := p.GetVersion()
		if version == "" {
			version = "(no level support)"
		}
		fmt.Fprintf(w, "%-58s %-10s %-26s %-7s %s\n", p.GetNodeId(), p.GetName(), joined, levels, version)
	}
	fmt.Fprintf(w, "\ncluster feature level: %d\n", resp.GetActiveLevel())
	if resp.GetMetaSize() > 0 {
		offline := "none"
		if len(resp.GetMetaOffline()) > 0 {
			offline = strings.Join(resp.GetMetaOffline(), ",")
		}
		fmt.Fprintf(w, "jetstream meta: leader=%s size=%d offline=%s\n", resp.GetMetaLeader(), resp.GetMetaSize(), offline)
	}
}

// evacuatePeer runs `gwag peer evacuate`.
func evacuatePeer(client cpv1.ControlPlaneClient, bearer, nodeID string, checkOnly, leave bool, wait time.Duration) int {
	deadline := time.Now().Add(wait)
	call := func(check bool) (*cpv1.EvacuatePeerResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return client.EvacuatePeer(withBearer(ctx, bearer), &cpv1.EvacuatePeerRequest{NodeId: nodeID, CheckOnly: check, Leave: leave})
	}
	// Each poll sends the evacuation again (unless --check): a replica
	// count raise after the first one can place a replica back on the
	// peer while it is still live. The gateway refuses to start while a
	// stream is not whole (FailedPrecondition); that is polled too.
	resp, err := call(checkOnly)
	for (err == nil && !resp.GetDone() || evacuateNotReady(err)) && time.Now().Before(deadline) {
		time.Sleep(time.Second)
		resp, err = call(checkOnly)
	}
	if evacuateNotReady(err) {
		fmt.Fprintln(os.Stderr, "evacuation not started: these streams are not at the cluster's replica count with every replica current.")
		fmt.Fprintln(os.Stderr, "An evacuation holds replica raises back until the peer is forgotten, so it waits for them. Run again, or with a longer --wait.")
		for _, v := range notReadyStreams(err) {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		return 3
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "EvacuatePeer: %v\n", err)
		return 1
	}
	for _, r := range resp.GetRemaining() {
		fmt.Printf("  %s\n", r)
	}
	fmt.Printf("peer=%s done=%v left=%v\n", resp.GetName(), resp.GetDone(), resp.GetLeft())
	if !resp.GetDone() {
		return 3
	}
	return 0
}

// evacuateNotReady: the gateway did not start the evacuation because
// a stream is not whole.
func evacuateNotReady(err error) bool {
	return err != nil && status.Code(err) == codes.FailedPrecondition
}

// notReadyStreams lists the streams of a FailedPrecondition answer as
// "STREAM: reason", or the status message when it has no details.
func notReadyStreams(err error) []string {
	st := status.Convert(err)
	var out []string
	for _, d := range st.Details() {
		if pf, ok := d.(*errdetails.PreconditionFailure); ok {
			for _, v := range pf.GetViolations() {
				out = append(out, v.GetSubject()+": "+v.GetDescription())
			}
		}
	}
	if len(out) == 0 {
		out = []string{st.Message()}
	}
	return out
}

// clusterCmd runs `gwag cluster ...`.
func clusterCmd(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "switchover":
			return runSwitchover(args[1:], os.Stdout, os.Stderr)
		case "freeze", "unfreeze", "export", "import":
			return clusterStateCmd(args, os.Stdin, os.Stdout, os.Stderr)
		case "mirror":
			return runMirror(args[1:], os.Stdout, os.Stderr)
		}
	}
	if len(args) == 0 || args[0] != "finalize" {
		fmt.Fprintln(os.Stderr, "Usage: gwag cluster finalize [--level N] [--dry-run] [--gateway HOST:PORT] [--context NAME]")
		fmt.Fprintln(os.Stderr, "       gwag cluster switchover [--percent N] [--rule SPEC]... [--target URL] [--clear] [--gateway URL]")
		fmt.Fprintln(os.Stderr, "       gwag cluster (freeze|unfreeze|export|import) ... (cutover state copy; gwag cluster export --help)")
		fmt.Fprintln(os.Stderr, "       gwag cluster mirror [--from BLUE] [--channel NAME]... [--status] [--stop] [--gateway GREEN] (event stream mirror)")
		return 2
	}
	fs := flag.NewFlagSet("cluster finalize", flag.ContinueOnError)
	gwAddr := fs.String("gateway", "", "Gateway control-plane address (default: from .gw primary or localhost:50090)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json (default: primary)")
	level := fs.Uint("level", 0, "Level to raise to; 0 = the highest level the gateway's binary supports")
	dryRun := fs.Bool("dry-run", false, "Report what would happen; change nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	login := resolveLogin(*ctxName)
	addr := resolveCtx(*gwAddr, login.Gateway, "localhost:50090")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", addr, err)
		return 1
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cpv1.NewControlPlaneClient(conn).FinalizeCluster(withBearer(ctx, login.Bearer), &cpv1.FinalizeClusterRequest{Level: uint32(*level), DryRun: *dryRun})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FinalizeCluster: %v\n", err)
		return 1
	}
	for _, b := range resp.GetBlockers() {
		fmt.Printf("blocked by %s\n", b)
	}
	verb := "level"
	if *dryRun {
		verb = "level (dry run)"
	}
	fmt.Printf("%s %d -> %d\n", verb, resp.GetPreviousLevel(), resp.GetActiveLevel())
	if len(resp.GetBlockers()) > 0 {
		return 1
	}
	return 0
}
