// Package depcheck holds black-box acceptance tests for tools/depcheck, the
// guard for the "no torrent libraries" constraint (§2, DEP-01, DEP-02).
package depcheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"torrentd.local/harness/acceptance/internal/app"
)

var (
	once   sync.Once
	binDir string
	bin    string
	errBld error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

func depcheck(t *testing.T) string {
	t.Helper()
	once.Do(func() {
		root := app.RepoRoot()
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			errBld = errors.New("no go.mod at the repo root: the product module does not exist yet")
			return
		}
		binDir, errBld = os.MkdirTemp("", "depcheck-")
		if errBld != nil {
			return
		}
		bin = filepath.Join(binDir, "depcheck")
		cmd := exec.Command("go", "build", "-o", bin, "./tools/depcheck")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			errBld = errors.New("go build ./tools/depcheck failed: " + err.Error() + "\n" + string(out))
		}
	})
	if errBld != nil {
		t.Fatal(errBld)
	}
	return bin
}

func run(t *testing.T, fixture string) (int, string) {
	t.Helper()
	dir := filepath.Join(app.RepoRoot(), "harness", "fixtures", "depcheck", fixture)
	cmd := exec.Command(depcheck(t), "-graph", filepath.Join(dir, "graph.txt"), "-modfile", filepath.Join(dir, "go.mod"), dir)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	default:
		t.Fatalf("running depcheck: %v", err)
		return -1, ""
	}
}

func TestRejects(t *testing.T) {
	cases := []struct {
		fixture, module string
		denylisted      bool
	}{
		{"reject-anacrolix-direct", "github.com/anacrolix/torrent", true},
		{"reject-denylisted-via-modernc", "github.com/zeebo/bencode", true},
		{"reject-bencode-name", "github.com/someone/fastbencode", true},
		{"reject-unknown-direct", "github.com/gorilla/mux", false},
		{"reject-modernc-dep-required-directly", "github.com/google/uuid", false},
		{"reject-indirect-orphan", "github.com/gorilla/mux", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			code, out := run(t, c.fixture)
			if code != 1 {
				t.Fatalf("exit code %d, want 1; output:\n%s", code, out)
			}
			if !strings.Contains(out, c.module) {
				t.Errorf("output does not name %s:\n%s", c.module, out)
			}
			if c.denylisted && !strings.Contains(strings.ToLower(out), "denylisted") {
				t.Errorf("output does not say the module is denylisted:\n%s", out)
			}
		})
	}
	t.Run("usage error", func(t *testing.T) {
		cmd := exec.Command(depcheck(t), "-graph", filepath.Join(t.TempDir(), "missing.txt"))
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("missing -graph file: err %v, want exit code 2", err)
		}
	})
}

func TestAccepts(t *testing.T) {
	for _, fixture := range []string{"accept-stdlib-only", "accept-modernc", "accept-transitive-via-x"} {
		t.Run(fixture, func(t *testing.T) {
			if code, out := run(t, fixture); code != 0 {
				t.Fatalf("exit code %d, want 0; output:\n%s", code, out)
			}
		})
	}
}
