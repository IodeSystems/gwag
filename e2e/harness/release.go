//go:build e2e

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ReleaseBinary returns a binary ("gateway", "gwag", ...) built from a
// release tag of this repository, e.g. "v2.11.0". The tag's tree is
// exported with git archive into a temp directory and built there;
// the binary is cached under the user cache directory
// (gwag-e2e/<tag>/<name>, override the root with E2E_CACHE_DIR), so
// later runs reuse it. A tag never changes, so the cache never goes
// stale; delete the directory to rebuild.
func ReleaseBinary(t testing.TB, tag, name string) string {
	t.Helper()
	tg, ok := targets[name]
	if !ok {
		t.Fatalf("harness: unknown binary %q", name)
	}
	v, _ := builds.LoadOrStore(tag+"/"+name, &buildResult{})
	br := v.(*buildResult)
	br.once.Do(func() { br.path, br.err = buildRelease(tag, name, tg) })
	if br.err != nil {
		t.Fatalf("harness: build %s at %s: %v", name, tag, br.err)
	}
	return br.path
}

func releaseCacheDir() (string, error) {
	if d := os.Getenv("E2E_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "gwag-e2e"), nil
}

func buildRelease(tag, name string, tg target) (string, error) {
	if strings.ContainsAny(tag, "/\\ ") || tag == "" {
		return "", fmt.Errorf("bad tag %q", tag)
	}
	cache, err := releaseCacheDir()
	if err != nil {
		return "", err
	}
	out := filepath.Join(cache, tag, name)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	src, err := os.MkdirTemp("", "gwag-e2e-"+tag+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(src)
	start := time.Now()
	// git archive, not a worktree: nothing is added to the repo's
	// worktree list, and a failed run leaves nothing behind.
	archive := exec.Command("sh", "-c", `git -C "$1" archive --format=tar "$2" | tar -x -C "$3"`, "sh", root, tag, src)
	if b, err := archive.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git archive %s: %v\n%s", tag, err, b)
	}
	if err := seedUIDist(src); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmp := out + fmt.Sprintf(".tmp%d", os.Getpid())
	args := []string{"build", "-o", tmp}
	if len(tg.tags) > 0 {
		args = append(args, "-tags", strings.Join(tg.tags, ","))
	}
	args = append(args, tg.pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join(src, tg.dir)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go %v: %v\n%s", args, err, b)
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "harness: built %s at %s in %s (cached at %s)\n", name, tag, time.Since(start).Round(time.Millisecond), out)
	return out, nil
}
