package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/gw/eventgen"
)

// Registry credentials come from the environment, never from flags (a
// flag shows in `ps`) or the lock file.
const (
	envRegistryUser  = "GWAG_REGISTRY_USERNAME"
	envRegistryPass  = "GWAG_REGISTRY_PASSWORD"
	envRegistryToken = "GWAG_REGISTRY_TOKEN"
	envRegistryCA    = "GWAG_REGISTRY_CA"
)

const registryEnvHelp = "Auth: URL user info, or " + envRegistryUser + "/" + envRegistryPass + " (basic), or " +
	envRegistryToken + " (bearer). " + envRegistryCA + "=FILE adds a CA for HTTPS."

func registryClient(rawURL string) (*sr.Client, error) {
	cfg := sr.Config{
		URL:      rawURL,
		Username: os.Getenv(envRegistryUser),
		Password: os.Getenv(envRegistryPass),
		Token:    os.Getenv(envRegistryToken),
	}
	if ca := os.Getenv(envRegistryCA); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s=%s: no certificates", envRegistryCA, ca)
		}
		cfg.TLS = &tls.Config{RootCAs: pool}
	}
	return sr.NewClient(cfg)
}

// lockFile pins imported registry schemas. `gwag gen` writes it;
// `gwag gen --check-lock` compares it with the registry and the files
// on disk.
type lockFile struct {
	// Registry is the registry URL without user info.
	Registry string        `json:"registry"`
	Subjects []lockSubject `json:"subjects"`
}

type lockSubject struct {
	Subject string `json:"subject"`
	Version int    `json:"version"`
	ID      int    `json:"id"`
	// Path is the root file under the import directory.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// Message is the record message; Name and Pattern bind its channel.
	Message    string    `json:"message"`
	Name       string    `json:"name"`
	Pattern    string    `json:"pattern"`
	References []lockRef `json:"references,omitempty"`
}

type lockRef struct {
	Path    string `json:"path"`
	Subject string `json:"subject"`
	Version int    `json:"version"`
	ID      int    `json:"id"`
	SHA256  string `json:"sha256"`
}

