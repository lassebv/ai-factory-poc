package integrity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"torrentd.local/harness/internal/policy"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func put(t *testing.T, dir, rel, s string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBranchTouchingProtectedFiles(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	put(t, dir, "spec/acceptance.yaml", "a\n")
	put(t, dir, "internal/x.go", "package x\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-qm", "base")
	run(t, dir, "git", "checkout", "-qb", "impl/M1")

	put(t, dir, "internal/x.go", "package x // changed\n")
	run(t, dir, "git", "commit", "-qam", "product change")
	put(t, dir, "spec/acceptance.yaml", "weakened\n")           // unstaged
	put(t, dir, "internal/y_acceptance_test.go", "package x\n") // untracked

	files, err := ChangedFiles(context.Background(), dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("changed = %v", files)
	}
	vs := Check(policy.Implementer, files)
	if len(vs) != 2 {
		t.Fatalf("violations = %v", vs)
	}
	if len(Check(policy.Human, files)) != 0 {
		t.Fatal("human may change anything")
	}
}
