// gwag deploy: run a gwag cluster on Docker Compose or minikube with
// one command, or export deployment files for Compose, Kubernetes,
// minikube and ECS. File generation lives in gw/deploy; this file is
// flags, the platform CLIs, and the login hand-off.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/gwag/v2/gw/deploy"
)

const deployUsage = `Usage: gwag deploy <command> [flags]

Commands:
  up       export + apply: build the image, write files, start the cluster
  down     stop it (add --purge to delete the data volumes too)
  status   show the nodes and their /api/health
  login    save the admin token and addresses as a login in .gw/credentials.json
  export   write deployment files only (compose, k8s, minikube, ecs)

up, down, status and login take --platform docker-compose|minikube.
Run 'gwag deploy <command> -h' for flags.

--replicas is 1 (default) or an odd number >= 3. A 1-node deploy cannot
grow into a cluster later except through the workaround in
docs/cluster.md ("Single node to three"): the data does not carry
over. Start with 3 if you will need 3.
`

func deployCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, deployUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	d := &deployer{out: stdout, err: stderr}
	switch args[0] {
	case "up":
		return d.run("up", args[1:], d.up)
	case "down":
		return d.run("down", args[1:], d.down)
	case "status":
		return d.run("status", args[1:], d.status)
	case "login":
		return d.run("login", args[1:], d.login)
	case "export":
		return d.run("export", args[1:], d.export)
	}
	fmt.Fprintf(stderr, "unknown deploy command %q\n\n%s", args[0], deployUsage)
	return 2
}

type deployer struct {
	out, err io.Writer

	platform   string
	replicas   int
	dir        string
	image      string
	bind       string
	httpPort   int
	cpPort     int
	natsPort   int
	namespace  string
	storage    string
	src        string
	noBuild    bool
	purge      bool
	loginName  string
	outDir     string
	exportOnly bool
}

