package deploy

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testToken = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestGolden(t *testing.T) {
	for _, platform := range []string{Compose, K8s, Minikube, ECS} {
		for _, n := range []int{1, 3} {
			name := fmt.Sprintf("%s-r%d", platform, n)
			t.Run(name, func(t *testing.T) {
				files, err := Generate(Spec{Platform: platform, Replicas: n, Token: testToken, Image: "example.com/gwag:v1"})
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join("testdata", name)
				want := map[string]bool{}
				for _, f := range files {
					want[f.Path] = true
					// Token files are gitignored by the generated
					// secrets/.gitignore, so they have no golden copy;
					// check they carry the token instead.
					if isSecretFile(f.Path) {
						if !strings.Contains(string(f.Data), testToken) {
							t.Errorf("%s does not carry the admin token", f.Path)
						}
						continue
					}
					p := filepath.Join(dir, f.Path)
					if *update {
						if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(p, f.Data, 0o644); err != nil {
							t.Fatal(err)
						}
						continue
					}
					got, err := os.ReadFile(p)
					if err != nil {
						t.Fatalf("%s: %v (run go test ./gw/deploy -update)", f.Path, err)
					}
					if string(got) != string(f.Data) {
						t.Errorf("%s differs from golden (run go test ./gw/deploy -update and review the diff)", f.Path)
					}
				}
				if *update {
					return
				}
				filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
					if err == nil && !info.IsDir() {
						rel, _ := filepath.Rel(dir, p)
						if !want[rel] || isSecretFile(rel) {
							t.Errorf("stale golden file %s", rel)
						}
					}
					return nil
				})
			})
		}
	}
}

// isSecretFile reports a generated file under secrets/ other than its
// .gitignore.
func isSecretFile(p string) bool {
	return strings.HasPrefix(filepath.ToSlash(p), "secrets/") && filepath.Base(p) != ".gitignore"
}

func TestPeers(t *testing.T) {
	one, _ := Generate(Spec{Platform: Compose, Replicas: 1, Token: testToken})
	if strings.Contains(string(one[0].Data), "--nats-peer") {
		t.Error("1 replica must not pass --nats-peer")
	}
	three, _ := Generate(Spec{Platform: Compose, Replicas: 3, Token: testToken})
	if got := strings.Count(string(three[0].Data), "--nats-peer"); got != 6 {
		t.Errorf("3 nodes want 6 peer flags (2 each), got %d", got)
	}
}

func TestValidateReplicas(t *testing.T) {
	for n, wantWarn := range map[int]bool{1: false, 2: true, 3: false, 4: true, 5: false, 9: false} {
		w, err := ValidateReplicas(n)
		if err != nil || (len(w) > 0) != wantWarn {
			t.Errorf("n=%d warn=%v err=%v", n, w, err)
		}
	}
	for _, n := range []int{0, -1, 10} {
		if _, err := ValidateReplicas(n); err == nil {
			t.Errorf("n=%d want error", n)
		}
	}
}
