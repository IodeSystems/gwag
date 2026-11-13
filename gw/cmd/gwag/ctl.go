package main

import (
	"fmt"
	"io"
	"os"
	"sort"
)

// gwag ctl: every command that talks to a running gateway's admin API
// or control plane. Before 2.x these were top-level names (gwag peer,
// gwag schema, ...); legacyAliases keeps them for one release.

// cmdFunc is one dispatchable command. Handlers that write to the
// process streams directly ignore the writers.
type cmdFunc func(args []string, stdout, stderr io.Writer) int

const topUsage = `Usage: gwag [gateway flags]         run a gateway (gwag --help lists the flags)
       gwag up [flags]              zero-config local gateway: NATS + admin + UI + auto-login
       gwag serve [flags]           single-upstream embedded translator
       gwag ctl <noun> ...          talk to a running gateway (gwag ctl help)
       gwag service install|uninstall|start|stop|restart|status|logs|login
                                    run the gateway as a user service (systemd / launchd)
       gwag login | logout | use | context
                                    manage saved gateway logins
       gwag deploy up|down|status|login|export
                                    Compose, minikube, k8s and ECS files
       gwag gen --lang go|ts|python typed event code from .proto files
       gwag probe URL               HTTP health check for shell-less images
       gwag help                    this text`

const ctlUsage = `Usage: gwag ctl <noun> ...

  services    list | add | remove | profiles | init | check | wait | schema
  peer        list | evacuate NODE_ID | forget NODE_ID
  cluster     finalize | switchover | freeze | unfreeze | export | import | mirror
  drain       drain a service instance
  schema      fetch | diff | check | push | import | snapshot
  key         create | list | revoke
  sign        sign a subscription channel token
  secret      gen
  limit       set | delete | list   (per-pool concurrency limits)
  quota       set | delete | list
  cost        set | delete | list
  usage       per-caller usage report

Each noun prints its own usage when run without arguments.`

// ctlNouns maps each ctl noun to its handler. Tests replace entries.
var ctlNouns = map[string]cmdFunc{
	"services": func(a []string, o, e io.Writer) int { return servicesDispatch(a, o, e) },
	"peer":     func(a []string, _, _ io.Writer) int { return peerCmd(a) },
	"cluster":  func(a []string, _, _ io.Writer) int { return clusterCmd(a) },
	"drain":    func(a []string, _, _ io.Writer) int { return drainCmd(a) },
	"schema":   func(a []string, _, _ io.Writer) int { return schemaCmd(a) },
	"key":      func(a []string, _, _ io.Writer) int { return keyCmd(a) },
	"sign":     func(a []string, _, _ io.Writer) int { return signCmd(a) },
	"secret":   func(a []string, o, e io.Writer) int { return secretCmd(a, o, e) },
	"limit":    func(a []string, _, _ io.Writer) int { return limitCmd(a) },
	"quota":    func(a []string, _, _ io.Writer) int { return quotaCmd(a) },
	"cost":     func(a []string, _, _ io.Writer) int { return costCmd(a) },
	"usage":    func(a []string, _, _ io.Writer) int { return usageCmd(a) },
}

// ctlMain runs `gwag ctl <noun> args...`.
func ctlMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, ctlUsage)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, ctlUsage)
		return 0
	}
	h, ok := ctlNouns[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "gwag ctl: unknown noun %q\n\n%s\n", args[0], ctlUsage)
		return 2
	}
	return h(args[1:], stdout, stderr)
}

// servicesDispatch handles `gwag ctl services`: gateway-side verbs
// (list, add, remove, profiles) and the per-service profile verbs
// (init, check, wait, schema).
func servicesDispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "init", "check", "wait", "schema":
			return runService(args, stdout, stderr)
		}
	}
	return servicesCmd(args)
}

// legacyAliases maps pre-ctl top-level names to their ctl noun. Hidden
// from help; removed in the release after next.
var legacyAliases = map[string]string{
	"peer": "peer", "cluster": "cluster", "services": "services",
	"schema": "schema", "sign": "sign", "drain": "drain", "key": "key",
	"limit": "limit", "quota": "quota", "cost": "cost", "usage": "usage",
	"secret": "secret",
}

// legacyServiceVerbs are the old `gwag service <verb>` profile commands,
// now `gwag ctl services <verb>`.
var legacyServiceVerbs = []string{"init", "check", "wait", "schema"}

const legacyServiceUsage = `Usage: gwag service init|check|wait|schema   (moved to gwag ctl services)`

// legacyMain handles an old command name. ok is false when name is not
// an alias. Prints one stderr line, then runs the ctl handler with
// stdout and the exit code unchanged.
func legacyMain(name string, args []string, stdout, stderr io.Writer) (code int, ok bool) {
	if name == "service" {
		if len(args) > 0 {
			for _, v := range legacyServiceVerbs {
				if args[0] == v {
					warnMoved(stderr, "service "+v, "services "+v)
					return ctlNouns["services"](args, stdout, stderr), true
				}
			}
		}
		fmt.Fprintln(stderr, legacyServiceUsage)
		return 2, true
	}
	noun, isAlias := legacyAliases[name]
	if !isAlias {
		return 0, false
	}
	warnMoved(stderr, name, noun)
	return ctlNouns[noun](args, stdout, stderr), true
}

func warnMoved(stderr io.Writer, old, new string) {
	fmt.Fprintf(stderr, "gwag: %q moved to \"gwag ctl %s\"; the old name goes away in the next release\n", old, new)
}

// dispatchSubcommand runs a subcommand named by os.Args[1], if any.
func dispatchSubcommand(argv []string) (code int, ok bool) {
	if len(argv) == 0 {
		return 0, false
	}
	rest := argv[1:]
	switch argv[0] {
	case "up":
		return upCmd(rest), true
	case "serve":
		return serveCmd(rest), true
	case "login":
		return loginCmd(rest), true
	case "logout":
		return logoutCmd(rest), true
	case "use":
		return useCmd(rest), true
	case "context":
		return contextCmd(rest), true
	case "gen":
		return genCmd(rest, os.Stdout, os.Stderr), true
	case "deploy":
		return deployCmd(rest, os.Stdout, os.Stderr), true
	case "probe":
		return probeCmd(rest), true
	case "ctl":
		return ctlMain(rest, os.Stdout, os.Stderr), true
	case "service":
		return serviceMain(rest, os.Stdout, os.Stderr), true
	case "help":
		fmt.Fprintln(os.Stdout, topUsage)
		return 0, true
	}
	return legacyMain(argv[0], rest, os.Stdout, os.Stderr)
}

// legacyNames lists every alias name, sorted (tests).
func legacyNames() []string {
	n := []string{"service"}
	for k := range legacyAliases {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