func (d *deployer) run(name string, args []string, fn func() int) int {
	fs := flag.NewFlagSet("deploy "+name, flag.ContinueOnError)
	fs.SetOutput(d.err)
	plat := "platform: docker-compose or minikube"
	if name == "export" {
		plat = "platform: compose (alias docker-compose), k8s, minikube or ecs"
	}
	fs.StringVar(&d.platform, "platform", "", plat)
	if name != "down" && name != "status" && name != "login" {
		fs.IntVar(&d.replicas, "replicas", 1, "number of nodes: 1, or odd >= 3. 2 and other even numbers work with a warning (no fault tolerance). A 1-node deploy cannot grow to a cluster later except via docs/cluster.md \"Single node to three\"")
		fs.StringVar(&d.image, "image", deploy.DefaultImage, "image reference. Default is the local tag that 'deploy up' builds; ECS needs a registry reference")
		fs.StringVar(&d.bind, "bind", "127.0.0.1", "compose: host address the ports are published on")
		fs.IntVar(&d.httpPort, "http-port", deploy.HTTPPort, "compose: host HTTP port of node 1 (node N gets +N-1)")
		fs.IntVar(&d.cpPort, "cp-port", deploy.CPPort, "compose: host control-plane port of node 1 (node N gets +N-1)")
		fs.IntVar(&d.natsPort, "nats-port", deploy.NATSClient, "compose: host NATS client port of node 1 (node N gets +N-1)")
		fs.StringVar(&d.namespace, "namespace", "gwag", "k8s and minikube: namespace")
		fs.StringVar(&d.storage, "storage", "1Gi", "k8s and minikube: volume size per node")
	}
	if name == "export" {
		fs.StringVar(&d.outDir, "out", "", "output directory (required)")
	} else {
		fs.StringVar(&d.dir, "dir", "gwag-deploy", "directory holding the generated files and secrets/")
	}
	switch name {
	case "up":
		fs.StringVar(&d.src, "src", ".", "gwag checkout holding the Dockerfile; the image is built from it when found")
		fs.BoolVar(&d.noBuild, "no-build", false, "do not build the image; use --image as it is")
	case "down":
		fs.BoolVar(&d.purge, "purge", false, "also delete the data volumes (the cluster state and registry are lost)")
		fs.StringVar(&d.namespace, "namespace", "gwag", "minikube: namespace")
	case "status":
		fs.StringVar(&d.namespace, "namespace", "gwag", "minikube: namespace")
	case "login":
		fs.StringVar(&d.loginName, "name", "gwag", "login name in .gw/credentials.json")
		fs.StringVar(&d.namespace, "namespace", "gwag", "minikube: namespace")
	}
	fs.Usage = func() {
		fmt.Fprintf(d.err, "Usage: gwag deploy %s --platform P [flags]\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if d.platform == "" {
		fs.Usage()
		return 2
	}
	if d.platform == "docker-compose" {
		d.platform = deploy.Compose
	}
	if name != "export" && d.platform != deploy.Compose && d.platform != deploy.Minikube {
		fmt.Fprintf(d.err, "--platform %q: deploy %s supports docker-compose and minikube. Use 'gwag deploy export' for k8s and ecs\n", d.platform, name)
		return 2
	}
	return fn()
}

func (d *deployer) spec() (deploy.Spec, int) {
	warns, err := deploy.ValidateReplicas(d.replicas)
	if err != nil {
		fmt.Fprintln(d.err, err)
		return deploy.Spec{}, 2
	}
	for _, w := range warns {
		fmt.Fprintln(d.err, "warning:", w)
	}
	return deploy.Spec{
		Platform: d.platform, Replicas: d.replicas, Image: d.image,
		Bind: d.bind, HTTPBase: d.httpPort, CPBase: d.cpPort, NATSBase: d.natsPort,
		Namespace: d.namespace, Storage: d.storage,
	}, 0
}

// deployMeta is what login and status need to find a compose
// deployment again. Not secret.
type deployMeta struct {
	Platform string `json:"platform"`
	Replicas int    `json:"replicas"`
	Bind     string `json:"bind"`
	HTTPBase int    `json:"http_port"`
	CPBase   int    `json:"cp_port"`
}

const metaFile = ".gwag-deploy.json"

// write generates the files into dir. The admin token is created once
// (dir/secrets/admin-token) and reused by later exports, so a re-apply
// does not rotate it.
func (d *deployer) write(dir string) (deploy.Spec, int) {
	spec, rc := d.spec()
	if rc != 0 {
		return spec, rc
	}
	tok, err := ensureToken(dir)
	if err != nil {
		fmt.Fprintln(d.err, err)
		return spec, 1
	}
	spec.Token = tok
	files, err := deploy.Generate(spec)
	if err != nil {
		fmt.Fprintln(d.err, err)
		return spec, 1
	}
	for _, f := range files {
		p := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			fmt.Fprintln(d.err, err)
			return spec, 1
		}
		if err := os.WriteFile(p, f.Data, os.FileMode(f.Mode)); err != nil {
			fmt.Fprintln(d.err, err)
			return spec, 1
		}
		fmt.Fprintln(d.out, "wrote", p)
	}
	n := spec.Normalize()
	meta, _ := json.Marshal(deployMeta{Platform: spec.Platform, Replicas: spec.Replicas, Bind: n.Bind, HTTPBase: n.HTTPBase, CPBase: n.CPBase})
	if err := os.WriteFile(filepath.Join(dir, metaFile), append(meta, '\n'), 0o644); err != nil {
		fmt.Fprintln(d.err, err)
		return spec, 1
	}
	return spec, 0
}

func ensureToken(dir string) (string, error) {
	path := filepath.Join(dir, "secrets", "admin-token")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

func readToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "secrets", "admin-token"))
	if err != nil {
		return "", fmt.Errorf("no admin token in %s: run 'gwag deploy up' first (%w)", dir, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (d *deployer) export() int {
	if d.outDir == "" {
		fmt.Fprintln(d.err, "--out is required")
		return 2
	}
	if d.platform == "docker-compose" {
		d.platform = deploy.Compose
	}
	if _, rc := d.write(d.outDir); rc != 0 {
		return rc
	}
	fmt.Fprintln(d.out, d.nextSteps())
	return 0
}

func (d *deployer) nextSteps() string {
	switch d.platform {
	case deploy.Compose:
		return fmt.Sprintf("next: docker compose -f %s/docker-compose.yaml up -d", d.outDir)
	case deploy.K8s, deploy.Minikube:
		return fmt.Sprintf("next: kubectl apply -f %[1]s/namespace.yaml -f %[1]s/secrets/secret.yaml -f %[1]s/gwag.yaml", d.outDir)
	case deploy.ECS:
		return fmt.Sprintf("next: aws cloudformation deploy --template-file %s/cloudformation.json --stack-name gwag --capabilities CAPABILITY_IAM --parameter-overrides VpcId=... SubnetA=... SubnetB=...", d.outDir)
	}
	return ""
}

// need fails early with a clear message when a platform CLI is missing.
func (d *deployer) need(bins ...string) bool {
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			fmt.Fprintf(d.err, "%s not found in PATH: %s needs it installed\n", b, "gwag deploy --platform "+d.platform)
			return false
		}
	}
	return true
}

