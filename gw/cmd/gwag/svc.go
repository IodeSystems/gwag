package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gwag service: run the gateway as a per-user OS service (systemd user
// unit on Linux, LaunchAgent on macOS). The old profile verbs
// (init|check|wait|schema) still win and forward to `gwag ctl services`.

const svcUsage = `Usage: gwag service <verb> [--name NAME]

  install [--exec PATH] [--data-dir DIR] [--env K=V]... [--enable] [--start] [-- UP-FLAGS...]
            write the unit (systemd user unit / LaunchAgent); args after -- go to gwag up
  uninstall stop, disable, remove the unit; the data dir stays
  start | stop | restart | status
  logs [-f] last 200 lines (journalctl --user / the log file); -f follows
  login [--data-dir DIR] [--primary]
            save a credentials context named NAME for this service's admin address

NAME defaults to "gwag".`

var svcDaemonVerbs = []string{"install", "uninstall", "start", "stop", "restart", "status", "logs", "login"}

var svcNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

// svcRunner runs an external command with its output on out / errw.
type svcRunner func(out, errw io.Writer, name string, args ...string) error

// svcEnv is everything the verbs touch outside the process: tests
// replace it.
type svcEnv struct {
	out, err io.Writer
	goos     string
	uid      int
	home     string
	getenv   func(string) string
	exe      func() (string, error)
	run      svcRunner
	sleep    func(time.Duration)
}

func defaultSvcEnv(out, errw io.Writer) *svcEnv {
	home, _ := os.UserHomeDir()
	return &svcEnv{
		out: out, err: errw, goos: runtime.GOOS, uid: os.Getuid(), home: home,
		getenv: os.Getenv, exe: os.Executable, sleep: time.Sleep,
		run: func(o, e io.Writer, name string, args ...string) error {
			c := exec.Command(name, args...)
			c.Stdout, c.Stderr = o, e
			c.Stdin = os.Stdin
			return c.Run()
		},
	}
}

func (e *svcEnv) darwin() bool { return e.goos == "darwin" }

func (e *svcEnv) unitDir() string   { return filepath.Join(e.home, ".config", "systemd", "user") }
func (e *svcEnv) agentsDir() string { return filepath.Join(e.home, "Library", "LaunchAgents") }
func (e *svcEnv) logPath(name string) string {
	return filepath.Join(e.home, "Library", "Logs", "gwag", name+".log")
}

func (e *svcEnv) defaultDataDir(name string) string {
	if x := e.getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "gwag", name)
	}
	return filepath.Join(e.home, ".local", "share", "gwag", name)
}

func (e *svcEnv) defPath(name string) string {
	u := svcUnit{Name: name}
	if e.darwin() {
		return filepath.Join(e.agentsDir(), u.plistFileName())
	}
	return filepath.Join(e.unitDir(), u.unitFileName())
}

func (e *svcEnv) systemctl(args ...string) error {
	return e.run(e.out, e.err, "systemctl", append([]string{"--user"}, args...)...)
}

func (e *svcEnv) hasUserSystemd() bool {
	return e.run(io.Discard, io.Discard, "systemctl", "--user", "show-environment") == nil
}

func (e *svcEnv) domain() string { return "gui/" + strconv.Itoa(e.uid) }
func (e *svcEnv) target(name string) string {
	return e.domain() + "/" + svcUnit{Name: name}.label()
}
func (e *svcEnv) loaded(name string) bool {
	return e.run(io.Discard, io.Discard, "launchctl", "print", e.target(name)) == nil
}

// serviceMain handles `gwag service ...`: legacy profile verbs first.
func serviceMain(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if slices.Contains(legacyServiceVerbs, args[0]) {
			code, _ := legacyMain("service", args, stdout, stderr)
			return code
		}
		if slices.Contains(svcDaemonVerbs, args[0]) {
			return defaultSvcEnv(stdout, stderr).dispatch(args[0], args[1:])
		}
	}
	fmt.Fprintln(stderr, svcUsage)
	return 2
}

func (e *svcEnv) dispatch(verb string, args []string) int {
	var err error
	switch verb {
	case "install":
		err = e.install(args)
	case "uninstall":
		err = e.uninstall(args)
	case "start", "stop", "restart", "status":
		err = e.control(verb, args)
	case "logs":
		err = e.logs(args)
	case "login":
		err = e.login(args)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	}
	fmt.Fprintf(e.err, "gwag service %s: %v\n", verb, err)
	return 1
}

