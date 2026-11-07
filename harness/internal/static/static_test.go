package static

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, rel, s string) {
	t.Helper()
	p := filepath.Join(root, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLintClock(t *testing.T) {
	root := t.TempDir()
	put(t, root, "internal/bt/peer/a.go", `package peer

import tm "time"

func f() { _ = tm.Now(); _ = tm.Duration(5); <-tm.After(1) }
`)
	put(t, root, "internal/bt/peer/a_test.go", "package peer\n\nimport \"time\"\n\nvar _ = time.Now()\n")
	put(t, root, "internal/api/x.go", "package api\n\nimport \"time\"\n\nvar _ = time.Now()\n")
	put(t, root, "internal/store/d.go", "package store\n\nimport . \"time\"\n\nvar _ = Second\n")
	fs, err := LintClock(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("findings = %v", fs)
	}
	for _, f := range fs {
		if strings.Contains(f.Pos, "_test.go") || strings.Contains(f.Pos, "internal/api") {
			t.Errorf("unexpected finding %v", f)
		}
	}
}

func TestQuality(t *testing.T) {
	root := t.TempDir()
	put(t, root, "internal/x/x.go", `package x

// Documented is fine.
func Documented() {}

func Undocumented() {}

type T struct{}

func (T) M() {}

type u struct{}

func (u) M() {}

// Grouped consts share the group doc.
const (
	A = 1
	B = 2
)

var V = 1
`)
	put(t, root, "internal/x/long.go", "package x\n"+strings.Repeat("\n", MaxFileLines+1))
	put(t, root, "harness/ignored.go", "package h\n\nfunc Ignored() {}\n")
	fs, err := Quality(root)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, f := range fs {
		msgs = append(msgs, f.Msg)
	}
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{"func Undocumented", "type T", "func M", "var V", "lines (limit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding %q in:\n%s", want, joined)
		}
	}
	if len(fs) != 5 {
		t.Errorf("want 5 findings, got %d:\n%s", len(fs), joined)
	}
}

func TestCoverageFloor(t *testing.T) {
	for dir, want := range map[string]float64{
		"cmd/torrentd": -1, "internal/bt/bencode": 80, "internal/api": 60, "tools/depcheck": 60, ".": 60,
	} {
		if got := CoverageFloor(dir); got != want {
			t.Errorf("CoverageFloor(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestHasTests(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a/a.go", "package a\n")
	put(t, root, "b/b.go", "package b\n")
	put(t, root, "b/b_test.go", "package b\n")
	if hasTests(root, "a") || !hasTests(root, "b") {
		t.Fatal("hasTests is wrong")
	}
}