func (d *deployer) sh(name string, args ...string) error {
	fmt.Fprintf(d.out, "+ %s %s\n", name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = d.out, d.err
	return c.Run()
}

func (d *deployer) shOut(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Stderr = d.err
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

func (d *deployer) tools() bool {
	if d.platform == deploy.Compose {
		if !d.need("docker") {
			return false
		}
		if err := exec.Command("docker", "compose", "version").Run(); err != nil {
			fmt.Fprintln(d.err, "'docker compose' (v2 plugin) not available")
			return false
		}
		return true
	}
	return d.need("minikube", "kubectl")
}

func (d *deployer) up() int {
	if !d.tools() {
		return 1
	}
	start := time.Now()
	if !d.noBuild {
		if _, err := os.Stat(filepath.Join(d.src, "Dockerfile")); err == nil {
			if !d.need("docker") {
				return 1
			}
			if err := d.sh("docker", "build", "-t", d.image, d.src); err != nil {
				fmt.Fprintln(d.err, "image build failed:", err)
				return 1
			}
		} else {
			fmt.Fprintf(d.err, "no Dockerfile in %s: using image %s as it is (pass --src to a gwag checkout to build)\n", d.src, d.image)
		}
	}
	d.outDir = d.dir
	spec, rc := d.write(d.dir)
	if rc != 0 {
		return rc
	}
	built := time.Since(start)
	switch d.platform {
	case deploy.Compose:
		if err := d.sh("docker", "compose", "-f", filepath.Join(d.dir, "docker-compose.yaml"), "up", "-d"); err != nil {
			return 1
		}
		n := spec.Normalize()
		deadline := time.Now().Add(5 * time.Minute)
		for i := 0; i < spec.Replicas; i++ {
			url := fmt.Sprintf("http://localhost:%d/api/health", n.HTTPBase+i)
			if !waitHealthy(url, deadline) {
				fmt.Fprintf(d.err, "node %d did not report 200 at %s in time\n", i+1, url)
				return 1
			}
		}
		fmt.Fprintf(d.out, "healthy: %d node(s) after %s (build+write %s)\nnext: gwag deploy login --platform docker-compose --dir %s\n", spec.Replicas, time.Since(start).Round(100*time.Millisecond), built.Round(100*time.Millisecond), d.dir)
	case deploy.Minikube:
		if err := d.sh("minikube", "image", "load", d.image); err != nil {
			return 1
		}
		for _, f := range []string{"namespace.yaml", "secrets/secret.yaml", "gwag.yaml"} {
			if err := d.sh("kubectl", "--context", "minikube", "apply", "-f", filepath.Join(d.dir, f)); err != nil {
				return 1
			}
		}
		if err := d.sh("kubectl", "--context", "minikube", "-n", d.namespace, "rollout", "status", "statefulset/gwag", "--timeout=10m"); err != nil {
			return 1
		}
		fmt.Fprintf(d.out, "ready: %d node(s) after %s\nnext: gwag deploy login --platform minikube --dir %s\n", spec.Replicas, time.Since(start).Round(time.Second), d.dir)
	}
	return 0
}

func waitHealthy(url string, deadline time.Time) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if resp, err := c.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func (d *deployer) down() int {
	if !d.tools() {
		return 1
	}
	switch d.platform {
	case deploy.Compose:
		args := []string{"compose", "-f", filepath.Join(d.dir, "docker-compose.yaml"), "down"}
		if d.purge {
			args = append(args, "--volumes")
		}
		if d.sh("docker", args...) != nil {
			return 1
		}
	case deploy.Minikube:
		if d.sh("kubectl", "--context", "minikube", "-n", d.namespace, "delete", "-f", filepath.Join(d.dir, "gwag.yaml"), "--ignore-not-found") != nil {
			return 1
		}
		if d.purge {
			if d.sh("kubectl", "--context", "minikube", "-n", d.namespace, "delete", "pvc", "-l", "app.kubernetes.io/name=gwag") != nil {
				return 1
			}
		}
	}
	if !d.purge {
		fmt.Fprintln(d.out, "data volumes kept; add --purge to delete them")
	}
	return 0
}

func (d *deployer) loadMeta() deployMeta {
	var m deployMeta
	if b, err := os.ReadFile(filepath.Join(d.dir, metaFile)); err == nil {
		json.Unmarshal(b, &m)
	}
	if m.Replicas == 0 {
		m.Replicas = 1
	}
	if m.HTTPBase == 0 {
		m.HTTPBase = deploy.HTTPPort
	}
	if m.CPBase == 0 {
		m.CPBase = deploy.CPPort
	}
	return m
}

func (d *deployer) status() int {
	if !d.tools() {
		return 1
	}
	switch d.platform {
	case deploy.Compose:
		if d.sh("docker", "compose", "-f", filepath.Join(d.dir, "docker-compose.yaml"), "ps") != nil {
			return 1
		}
		m := d.loadMeta()
		c := &http.Client{Timeout: 2 * time.Second}
		for i := 0; i < m.Replicas; i++ {
			url := fmt.Sprintf("http://localhost:%d/api/health", m.HTTPBase+i)
			resp, err := c.Get(url)
			if err != nil {
				fmt.Fprintf(d.out, "gwag-%d  %s  unreachable\n", i+1, url)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Fprintf(d.out, "gwag-%d  %s  %d %s\n", i+1, url, resp.StatusCode, strings.TrimSpace(string(b)))
		}
	case deploy.Minikube:
		return boolRC(d.sh("kubectl", "--context", "minikube", "-n", d.namespace, "get", "statefulset,pods,pvc", "-l", "app.kubernetes.io/name=gwag"))
	}
	return 0
}

func boolRC(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

func (d *deployer) login() int {
	var entry loginEntry
	switch d.platform {
	case deploy.Compose:
		tok, err := readToken(d.dir)
		if err != nil {
			fmt.Fprintln(d.err, err)
			return 1
		}
		m := d.loadMeta()
		host := m.Bind
		if host == "" || host == "0.0.0.0" {
			host = "localhost"
		}
		entry = loginEntry{Gateway: fmt.Sprintf("%s:%d", host, m.CPBase), Endpoint: fmt.Sprintf("http://%s:%d", host, m.HTTPBase), Bearer: tok}
	case deploy.Minikube:
		if !d.tools() {
			return 1
		}
		b64, err := d.shOut("kubectl", "--context", "minikube", "-n", d.namespace, "get", "secret", "gwag-admin", "-o", "jsonpath={.data.token}")
		if err != nil {
			fmt.Fprintln(d.err, "read secret gwag-admin:", err)
			return 1
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			fmt.Fprintln(d.err, "secret gwag-admin:", err)
			return 1
		}
		ip, err := d.shOut("minikube", "ip")
		if err != nil {
			fmt.Fprintln(d.err, "minikube ip:", err)
			return 1
		}
		entry = loginEntry{Gateway: ip + ":30090", Endpoint: "http://" + ip + ":30080", Bearer: strings.TrimSpace(string(raw))}
	}
	entry.Name = d.loginName
	creds, err := loadCredentialsAt(credentialsFile)
	if err != nil {
		fmt.Fprintln(d.err, "load credentials:", err)
		return 1
	}
	creds.upsert(entry)
	creds.setPrimary(entry.Name)
	if err := saveCredentialsAt(credentialsFile, creds); err != nil {
		fmt.Fprintln(d.err, "save credentials:", err)
		return 1
	}
	abs, _ := filepath.Abs(credentialsFile)
	fmt.Fprintf(d.out, "login %q (primary) in %s\n  gateway:  %s\n  endpoint: %s\n", entry.Name, abs, entry.Gateway, entry.Endpoint)
	return 0
}