func readLock(path string) (*lockFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	l := &lockFile{}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

func writeLock(path string, l *lockFile) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// importedSource is the file gwag writes for a registry schema: a
// header naming its origin, then the registry's text.
func importedSource(registry string, f sr.TreeFile) string {
	return fmt.Sprintf("// Imported from %s subject %s version %d id %d; do not edit.\n// Re-import with gwag gen --registry; gwag gen --check-lock checks it.\n\n%s",
		registry, f.Subject, f.Version, f.ID, strings.TrimLeft(f.Source, "\n"))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var subjectFileRE = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// subjectPath is the file a subject's root schema is written to.
func subjectPath(subject string) string {
	return "registry/" + subjectFileRE.ReplaceAllString(subject, "_") + ".proto"
}

// topicOf is the topic of a TopicNameStrategy subject.
func topicOf(subject string) (string, bool) {
	for _, suf := range []string{"-value", "-key"} {
		if t, ok := strings.CutSuffix(subject, suf); ok && t != "" {
			return t, true
		}
	}
	return "", false
}

var topicTokenRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// patternOf turns a topic into a literal pattern: "." splits tokens,
// other characters a subject token cannot hold become "_".
func patternOf(topic string) string {
	parts := strings.Split(topic, ".")
	for i, p := range parts {
		p = topicTokenRE.ReplaceAllString(p, "_")
		if p == "" {
			p = "_"
		}
		parts[i] = p
	}
	return strings.Join(parts, ".")
}

// importSpec is one --subject with its --bind overrides.
type importSpec struct {
	subject, version       string
	message, name, pattern string
}

// parseSubject parses SUBJECT[@VERSION].
func parseSubject(v string) importSpec {
	s := importSpec{subject: v, version: "latest"}
	if i := strings.LastIndex(v, "@"); i > 0 {
		s.subject, s.version = v[:i], v[i+1:]
	}
	return s
}

// parseBind parses SUBJECT=MESSAGE[,name=N][,pattern=P]; MESSAGE may be
// empty ("orders-value=,pattern=orders").
func parseBind(v string, specs []importSpec) error {
	subj, rest, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--bind %q: want SUBJECT=MESSAGE[,name=N][,pattern=P]", v)
	}
	for i := range specs {
		if specs[i].subject != subj {
			continue
		}
		parts := strings.Split(rest, ",")
		specs[i].message = parts[0]
		for _, p := range parts[1:] {
			k, val, _ := strings.Cut(p, "=")
			switch k {
			case "name":
				specs[i].name = val
			case "pattern":
				specs[i].pattern = val
			default:
				return fmt.Errorf("--bind %q: unknown key %q (want name or pattern)", v, k)
			}
		}
		return nil
	}
	return fmt.Errorf("--bind %q: no --subject %s", v, subj)
}

// importSchemas fetches specs, writes the read-only copies under dir
// and returns the lock. A spec version "latest" is pinned to the
// number the registry returns.
func importSchemas(ctx context.Context, c *sr.Client, dir string, specs []importSpec) (*lockFile, error) {
	l := &lockFile{Registry: c.URL()}
	for _, sp := range specs {
		v, err := c.SubjectVersion(ctx, sp.subject, sp.version, "")
		if err != nil {
			return nil, fmt.Errorf("subject %s version %s: %w", sp.subject, sp.version, err)
		}
		if v.SchemaType != "" && v.SchemaType != "PROTOBUF" {
			return nil, fmt.Errorf("subject %s is %s, not PROTOBUF", sp.subject, v.SchemaType)
		}
		if v.Subject == "" {
			v.Subject = sp.subject
		}
		root := subjectPath(sp.subject)
		tree, err := c.Tree(ctx, v, root)
		if err != nil {
			return nil, fmt.Errorf("subject %s: %w", sp.subject, err)
		}
		fd, err := tree.Compile(ctx)
		if err != nil {
			return nil, fmt.Errorf("subject %s: %w", sp.subject, err)
		}
		ls := lockSubject{Subject: sp.subject, Version: v.Version, ID: v.ID, Path: root}
		ls.Message = sp.message
		if ls.Message == "" {
			if fd.Messages().Len() == 0 {
				return nil, fmt.Errorf("subject %s: schema has no message", sp.subject)
			}
			ls.Message = string(fd.Messages().Get(0).FullName())
		}
		md := sr.FindMessage(fd, protoreflect.FullName(ls.Message))
		if md == nil {
			return nil, fmt.Errorf("subject %s: no message %s in the schema", sp.subject, ls.Message)
		}
		ls.Name = sp.name
		if ls.Name == "" {
			ls.Name = string(md.Name())
		}
		ls.Pattern = sp.pattern
		if ls.Pattern == "" {
			topic, ok := topicOf(sp.subject)
			if !ok {
				return nil, fmt.Errorf("subject %s is not <topic>-value; give the pattern with --bind %s=%s,pattern=P", sp.subject, sp.subject, ls.Message)
			}
			ls.Pattern = patternOf(topic)
		}
		for _, f := range tree.Files {
			src := importedSource(l.Registry, f)
			p := filepath.Join(dir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			// Read-only: the header says do not edit; the mode says it too.
			_ = os.Chmod(p, 0o644)
			if err := os.WriteFile(p, []byte(src), 0o444); err != nil {
				return nil, err
			}
			if err := os.Chmod(p, 0o444); err != nil {
				return nil, err
			}
			if f.Path == root {
				ls.SHA256 = sha(src)
			} else {
				ls.References = append(ls.References, lockRef{Path: f.Path, Subject: f.Subject, Version: f.Version, ID: f.ID, SHA256: sha(src)})
			}
		}
		l.Subjects = append(l.Subjects, ls)
	}
	sort.SliceStable(l.Subjects, func(i, j int) bool { return l.Subjects[i].Subject < l.Subjects[j].Subject })
	return l, nil
}

// checkLock compares l with the registry and the files under dir and
// returns one line per difference.
func checkLock(ctx context.Context, c *sr.Client, dir string, l *lockFile, notes io.Writer) ([]string, error) {
	var bad []string
	check := func(path, subject string, version, id int, want string) error {
		v, err := c.SubjectVersion(ctx, subject, strconv.Itoa(version), "")
		if err != nil {
			if sr.IsNotFound(err) {
				bad = append(bad, fmt.Sprintf("%s: subject %s version %d is gone from the registry", path, subject, version))
				return nil
			}
			return err
		}
		if v.ID != id {
			bad = append(bad, fmt.Sprintf("%s: subject %s version %d has id %d, lock has %d", path, subject, version, v.ID, id))
		}
		reg := sha(importedSource(l.Registry, sr.TreeFile{Path: path, Source: v.Schema, Subject: subject, Version: version, ID: v.ID}))
		if reg != want {
			bad = append(bad, fmt.Sprintf("%s: subject %s version %d differs from the lock (sha256 %s, lock %s)", path, subject, version, reg[:12], short(want)))
		}
		disk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", path, err))
		case sha(string(disk)) != want:
			bad = append(bad, fmt.Sprintf("%s: file was edited (sha256 %s, lock %s)", path, sha(string(disk))[:12], short(want)))
		}
		return nil
	}
	for _, s := range l.Subjects {
		if err := check(s.Path, s.Subject, s.Version, s.ID, s.SHA256); err != nil {
			return nil, err
		}
		for _, r := range s.References {
			if err := check(r.Path, r.Subject, r.Version, r.ID, r.SHA256); err != nil {
				return nil, err
			}
		}
		if latest, err := c.SubjectVersion(ctx, s.Subject, "latest", ""); err == nil && latest.Version != s.Version {
			// Not a failure: the lock pins on purpose.
			fmt.Fprintf(notes, "note: %s: version %d is locked; the registry's latest is %d\n", s.Subject, s.Version, latest.Version)
		}
	}
	return bad, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// lockBindings returns the channel bindings of l and every file it
// imported (references first), to generate.
func lockBindings(l *lockFile) (map[string]eventgen.Binding, []string) {
	b := map[string]eventgen.Binding{}
	var files []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, s := range l.Subjects {
		b[s.Message] = eventgen.Binding{Name: s.Name, Pattern: s.Pattern}
		for _, r := range s.References {
			add(r.Path)
		}
		add(s.Path)
	}
	return b, files
}
