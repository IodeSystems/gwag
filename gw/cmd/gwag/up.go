// gwag up: zero-config full-featured gateway boot. Embedded NATS
// (subscriptions + cluster-ready), persistent admin token, admin
// huma routes self-ingested as admin_* GraphQL fields, /metrics +
// /health + /schema/* endpoints, and the UI bundle on /. Auto-
// registers a primary login in .gw/credentials.json so subsequent
// gwag subcommands resolve without flags.
//
// State for the running gateway lives under .gw/contexts/<name>/data/
// (admin-token, JetStream). Each --context name gets its own subdir,
// so multiple gwag-up invocations from one project (e.g. local +
// staging-mirror) don't share state.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	gateway "github.com/iodesystems/gwag/v2/gw"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
	gwui "github.com/iodesystems/gwag/v2/ui"
)

type peerList []string

func (p *peerList) String() string     { return strings.Join(*p, ",") }
func (p *peerList) Set(v string) error { *p = append(*p, v); return nil }

func upCmd(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	httpAddr := fs.String("addr", ":8080", "HTTP listen address")
	cpAddr := fs.String("control-plane", ":50090", "Control-plane gRPC listen address")
	natsListen := fs.String("nats-listen", ":14222", "Embedded NATS client listen address")
	natsCluster := fs.String("nats-cluster", ":14248", "Embedded NATS cluster route address")
	var natsPeers peerList
	fs.Var(&natsPeers, "nats-peer", "Cluster peer route, repeatable")
	natsClusterName := fs.String("nats-cluster-name", "", "NATS cluster identifier; empty = default")
	nodeName := fs.String("node-name", "", "Cluster node name (defaults to control-plane addr)")
	dataDir := fs.String("data-dir", "", "Persistent state directory (default: .gw/contexts/<context>/data)")
	ctxName := fs.String("context", "", "Login name in .gw/credentials.json to register and use (default: primary, or 'default' if .gw is empty)")
	noContext := fs.Bool("no-context-write", false, "Skip auto-writing the credential entry to .gw/credentials.json")
	allowTier := fs.String("allow-tier", "unstable,stable,vN", "Comma-separated tiers accepted by this gateway")
	insecureSubscribe := fs.Bool("insecure-subscribe", false, "Disable HMAC verification on subscriptions (dev only)")
	signerSecret := fs.String("signer-secret", "", "Hex-encoded bearer for SignSubscriptionToken (admin token also works)")
	environment := fs.String("environment", "", "Environment name this gateway belongs to (e.g. dev, staging, prod); a label shown by the admin API and UI")
	pprofEnable := fs.Bool("pprof", false, "Mount net/http/pprof under /debug/pprof behind AdminMiddleware")
	publicServices := fs.String("public-services", "", "Admin readers without a token see these services: \"all\", or a comma-separated list of namespaces. Empty = only services that register with public=true. Admin visibility only; calls through the gateway are not affected.")
	autoFinalize := fs.Bool("cluster-auto-finalize", false, "Raise the cluster feature level as soon as every live peer supports a higher one, instead of waiting for gwag cluster finalize. Ends the rollback window without an operator step")
	clusterReplicas := fs.String("cluster-replicas", "odd", "JetStream KV replica policy: odd (largest odd <= min(peers,3); 2 peers stay at R1), auto (min(peers,3)), or a fixed integer 1..5 capped at live peers")
	declHook := fs.String("declarations-hook", "", "POST every external-dependent change to this URL. In a cluster the node that accepted the write sends it, once")
	declHookSecret := fs.String("declarations-hook-secret", "", "HMAC-SHA256 key for --declarations-hook (the string's bytes; at least 32 bytes and 128 bits of estimated entropy, else refused; make one with `gwag secret gen`): X-Gwag-Signature over X-Gwag-Timestamp + \".\" + body")
	declHookMode := fs.String("declarations-hook-mode", "full", "--declarations-hook body: full (the whole set) or delta (one change)")
	declFile := fs.String("declarations-file", "", "Keep this file in step with the external dependents. Every node writes its own")
	declFileMode := fs.String("declarations-file-mode", "full", "--declarations-file content: full (the whole set as JSON, replaced atomically) or delta (one JSON line appended per change)")
	shutdownDelay := fs.Duration("shutdown-delay", 5*time.Second, "Keep serving this long after /health flips to 503 on SIGTERM/SIGINT, so load balancers pull the node before the listener closes. Default 5s. Use 0 for an interactive run or with no load balancer in front. Otherwise use at least the LB probe interval x unhealthy threshold")
	shutdownTimeout := fs.Duration("shutdown-timeout", gateway.DefaultShutdownTimeout, "Bound for the stream drain and, separately, for in-flight HTTP requests on SIGTERM/SIGINT")
	mcpUpstreams := &stringListValue{}
	fs.Var(mcpUpstreams, "mcp-upstream", "Ingest a downstream MCP server's tools — NS:TRANSPORT:TARGET (transport = stdio|http|sse; repeatable)")
	usageOn := fs.Bool("usage", false, "Record usage per caller, service, operation and day for 'gwag usage' (GET /api/admin/usage); stored in the gwag-usage bucket in a cluster")
	usageRetention := fs.Duration("usage-retention", 0, "How long usage day rows are kept (default 35 days); in a cluster, the gwag-usage bucket max age when this node creates it")
	callerIDPublic := fs.Bool("caller-id-public", false, "Take each call's caller id from the X-Caller-Id header (gRPC metadata caller-id) for usage stats, deprecatedStats and the caller metric label. Forgeable: for dev, or behind a proxy that sets the header. Default off: every caller is \"unknown\"")
	requireAppKeys := fs.Bool("require-app-keys", false, "Control plane: remote Register/Heartbeat/Deregister/DrainInstance need an app key covering the namespaces (gRPC metadata authorization: Bearer KEY; mint with 'gwag key create'), operator RPCs the admin token. Default off: the control plane is open")
	switchoverOn := fs.Bool("switchover", false, "Blue side of a cutover: forward a share of GraphQL requests to green (docs/cutover.md). The share starts at 0; set it with gwag cluster switchover. Implied by --switchover-target")
	switchoverTarget := fs.String("switchover-target", "", "Green's GraphQL URL for --switchover (e.g. http://green:8080/api/graphql); gwag cluster switchover --target can set it instead")
	switchoverSticky := fs.String("switchover-sticky-header", "", "With --switchover: request header that keeps a client on one side when it has no caller id (e.g. X-Client-Id); else the source address")
	replayTarget := fs.String("replay-verify-target", "", "Blue side of replay verification (docs/cutover.md): green's verify URL (e.g. http://green:8080/api/admin/replay/verify). Records a sample of GraphQL requests with their upstream calls and posts them there. Default off")
	replayTokenFile := fs.String("replay-verify-token-file", "", "With --replay-verify-target: file holding the bearer for green (a system key minted on green). Read for every post, so it may appear or change after start")
	replayRate := fs.Float64("replay-verify-sample-rate", 1, "With --replay-verify-target: fraction (0..1] of GraphQL requests recorded")
	replayEndpoint := fs.Bool("replay-verify-endpoint", false, "Green side of replay verification: serve POST /api/admin/replay/verify (system key or admin token). Default off")
	mirrorListen := fs.String("mirror-listen", "", "Blue side of the event stream mirror (docs/cutover.md): NATS leafnode listen address (e.g. :14223) for a green cluster, which connects with a system key of this cluster. Default off. Set on every node")
	mirrorFrom := fs.String("mirror-from", "", "Green side of the event stream mirror: blue's --mirror-listen address(es), comma-separated. Needs --mirror-key-file or GWAG_MIRROR_KEY. Default off. Set on every node; start the mirror with gwag cluster mirror")
	mirrorKeyFile := fs.String("mirror-key-file", "", "With --mirror-from: file holding a system key minted on blue (gwag key create --system). Env GWAG_MIRROR_KEY works the same when the flag is empty")
	mirrorTLSCert := fs.String("mirror-tls-cert", "", "TLS certificate of the mirror listener (blue) or of the connection to it (green); with --mirror-tls-key and --mirror-tls-ca (mutual TLS). Default: plain TCP")
	mirrorTLSKey := fs.String("mirror-tls-key", "", "Key for --mirror-tls-cert")
	mirrorTLSCA := fs.String("mirror-tls-ca", "", "CA that signs both sides' --mirror-tls-cert")
	adminTokenFile := fs.String("admin-token-file", "", "Read the admin bearer token (hex) from this file instead of generating one in the data dir. Env GWAG_ADMIN_TOKEN works the same when the flag is empty. Use it to hand every node of a deployment one platform-managed secret")
	srFlags := addSchemaRegistryFlags(fs)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gwag up [flags]")
		fmt.Fprintln(fs.Output(), "  Boots a standalone gateway: embedded NATS, admin endpoints, UI,")
		fmt.Fprintln(fs.Output(), "  metrics, health. Defaults are zero-config; pass --nats-peer to")
		fmt.Fprintln(fs.Output(), "  join an existing cluster.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tiers, err := parseAllowTier(*allowTier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--allow-tier: %v\n", err)
		return 2
	}

	// Resolve which credential entry this invocation owns. Default
	// is always "default" — never silently overwrite an existing
	// remote primary (e.g. operator has --name staging set, runs
	// gwag up; the local gateway should land in a distinct slot).
	resolvedCtx := *ctxName
	if resolvedCtx == "" {
		resolvedCtx = "default"
	}

	dir := *dataDir
	if dir == "" {
		dir = contextDataDir(resolvedCtx)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatalf("data-dir %s: %v", dir, err)
	}

	clusterNodeName := *nodeName
	if clusterNodeName == "" {
		clusterNodeName = "gwag" + *cpAddr
	}
	replicaPolicy, err := gateway.ParseClusterReplicas(*clusterReplicas)
	if err != nil {
		log.Fatalf("--cluster-replicas: %v", err)
	}
	clusterOpts := gateway.ClusterOptions{
		NodeName:      clusterNodeName,
		ClientListen:  *natsListen,
		ClusterListen: *natsCluster,
		Peers:         natsPeers,
		DataDir:       dir,
		ClusterName:   *natsClusterName,
	}
	if err := mirrorClusterOptions(&clusterOpts, *mirrorListen, *mirrorFrom, *mirrorKeyFile, *mirrorTLSCert, *mirrorTLSKey, *mirrorTLSCA); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cluster, err := gateway.StartCluster(clusterOpts)
	if err != nil {
		log.Fatalf("start cluster: %v", err)
	}
	log.Printf("nats client=%s cluster=%s data=%s peers=%v node=%s",
		*natsListen, *natsCluster, dir, []string(natsPeers), cluster.NodeID)
	if *mirrorListen != "" {
		log.Printf("event stream mirror listener on %s (system keys)", *mirrorListen)
	}
	if *mirrorFrom != "" {
		log.Printf("event stream mirror source %s", *mirrorFrom)
	}

	gwOpts := []gateway.Option{
		gateway.WithAllowTier(tiers...),
		gateway.WithCluster(cluster),
		gateway.WithClusterReplicas(replicaPolicy),
		gateway.WithAdminDataDir(dir),
		gateway.WithEnvironment(*environment),
		gateway.WithPublicServices(strings.Split(*publicServices, ",")...),
	}
	tokOpt, err := adminTokenOption(*adminTokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "--admin-token-file: %v\n", err)
		return 2
	}
	if tokOpt != nil {
		gwOpts = append(gwOpts, tokOpt)
	}
	if *insecureSubscribe {
		gwOpts = append(gwOpts, gateway.WithoutSubscriptionAuth())
	}
	if *autoFinalize {
		gwOpts = append(gwOpts, gateway.WithClusterAutoFinalize())
	}
	if *signerSecret != "" {
		secret, err := hex.DecodeString(*signerSecret)
		if err != nil {
			log.Fatalf("--signer-secret must be hex: %v", err)
		}
		gwOpts = append(gwOpts, gateway.WithSignerSecret(secret))
	}
	if *pprofEnable {
		gwOpts = append(gwOpts, gateway.WithPprof())
	}
	if *requireAppKeys {
		gwOpts = append(gwOpts, gateway.WithRequireAppKeys())
	}
	if *callerIDPublic {
		gwOpts = append(gwOpts, gateway.WithCallerIDPublic())
	}
	if *usageOn {
		gwOpts = append(gwOpts, gateway.WithUsage(gateway.UsageOptions{Retention: *usageRetention}))
	}
	if *switchoverOn || *switchoverTarget != "" {
		gwOpts = append(gwOpts, gateway.WithSwitchover(gateway.SwitchoverOptions{Target: *switchoverTarget, StickyHeader: *switchoverSticky}))
	}
	replayOpts, err := replayVerifyOptions(*replayTarget, *replayTokenFile, *replayRate, *replayEndpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	gwOpts = append(gwOpts, replayOpts...)
	srOpt, err := srFlags.option()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if srOpt != nil {
		gwOpts = append(gwOpts, srOpt)
	}
	sinkOpts, err := declarationsSinkOptions(*declHook, *declHookSecret, *declHookMode, *declFile, *declFileMode)
	if err != nil {
		log.Fatal(err)
	}
	gwOpts = append(gwOpts, sinkOpts...)
	gw := gateway.New(gwOpts...)

	cpLis, err := net.Listen("tcp", *cpAddr)
	if err != nil {
		log.Fatalf("listen control plane: %v", err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	cpv1.RegisterControlPlaneServer(srv, gw.ControlPlane())
	go func() {
		log.Printf("control plane listening on %s", *cpAddr)
		if err := srv.Serve(cpLis); err != nil {
			log.Fatalf("control plane serve: %v", err)
		}
	}()

	adminMux, adminSpec, err := gw.AdminHumaRouter()
	if err != nil {
		log.Fatalf("admin huma router: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/graphql", gw.Handler())
	mux.Handle("/api/ingress/", http.StripPrefix("/api/ingress", gw.IngressHandler()))
	mux.Handle("/api/connect/", http.StripPrefix("/api/connect", gw.ConnectHandler()))
	mux.Handle("/api/schema", gw.SchemaHandler())
	mux.Handle("/api/schema/graphql", gw.SchemaHandler())
	mux.Handle("/api/schema/proto", gw.SchemaProtoHandler())
	mux.Handle("/api/schema/openapi", gw.SchemaOpenAPIHandler())
	mux.Handle("/api/metrics", gw.MetricsHandler())
	mux.Handle("/api/health", gw.HealthHandler())
	mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))
	gw.MountMCP(mux)
	srFlags.mount(mux, gw)
	if pmux := gw.PprofMux(); pmux != nil {
		mux.Handle("/debug/pprof/", gw.AdminMiddleware(pmux))
		log.Printf("pprof enabled at /debug/pprof (admin bearer required)")
	}
	mux.Handle("/", apiOrUIHandler(gateway.UIHandler(gwui.FS())))

	for _, v := range mcpUpstreams.values {
		ns, transport, target, err := parseMCPUpstream(v)
		if err != nil {
			log.Fatalf("--mcp-upstream: %v", err)
		}
		if err := gw.AddMCP(transport, target, gateway.As(ns)); err != nil {
			log.Fatalf("register mcp upstream %q: %v", v, err)
		}
		log.Printf("registered mcp upstream %s (%s) → namespace %s", target, transport, ns)
	}

	if err := gw.AddOpenAPIBytes(adminSpec,
		gateway.As("admin"),
		gateway.To("http://localhost"+*httpAddr+"/api")); err != nil {
		log.Fatalf("self-ingest admin openapi: %v", err)
	}

	if err := gw.AddAdminEvents(); err != nil {
		log.Fatalf("AddAdminEvents: %v", err)
	}

	token := gw.AdminTokenHex()
	if tokOpt != nil {
		// Supplied from a platform secret: keep it out of the logs.
		log.Printf("admin token = (supplied; not logged)")
		log.Printf("dev token   = (not logged; persisted to %s)", gw.DevTokenPath())
	} else if path := gw.AdminTokenPath(); path != "" {
		log.Printf("admin token = %s  (persisted to %s)", token, path)
		log.Printf("dev token   = %s  (persisted to %s)", gw.DevTokenHex(), gw.DevTokenPath())
	} else {
		log.Printf("admin token = %s  (in-memory)", token)
		log.Printf("dev token   = %s  (in-memory)", gw.DevTokenHex())
	}

	// Auto-login: write the credential entry and promote it to
	// primary so subsequent `gwag` subcommands resolve here without
	// flags. Demotes any other primary; operator switches back with
	// `gwag use NAME`. Skip with --no-context-write.
	if !*noContext {
		// gwag up manages the project-local context; load local-only so a
		// global fallback isn't copied into a fresh .gw/.
		creds, _ := loadCredentialsAt(credentialsFile)
		entry := loginEntry{
			Name:     resolvedCtx,
			Primary:  true,
			Gateway:  endpointHost(*cpAddr),
			Endpoint: "http://" + endpointHost(*httpAddr),
			Bearer:   token,
		}
		for i := range creds.Logins {
			if creds.Logins[i].Name != resolvedCtx {
				creds.Logins[i].Primary = false
			}
		}
		creds.upsert(entry)
		if err := saveCredentials(creds); err != nil {
			log.Printf("warn: write credentials: %v", err)
		} else {
			abs, _ := filepath.Abs(credentialsFile)
			log.Printf("registered context %q (primary) in %s", resolvedCtx, abs)
		}
	}

	log.Printf("graphql listening on %s", *httpAddr)
	err = gateway.ServeHTTP(gateway.NewHTTPServer(*httpAddr, mux), gw, gateway.ShutdownOptions{
		Delay:   *shutdownDelay,
		Timeout: *shutdownTimeout,
		Teardown: func() {
			srv.GracefulStop()
			cluster.Close()
		},
	})
	if err != nil {
		log.Printf("http: %v", err)
		return 1
	}
	return 0
}

// endpointHost converts a listen-style addr (":8080" or "host:8080")
// into a host:port suitable for a credential entry. Bare ":PORT"
// becomes "localhost:PORT" so the saved context dials through
// localhost regardless of the bind address.
// declarationsSinkOptions turns the --declarations-* flags into
// gateway options.
func declarationsSinkOptions(hookURL, hookSecret, hookMode, filePath, fileMode string) ([]gateway.Option, error) {
	var opts []gateway.Option
	if hookURL != "" {
		if hookSecret != "" {
			if err := gateway.CheckSecretStrength([]byte(hookSecret)); err != nil {
				return nil, fmt.Errorf("--declarations-hook-secret: %w", err)
			}
		}
		m, err := gateway.ParseDeclarationsSinkMode(hookMode)
		if err != nil {
			return nil, fmt.Errorf("--declarations-hook-mode: %w", err)
		}
		opts = append(opts, gateway.WithDeclarationsHook(gateway.DeclarationsHookOptions{URL: hookURL, Secret: []byte(hookSecret), Mode: m}))
	}
	if filePath != "" {
		m, err := gateway.ParseDeclarationsSinkMode(fileMode)
		if err != nil {
			return nil, fmt.Errorf("--declarations-file-mode: %w", err)
		}
		opts = append(opts, gateway.WithDeclarationsFile(gateway.DeclarationsFileOptions{Path: filePath, Mode: m}))
	}
	return opts, nil
}

func endpointHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// apiOrUIHandler is the catch-all for the gwag up mux: unmatched
// /api/* returns JSON 404 (so a typo in a client doesn't render the
// SPA); everything else is the embedded UI bundle, with index.html
// fallback for SPA routes. Mirrors examples/multi/cmd/gateway's
// apiOrUI; copied here so gwag has no dependency on the example.
func apiOrUIHandler(uiFS http.Handler) http.HandlerFunc {
	ui := uiFS
	if ui == nil {
		ui = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "ui not embedded", http.StatusNotFound)
		})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			esc := strings.ReplaceAll(r.URL.Path, `\`, `\\`)
			esc = strings.ReplaceAll(esc, `"`, `\"`)
			fmt.Fprintf(w, `{"error":"not found","path":"%s"}`, esc)
			return
		}
		ui.ServeHTTP(w, r)
	}
}
