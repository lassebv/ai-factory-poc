package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"torrentd.local/harness/internal/acceptance"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goEntry(id, pkg, run string) acceptance.Entry {
	return acceptance.Entry{ID: id, MS: 1, Tier: "L1", Check: acceptance.Check{Go: pkg, Run: run}}
}

// TestGoChecksEndToEnd runs real `go test` against a throwaway product module.
func TestGoChecksEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test")
	}
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/p\n\ngo 1.23\n")
	write(t, root, "a/a.go", "package a\n\nfunc Add(x, y int) int { return x + y }\n")
	write(t, root, "a/a_acceptance_test.go", `package a

import "testing"

func TestPasses(t *testing.T) { if Add(1, 2) != 3 { t.Fatal("bad") } }
func TestFails(t *testing.T)  { t.Fatal("boom") }
func TestSkips(t *testing.T)  { t.Skip("later") }
func TestSub(t *testing.T)    { t.Run("inner", func(t *testing.T) {}) }
`)
	write(t, root, "a/impl_test.go", "package a\n\nimport \"testing\"\n\nfunc TestSneaky(t *testing.T) {}\n")
	write(t, root, "b/b_acceptance_test.go", "package b\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { undefinedThing() }\n")

	r := &Runner{Root: root}
	res := r.Run(context.Background(), []acceptance.Entry{
		goEntry("P", "./a", "TestPasses"),
		goEntry("F", "./a", "TestFails"),
		goEntry("S", "./a", "TestSkips"),
		goEntry("SUB", "./a", "TestSub"),
		goEntry("SNEAKY", "./a", "TestSneaky"),
		goEntry("ABSENT", "./a", "TestAbsent"),
		goEntry("NOPKG", "./nope", "TestX"),
		goEntry("BUILD", "./b", "TestBroken"),
	})
	want := map[string]Status{
		"P": Pass, "F": Fail, "S": Fail, "SUB": Pass, "SNEAKY": Fail,
		"ABSENT": Missing, "NOPKG": Missing, "BUILD": Fail,
	}
	for _, r := range res {
		if r.Status != want[r.ID] {
			t.Errorf("%s: status %s, want %s (%s)", r.ID, r.Status, want[r.ID], r.Detail)
		}
	}
}

func TestNoProductModule(t *testing.T) {
	r := &Runner{Root: t.TempDir()}
	res := r.Run(context.Background(), []acceptance.Entry{goEntry("X", "./a", "TestA")})
	if res[0].Status != Missing || !strings.Contains(res[0].Detail, "go.mod") {
		t.Fatalf("got %+v", res[0])
	}
}

func TestScriptAndManual(t *testing.T) {
	root := t.TempDir()
	write(t, root, "harness/checks/ok.sh", "exit 0\n")
	write(t, root, "harness/checks/bad.sh", "echo nope; exit 3\n")
	r := &Runner{Root: root}
	res := r.Run(context.Background(), []acceptance.Entry{
		{ID: "OK", Check: acceptance.Check{Script: "harness/checks/ok.sh"}},
		{ID: "BAD", Check: acceptance.Check{Script: "harness/checks/bad.sh"}},
		{ID: "GONE", Check: acceptance.Check{Script: "harness/checks/gone.sh"}},
		{ID: "MAN", Check: acceptance.Check{Manual: "look at it"}},
		{ID: "PW", Check: acceptance.Check{Playwright: "e2e/x.spec.ts"}},
	})
	want := []Status{Pass, Fail, Missing, Manual, Missing}
	for i, r := range res {
		if r.Status != want[i] {
			t.Errorf("%s: %s, want %s (%s)", r.ID, r.Status, want[i], r.Detail)
		}
	}
	if !strings.Contains(res[1].Detail, "nope") {
		t.Errorf("failure detail should include output: %q", res[1].Detail)
	}
	if Passed(res) {
		t.Error("Passed must be false")
	}
}

func TestParseTestJSON(t *testing.T) {
	out := `# example.com/p [build failed]
{"Action":"run","Test":"TestA"}
{"Action":"output","Test":"TestA","Output":"hello\n"}
{"Action":"run","Test":"TestA/sub"}
{"Action":"fail","Test":"TestA/sub"}
{"Action":"fail","Test":"TestA"}
{"Action":"output","Output":"FAIL\n"}
`
	final, per, raw := parseTestJSON([]byte(out))
	if final["TestA"] != "fail" || final["TestA/sub"] != "" {
		t.Fatalf("final = %v", final)
	}
	if !strings.Contains(per["TestA"], "hello") || !strings.Contains(raw, "build failed") {
		t.Fatalf("per=%q raw=%q", per["TestA"], raw)
	}
}