var errUsage = errors.New("usage")

func (e *svcEnv) flags(verb string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("service "+verb, flag.ContinueOnError)
	fs.SetOutput(e.err)
	name := fs.String("name", svcDefaultName, "service name")
	return fs, name
}

func (e *svcEnv) parse(fs *flag.FlagSet, name *string, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.err, "unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	if !svcNameRE.MatchString(*name) {
		return fmt.Errorf("invalid --name %q", *name)
	}
	return nil
}

func (e *svcEnv) refuseRoot() error {
	if e.darwin() && e.uid == 0 {
		return errors.New("run as your own user, not root: the LaunchAgent is per-user (gui/<uid>, ~/Library/LaunchAgents)")
	}
	return nil
}

func (e *svcEnv) install(args []string) error {
	var own, up []string
	if i := slices.Index(args, "--"); i >= 0 {
		own, up = args[:i], args[i+1:]
	} else {
		own = args
	}
	fs, name := e.flags("install")
	execBin := fs.String("exec", "", "absolute path of gwag (default: this binary, symlinks resolved)")
	dataDir := fs.String("data-dir", "", "state directory (default $XDG_DATA_HOME/gwag/NAME or ~/.local/share/gwag/NAME)")
	var envs multiFlag
	fs.Var(&envs, "env", "KEY=VALUE for the service (repeatable)")
	enable := fs.Bool("enable", false, "enable at login")
	start := fs.Bool("start", false, "start (or restart) now")
	if err := e.parse(fs, name, own); err != nil {
		return err
	}
	if err := e.refuseRoot(); err != nil {
		return err
	}
	for _, kv := range envs {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return fmt.Errorf("--env %q: want KEY=VALUE", kv)
		}
	}
	bin := *execBin
	if bin == "" {
		p, err := e.exe()
		if err != nil {
			return fmt.Errorf("locate this binary (pass --exec): %w", err)
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		bin = p
	}
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("--exec %q must be absolute", bin)
	}
	dir := *dataDir
	if v := flagValue(up, "--data-dir"); v != "" {
		dir = v // the passthrough flag wins; keep one data dir
	}
	if dir == "" {
		dir = e.defaultDataDir(*name)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// --data-dir is carried in the unit once; drop it from the passthrough.
	up = stripFlag(up, "--data-dir")
	u := svcUnit{Name: *name, Exec: bin, DataDir: dir, Args: up, Env: envs, StopTimeout: svcStopTimeout(up)}
	if e.darwin() && !slices.ContainsFunc(u.Env, func(s string) bool { return strings.HasPrefix(s, "PATH=") }) {
		// launchd starts with a minimal PATH, not the login shell's.
		u.Env = append(u.Env, "PATH="+e.getenv("PATH"))
	}
	path := e.defPath(*name)
	var content string
	if e.darwin() {
		content = u.renderPlist(e.logPath(*name))
		if err := os.MkdirAll(filepath.Dir(e.logPath(*name)), 0o755); err != nil {
			return err
		}
	} else {
		content = u.renderSystemd()
	}
	if err := writeFileAtomic(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "wrote %s\n  exec     %s\n  data-dir %s\n  up       %s\n", path, bin, dir, strings.Join(u.upArgs()[1:], " "))

	if e.darwin() {
		fmt.Fprintf(e.out, "  logs     %s\n", e.logPath(*name))
		if *enable {
			if err := e.run(e.out, e.err, "launchctl", "enable", e.target(*name)); err != nil {
				return fmt.Errorf("enable: %w", err)
			}
			fmt.Fprintln(e.out, "enabled at login")
		}
		if *start {
			if err := e.launchdLoad(*name); err != nil {
				return err
			}
			fmt.Fprintln(e.out, "started")
		}
		return nil
	}
	if !e.hasUserSystemd() {
		fmt.Fprintln(e.out, "\nNOTE: no user systemd is reachable here; the unit is written but not loaded.")
		if *enable || *start {
			fmt.Fprintln(e.out, "--enable / --start were not applied.")
		}
		return nil
	}
	if err := e.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if *enable {
		if err := e.systemctl("enable", u.unitFileName()); err != nil {
			return fmt.Errorf("enable: %w", err)
		}
		fmt.Fprintln(e.out, "enabled at login")
	}
	if *start {
		if err := e.systemctl("restart", u.unitFileName()); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		fmt.Fprintln(e.out, "started")
	}
	return nil
}

