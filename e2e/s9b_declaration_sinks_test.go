//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS9bDeclarationSinks: the books team wants its contract system
// to hear about every external-dependent change, and each gateway
// host to keep the current set on disk for a local audit job. Three
// gateways in a cluster, each with the same --declarations-hook and
// its own --declarations-file. Changes go to different nodes: put on
// n1, review on n2, delete on n3.
//
//   - hook delta + file full: one signed POST per change, from the
//     node that took the write; every node's file is the current set
//   - hook full + file delta: one POST per change carrying the whole
//     set; every node's file has one line per change, the same lines
//     (by seq) on every node
func TestS9bDeclarationSinks(t *testing.T) {
	const secret = "kT7vQ2mXp9LdR4sWc8NfH1bZy6JgAe3UoVi5MqBxD0Pn"
	type record struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ReviewAt   string `json:"reviewAt"`
		ReviewedBy string `json:"reviewedBy"`
	}
	type full struct {
		Schema       string   `json:"schema"`
		Seq          uint64   `json:"seq"`
		At           string   `json:"at"`
		Declarations []record `json:"declarations"`
	}
	type delta struct {
		Schema string  `json:"schema"`
		Seq    uint64  `json:"seq"`
		Op     string  `json:"op"`
		ID     string  `json:"id"`
		Before *record `json:"before"`
		After  *record `json:"after"`
		At     string  `json:"at"`
	}
	review1 := time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	review2 := time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	acme := harness.ExternalDependent{
		ID: "acme", Name: "Acme Corp", Kind: "customer",
		Depends:  []harness.ExternalPin{{Service: "library", Lines: []string{"v1"}}},
		ReviewAt: review1,
	}
	const fileName = "declarations.out"

	start := func(t *testing.T, hookMode, fileMode string) (*harness.Cluster, *harness.HookSink) {
		env := harness.New(t)
		sink := harness.NewHookSink(t)
		c := env.ClusterWithNodeArgs(3, harness.GatewayOpts{Args: []string{
			"--declarations-hook", sink.URL,
			"--declarations-hook-secret", secret,
			"--declarations-hook-mode", hookMode,
			"--declarations-file-mode", fileMode,
		}}, func(_, dataDir string) []string {
			return []string{"--declarations-file", filepath.Join(dataDir, fileName)}
		})
		return c, sink
	}
	// The three changes, one per node.
	type step struct {
		desc string
		do   func(t *testing.T, c *harness.Cluster)
	}
	steps := []step{
		{"put on n1", func(t *testing.T, c *harness.Cluster) {
			if _, err := c.Nodes[0].Admin().PutExternalDependent(harness.Ctx(t, 30*time.Second), acme); err != nil {
				t.Fatal(err)
			}
		}},
		{"review on n2", func(t *testing.T, c *harness.Cluster) {
			ctx := harness.Ctx(t, 30*time.Second)
			harness.Eventually(t, 15*time.Second, "acme on n2", func() error {
				l, err := c.Nodes[1].Admin().ExternalDependents(ctx)
				if err != nil || len(l) != 1 {
					return fmt.Errorf("%+v %v", l, err)
				}
				return nil
			})
			if _, err := c.Nodes[1].Admin().ReviewExternalDependent(ctx, "acme", review2, "books-oncall"); err != nil {
				t.Fatal(err)
			}
		}},
		{"delete on n3", func(t *testing.T, c *harness.Cluster) {
			ctx := harness.Ctx(t, 30*time.Second)
			harness.Eventually(t, 15*time.Second, "reviewed acme on n3", func() error {
				l, err := c.Nodes[2].Admin().ExternalDependents(ctx)
				if err != nil || len(l) != 1 || l[0].ReviewAt != review2 {
					return fmt.Errorf("%+v %v", l, err)
				}
				return nil
			})
			if err := c.Nodes[2].Admin().DeleteExternalDependent(ctx, "acme"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	ops := []string{"put", "review", "delete"}

	readFile := func(g *harness.Gateway) ([]byte, error) {
		return os.ReadFile(filepath.Join(g.DataDir, fileName))
	}

	t.Run("hook delta, file full", func(t *testing.T) {
		c, sink := start(t, "delta", "full")
		wantFile := func(t *testing.T, check func(f full) error) {
			t.Helper()
			for _, g := range c.Nodes {
				harness.Eventually(t, 15*time.Second, g.Name+" declarations file", func() error {
					b, err := readFile(g)
					if err != nil {
						return err
					}
					var f full
					if err := json.Unmarshal(b, &f); err != nil {
						return fmt.Errorf("%v: %s", err, b)
					}
					if f.Schema != "gwag.declarations.full/v1" {
						return fmt.Errorf("schema %q", f.Schema)
					}
					return check(f)
				})
				st, err := os.Stat(filepath.Join(g.DataDir, fileName))
				if err != nil || st.Mode().Perm() != 0o644 {
					t.Fatalf("%s file mode: %v %v", g.Name, st.Mode().Perm(), err)
				}
			}
		}
		// After the replay every node has written the empty set.
		wantFile(t, func(f full) error {
			if len(f.Declarations) != 0 {
				return fmt.Errorf("want empty: %+v", f)
			}
			return nil
		})
		checks := []func(f full) error{
			func(f full) error {
				if len(f.Declarations) != 1 || f.Declarations[0].ID != "acme" || f.Declarations[0].ReviewAt != review1 {
					return fmt.Errorf("after put: %+v", f)
				}
				return nil
			},
			func(f full) error {
				if len(f.Declarations) != 1 || f.Declarations[0].ReviewAt != review2 || f.Declarations[0].ReviewedBy != "books-oncall" {
					return fmt.Errorf("after review: %+v", f)
				}
				return nil
			},
			func(f full) error {
				if len(f.Declarations) != 0 {
					return fmt.Errorf("after delete: %+v", f)
				}
				return nil
			},
		}
		var lastSeq uint64
		for i, s := range steps {
			s.do(t, c)
			reqs := sink.WaitN(t, i+1, 15*time.Second, 2*time.Second)
			r := reqs[i]
			if err := harness.VerifyHookSignature(r, secret); err != nil {
				t.Fatalf("%s: %v", s.desc, err)
			}
			if r.Header.Get("X-Gwag-Event") != ops[i] {
				t.Errorf("%s: X-Gwag-Event %q", s.desc, r.Header.Get("X-Gwag-Event"))
			}
			var d delta
			if err := json.Unmarshal(r.Body, &d); err != nil {
				t.Fatal(err)
			}
			if d.Schema != "gwag.declarations.delta/v1" || d.Op != ops[i] || d.ID != "acme" || d.Seq <= lastSeq || d.At == "" {
				t.Fatalf("%s: hook body %s", s.desc, r.Body)
			}
			lastSeq = d.Seq
			switch ops[i] {
			case "put":
				if d.Before != nil || d.After == nil || d.After.Name != "Acme Corp" {
					t.Errorf("put body %s", r.Body)
				}
			case "review":
				if d.Before == nil || d.Before.ReviewAt != review1 || d.After == nil || d.After.ReviewAt != review2 {
					t.Errorf("review body %s", r.Body)
				}
			case "delete":
				if d.Before == nil || d.After != nil {
					t.Errorf("delete body %s", r.Body)
				}
			}
			wantFile(t, checks[i])
		}
	})

	t.Run("hook full, file delta", func(t *testing.T) {
		c, sink := start(t, "full", "delta")
		var lastSeq uint64
		for i, s := range steps {
			s.do(t, c)
			reqs := sink.WaitN(t, i+1, 15*time.Second, 2*time.Second)
			r := reqs[i]
			if err := harness.VerifyHookSignature(r, secret); err != nil {
				t.Fatalf("%s: %v", s.desc, err)
			}
			var f full
			if err := json.Unmarshal(r.Body, &f); err != nil {
				t.Fatal(err)
			}
			want := 1
			if ops[i] == "delete" {
				want = 0
			}
			if f.Schema != "gwag.declarations.full/v1" || f.Seq <= lastSeq || len(f.Declarations) != want {
				t.Fatalf("%s: hook body %s", s.desc, r.Body)
			}
			if ops[i] == "review" && f.Declarations[0].ReviewAt != review2 {
				t.Fatalf("review: hook body %s", r.Body)
			}
			lastSeq = f.Seq
		}

		// Every node: three lines, put/review/delete, with the same
		// seqs (the KV revisions) on every node.
		var seqs string
		for _, g := range c.Nodes {
			harness.Eventually(t, 15*time.Second, g.Name+" delta file", func() error {
				b, err := readFile(g)
				if err != nil {
					return err
				}
				lines := strings.Split(strings.TrimSpace(string(b)), "\n")
				if len(lines) != 3 {
					return fmt.Errorf("%d lines: %s", len(lines), b)
				}
				var got []string
				var prev uint64
				for i, l := range lines {
					var d delta
					if err := json.Unmarshal([]byte(l), &d); err != nil {
						return harness.Permanent(err)
					}
					if d.Schema != "gwag.declarations.delta/v1" || d.ID != "acme" || d.Seq <= prev {
						return harness.Permanent(fmt.Errorf("line %d: %s", i, l))
					}
					prev = d.Seq
					got = append(got, fmt.Sprintf("%s@%d", d.Op, d.Seq))
				}
				s := strings.Join(got, ",")
				if !strings.HasPrefix(s, "put@") || !strings.Contains(s, ",review@") || !strings.Contains(s, ",delete@") {
					return harness.Permanent(fmt.Errorf("ops %v", got))
				}
				if seqs == "" {
					seqs = s
				} else if s != seqs {
					return harness.Permanent(fmt.Errorf("%s, another node has %s", s, seqs))
				}
				return nil
			})
		}
	})
}