// stripFlag removes "--name value" and "--name=value".
func stripFlag(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == name:
			i++
		case strings.HasPrefix(args[i], name+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

func (e *svcEnv) uninstall(args []string) error {
	fs, name := e.flags("uninstall")
	if err := e.parse(fs, name, args); err != nil {
		return err
	}
	if err := e.refuseRoot(); err != nil {
		return err
	}
	dataDir := e.installedDataDir(*name)
	path := e.defPath(*name)
	if e.darwin() {
		if e.loaded(*name) {
			_ = e.run(e.out, e.err, "launchctl", "bootout", e.target(*name))
		}
	} else if e.hasUserSystemd() {
		unit := svcUnit{Name: *name}.unitFileName()
		// A unit never started or enabled must not fail removal.
		_ = e.systemctl("stop", unit)
		_ = e.systemctl("disable", unit)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !e.darwin() && e.hasUserSystemd() {
		_ = e.systemctl("daemon-reload")
	}
	fmt.Fprintf(e.out, "removed %s\ndata kept in %s\n", path, dataDir)
	return nil
}

func (e *svcEnv) control(verb string, args []string) error {
	fs, name := e.flags(verb)
	if err := e.parse(fs, name, args); err != nil {
		return err
	}
	if e.darwin() {
		if err := e.refuseRoot(); err != nil {
			return err
		}
		return e.launchdCtl(verb, *name)
	}
	return e.systemctl(verb, svcUnit{Name: *name}.unitFileName())
}

func (e *svcEnv) logs(args []string) error {
	fs, name := e.flags("logs")
	follow := fs.Bool("f", false, "follow")
	if err := e.parse(fs, name, args); err != nil {
		return err
	}
	if e.darwin() {
		a := []string{"-n", "200"}
		if *follow {
			a = append(a, "-f")
		}
		return e.run(e.out, e.err, "tail", append(a, e.logPath(*name))...)
	}
	a := []string{"--user", "-u", *name, "-n", "200", "--no-pager"}
	if *follow {
		a = append(a, "-f")
	}
	return e.run(e.out, e.err, "journalctl", a...)
}

// launchdLoad (re)loads the plist: a loaded job keeps its old
// definition until booted out, so this is a restart when running.
func (e *svcEnv) launchdLoad(name string) error {
	if e.loaded(name) {
		fmt.Fprintln(e.out, "stopping...")
		if err := e.run(e.out, e.err, "launchctl", "bootout", e.target(name)); err != nil {
			return fmt.Errorf("bootout: %w", err)
		}
		// bootout can return while a draining job is still leaving.
		for deadline := time.Now().Add(150 * time.Second); e.loaded(name) && time.Now().Before(deadline); {
			e.sleep(250 * time.Millisecond)
		}
	}
	fmt.Fprintln(e.out, "starting...")
	var msg strings.Builder
	if err := e.run(e.out, &msg, "launchctl", "bootstrap", e.domain(), e.defPath(name)); err != nil {
		if e.loaded(name) {
			return nil
		}
		return fmt.Errorf("launchctl bootstrap %s %s: %s (%w)\nlog: %s", e.domain(), e.defPath(name), strings.TrimSpace(msg.String()), err, e.logPath(name))
	}
	return nil
}

func (e *svcEnv) launchdCtl(verb, name string) error {
	switch verb {
	case "start":
		if e.loaded(name) {
			return e.run(e.out, e.err, "launchctl", "kickstart", e.target(name))
		}
		return e.launchdLoad(name)
	case "stop":
		// bootout, not kill: KeepAlive would restart a failed exit.
		if !e.loaded(name) {
			return nil
		}
		return e.run(e.out, e.err, "launchctl", "bootout", e.target(name))
	case "restart":
		return e.launchdLoad(name) // picks up a reinstalled plist
	default:
		return e.run(e.out, e.err, "launchctl", "print", e.target(name))
	}
}

// installedArgs returns the `gwag up` arguments of an installed
// service (after the executable), from its unit or plist.
func (e *svcEnv) installedArgs(name string) ([]string, error) {
	b, err := os.ReadFile(e.defPath(name))
	if err != nil {
		return nil, fmt.Errorf("service %q is not installed (%s)", name, e.defPath(name))
	}
	var toks []string
	if e.darwin() {
		toks = plistArgs(string(b))
	} else {
		for _, l := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(l, "ExecStart="); ok {
				toks = sdSplit(rest)
			}
		}
	}
	if len(toks) < 2 {
		return nil, fmt.Errorf("no ExecStart in %s", e.defPath(name))
	}
	return toks[1:], nil
}

func (e *svcEnv) installedDataDir(name string) string {
	if a, err := e.installedArgs(name); err == nil {
		if d := flagValue(a, "--data-dir"); d != "" {
			return d
		}
	}
	return e.defaultDataDir(name)
}

// sdSplit undoes sdQuote.
func sdSplit(s string) []string {
	var out []string
	var cur strings.Builder
	in, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case in && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case in && c == '"':
			in = false
		case !in && c == '"':
			in, started = true, true
		case !in && c == ' ':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// plistArgs reads ProgramArguments back from a plist we wrote.
func plistArgs(p string) []string {
	_, rest, ok := strings.Cut(p, "<key>ProgramArguments</key>")
	if !ok {
		return nil
	}
	rest, _, _ = strings.Cut(rest, "</array>")
	un := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")
	var out []string
	for {
		_, after, ok := strings.Cut(rest, "<string>")
		if !ok {
			return out
		}
		v, next, _ := strings.Cut(after, "</string>")
		out = append(out, un.Replace(v))
		rest = next
	}
}

// listenHost turns a listen address into a dialable host:port.
func listenHost(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr
	}
	h, p := addr[:i], addr[i+1:]
	switch h {
	case "", "0.0.0.0", "::", "[::]":
		h = "localhost"
	}
	return h + ":" + p
}

func (e *svcEnv) login(args []string) error {
	fs, name := e.flags("login")
	dataDir := fs.String("data-dir", "", "data dir (default: the installed service's)")
	primary := fs.Bool("primary", false, "make this the primary login")
	if err := e.parse(fs, name, args); err != nil {
		return err
	}
	var upArgs []string
	if a, err := e.installedArgs(*name); err == nil {
		upArgs = a
	} else if *dataDir == "" {
		return err
	}
	dir := *dataDir
	if dir == "" {
		dir = flagValue(upArgs, "--data-dir")
	}
	tokPath := filepath.Join(dir, "admin-token")
	if f := flagValue(upArgs, "--admin-token-file"); f != "" {
		tokPath = f
	}
	raw, err := os.ReadFile(tokPath)
	if err != nil {
		return fmt.Errorf("read admin token: %w (has the service started once?)", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return fmt.Errorf("%s is empty", tokPath)
	}
	httpAddr, cpAddr := ":8080", ":50090"
	if v := flagValue(upArgs, "--addr"); v != "" {
		httpAddr = v
	}
	if v := flagValue(upArgs, "--control-plane"); v != "" {
		cpAddr = v
	}
	path, err := credentialsPath(true)
	if err != nil {
		return err
	}
	creds, err := loadCredentialsAt(path)
	if err != nil {
		return err
	}
	entry := loginEntry{
		Name:     *name,
		Primary:  *primary || len(creds.Logins) == 0,
		Gateway:  listenHost(cpAddr),
		Endpoint: "http://" + listenHost(httpAddr),
		Bearer:   tok,
	}
	if entry.Primary {
		for i := range creds.Logins {
			if creds.Logins[i].Name != *name {
				creds.Logins[i].Primary = false
			}
		}
	}
	creds.upsert(entry)
	if err := saveCredentialsAt(path, creds); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "login %q in %s\n  gateway:  %s\n  endpoint: %s\n  primary:  %v\n  bearer:   persisted\nuse it: gwag ctl services list --context %s\n",
		*name, path, entry.Gateway, entry.Endpoint, entry.Primary, *name)
	return nil
}
