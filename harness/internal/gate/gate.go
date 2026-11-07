// Package gate runs the checks in the acceptance matrix and reports results.
//
// The gate is deliberately suspicious. A Go test only counts if it is defined
// in a verifier-owned file (a *_acceptance_test.go file, or anywhere under
// harness/acceptance/). A test that is absent, skipped, or did not run never
// counts as passing.
package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"torrentd.local/harness/internal/acceptance"
)

// Status is the outcome of one check.
type Status string

// Check outcomes.
const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Missing Status = "missing" // the check (test, file, script) does not exist yet
	Manual  Status = "manual"  // not automated; recorded at milestones
	Error   Status = "error"   // the harness could not run the check
)

// Result is the outcome of one acceptance entry.
type Result struct {
	ID      string  `json:"id"`
	MS      int     `json:"ms"`
	Tier    string  `json:"tier"`
	Kind    string  `json:"kind"`
	Status  Status  `json:"status"`
	Detail  string  `json:"detail,omitempty"`
	Seconds float64 `json:"seconds"`
}

// ExecFunc runs a command in dir and returns its combined output and exit
// code. err is non-nil only when the command could not run or timed out.
type ExecFunc func(ctx context.Context, dir string, env []string, name string, args ...string) (out []byte, code int, err error)

// Runner executes checks.
type Runner struct {
	Root           string // repository root
	DefaultTimeout time.Duration
	Exec           ExecFunc
	Log            func(format string, args ...any)
	// MaxMS is the highest milestone selected for this run; scripts see it as
	// GATE_MS_MAX so milestone-scoped checks (coverage) can skip later packages.
	MaxMS int
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Run executes the given entries and returns results in the same order.
func (r *Runner) Run(ctx context.Context, entries []acceptance.Entry) []Result {
	if r.Exec == nil {
		r.Exec = DefaultExec
	}
	if r.DefaultTimeout == 0 {
		r.DefaultTimeout = 10 * time.Minute
	}
	results := map[string]Result{}

	// Group Go checks by module and package so each package runs once.
	type pkgKey struct{ module, pkg string }
	groups := map[pkgKey][]acceptance.Entry{}
	var order []pkgKey
	for _, e := range entries {
		if e.Check.Kind() != "go" {
			continue
		}
		k := pkgKey{e.Check.Module, e.Check.Go}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	for _, k := range order {
		r.logf("go test %s (%s)", k.pkg, moduleName(k.module))
		for id, res := range r.runGoPackage(ctx, k.module, k.pkg, groups[k]) {
			results[id] = res
		}
	}
	for _, e := range entries {
		if e.Check.Kind() == "go" {
			continue
		}
		r.logf("%s %s", e.Check.Kind(), e.ID)
		start := time.Now()
		res := r.runOther(ctx, e)
		res.Seconds = time.Since(start).Seconds()
		results[e.ID] = res
	}

	out := make([]Result, 0, len(entries))
	for _, e := range entries {
		res := results[e.ID]
		res.ID, res.MS, res.Tier, res.Kind = e.ID, e.MS, e.Tier, e.Check.Kind()
		out = append(out, res)
	}
	return out
}

func moduleName(m string) string {
	if m == "" {
		return "product"
	}
	return m
}

// moduleDir returns the directory of the named module.
func (r *Runner) moduleDir(module string) string {
	if module == "harness" {
		return filepath.Join(r.Root, "harness")
	}
	return r.Root
}

var funcDefRE = regexp.MustCompile(`(?m)^func\s+((?:Test|Fuzz)[A-Za-z0-9_]*)\s*\(`)

// testDefs maps Test/Fuzz function names in dir's _test.go files to the file
// that defines them.
func testDefs(dir string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	defs := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range funcDefRE.FindAllSubmatch(data, -1) {
			defs[string(m[1])] = filepath.Base(f)
		}
	}
	return defs, nil
}

// trusted reports whether a test file may count as an acceptance check.
func trusted(module, file string) bool {
	if module == "harness" {
		return true // the whole package lives under harness/acceptance/
	}
	return strings.HasSuffix(file, "_acceptance_test.go")
}

// locate finds name in pkg and checks that it is defined in a trusted file.
// It returns a non-empty Result status when the check cannot proceed.
func (r *Runner) locate(module, pkg, name string) (Status, string) {
	modDir := r.moduleDir(module)
	if _, err := os.Stat(filepath.Join(modDir, "go.mod")); err != nil {
		if module == "" {
			return Missing, "no go.mod at the repo root yet (product module not created)"
		}
		return Error, "harness module not found"
	}
	dir := filepath.Join(modDir, filepath.FromSlash(pkg))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Missing, fmt.Sprintf("package directory %s does not exist", pkg) + r.stagedHint(pkg)
	}
	defs, err := testDefs(dir)
	if err != nil {
		return Error, err.Error()
	}
	file, ok := defs[name]
	if !ok {
		return Missing, fmt.Sprintf("%s is not defined in %s", name, pkg) + r.stagedHint(pkg)
	}
	if !trusted(module, file) {
		return Fail, fmt.Sprintf("%s is defined in %s, which is not a verifier-owned *_acceptance_test.go file", name, file)
	}
	return "", ""
}

// stagedHint explains where a not-yet-promoted acceptance test lives.
func (r *Runner) stagedHint(pkg string) string {
	matches, _ := filepath.Glob(filepath.Join(r.Root, "harness", "_staged", "m*", filepath.FromSlash(pkg), "*_acceptance_test.go"))
	if len(matches) == 0 {
		return ""
	}
	rel, _ := filepath.Rel(r.Root, matches[0])
	ms := strings.TrimPrefix(strings.Split(filepath.ToSlash(rel), "/")[2], "m")
	return fmt.Sprintf(" (staged in %s; the verifier promotes it with `harness/factory promote --ms %s` when milestone %s starts)", filepath.ToSlash(filepath.Dir(rel)), ms, ms)
}

type testEvent struct {
	Action string
	Test   string
	Output string
}

func (r *Runner) runGoPackage(ctx context.Context, module, pkg string, entries []acceptance.Entry) map[string]Result {
	results := map[string]Result{}
	var runnable []acceptance.Entry
	timeout := time.Duration(0)
	for _, e := range entries {
		if st, detail := r.locate(module, pkg, e.Check.Run); st != "" {
			results[e.ID] = Result{Status: st, Detail: detail}
			continue
		}
		runnable = append(runnable, e)
		if t := e.TimeoutOr(r.DefaultTimeout); t > timeout {
			timeout = t
		}
	}
	if len(runnable) == 0 {
		return results
	}

	names := make([]string, len(runnable))
	for i, e := range runnable {
		names[i] = e.Check.Run
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	start := time.Now()
	out, _, err := r.Exec(ctx, r.moduleDir(module), nil, "go", "test", pkg,
		"-run", "^("+strings.Join(names, "|")+")$", "-count=1", "-json", "-timeout", timeout.String())
	secs := time.Since(start).Seconds() / float64(len(runnable))

	final, perTest, raw := parseTestJSON(out)
	for _, e := range runnable {
		name := e.Check.Run
		res := Result{Seconds: secs}
		switch {
		case err != nil:
			res.Status, res.Detail = Error, fmt.Sprintf("go test could not run: %v\n%s", err, tail(raw, 15))
		case final[name] == "pass":
			res.Status = Pass
		case final[name] == "fail":
			res.Status, res.Detail = Fail, tail(perTest[name], 25)
		case final[name] == "skip":
			res.Status, res.Detail = Fail, "test was skipped; acceptance tests must not skip\n"+tail(perTest[name], 10)
		default:
			res.Status, res.Detail = Fail, "test did not run (does the package build?)\n"+tail(raw, 25)
		}
		results[e.ID] = res
	}
	return results
}

// parseTestJSON reads `go test -json` output. It returns the final action of
// each top-level test, the output of each top-level test, and all output that
// was not attributed to a test (build errors and the like).
func parseTestJSON(out []byte) (final map[string]string, perTest map[string]string, raw string) {
	final = map[string]string{}
	tests := map[string]*strings.Builder{}
	var other strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var ev testEvent
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil {
			other.Write(line)
			other.WriteByte('\n')
			continue
		}
		top := ev.Test
		if i := strings.Index(top, "/"); i >= 0 {
			top = top[:i]
		}
		if top == "" {
			other.WriteString(ev.Output)
			continue
		}
		if tests[top] == nil {
			tests[top] = &strings.Builder{}
		}
		tests[top].WriteString(ev.Output)
		if ev.Test == top && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip") {
			final[top] = ev.Action
		}
	}
	perTest = map[string]string{}
	for k, b := range tests {
		perTest[k] = b.String()
	}
	return final, perTest, other.String()
}

func (r *Runner) runOther(ctx context.Context, e acceptance.Entry) Result {
	c := e.Check
	timeout := e.TimeoutOr(r.DefaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(r.Root, filepath.FromSlash(rel)))
		return err == nil
	}
	run := func(dir string, env []string, name string, args ...string) Result {
		out, code, err := r.Exec(ctx, dir, env, name, args...)
		switch {
		case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			return Result{Status: Fail, Detail: "timed out after " + timeout.String() + "\n" + tail(string(out), 15)}
		case err != nil:
			return Result{Status: Error, Detail: err.Error()}
		case code == 0:
			return Result{Status: Pass}
		default:
			return Result{Status: Fail, Detail: fmt.Sprintf("exit code %d\n%s", code, tail(string(out), 25))}
		}
	}

	switch c.Kind() {
	case "manual":
		return Result{Status: Manual, Detail: c.Manual}
	case "script":
		if !exists(c.Script) {
			return Result{Status: Missing, Detail: c.Script + " does not exist"}
		}
		return run(r.Root, []string{"GATE_ROOT=" + r.Root, fmt.Sprintf("GATE_MS_MAX=%d", r.MaxMS)}, "bash", filepath.Join(r.Root, c.Script))
	case "fuzz":
		if st, detail := r.locate("", c.Fuzz, c.Target); st != "" {
			return Result{Status: st, Detail: detail}
		}
		return run(r.Root, nil, "go", "test", c.Fuzz, "-run", "^$", "-fuzz", "^"+c.Target+"$", "-fuzztime", c.Time)
	case "vitest":
		if !exists(c.Vitest) {
			return Result{Status: Missing, Detail: c.Vitest + " does not exist"}
		}
		if !exists("web/node_modules") {
			return Result{Status: Error, Detail: "web/node_modules is missing; run `npm ci` in web/"}
		}
		return run(filepath.Join(r.Root, "web"), nil, "npx", "vitest", "run", strings.TrimPrefix(c.Vitest, "web/"))
	case "playwright":
		if !exists(c.Playwright) {
			return Result{Status: Missing, Detail: c.Playwright + " does not exist"}
		}
		if !exists("e2e/node_modules") {
			return Result{Status: Error, Detail: "e2e/node_modules is missing; run `npm ci` in e2e/"}
		}
		return run(filepath.Join(r.Root, "e2e"), nil, "npx", "playwright", "test", strings.TrimPrefix(c.Playwright, "e2e/"))
	}
	return Result{Status: Error, Detail: "unknown check kind"}
}

// tail returns the last n non-empty lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = append([]string{"…"}, kept[len(kept)-n:]...)
	}
	return strings.Join(kept, "\n")
}

// Totals counts results by status.
func Totals(results []Result) map[Status]int {
	t := map[Status]int{}
	for _, r := range results {
		t[r.Status]++
	}
	return t
}

// Passed reports whether every automated result passed.
func Passed(results []Result) bool {
	for _, r := range results {
		if r.Status != Pass && r.Status != Manual {
			return false
		}
	}
	return true
}

// Summary renders a human-readable report.
func Summary(results []Result, verbose bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %-3s %-4s %-8s %s\n", "ID", "MS", "TIER", "STATUS", "DETAIL")
	for _, r := range results {
		first := r.Detail
		if i := strings.IndexByte(first, '\n'); i >= 0 && !verbose {
			first = first[:i]
		}
		if len(first) > 110 && !verbose {
			first = first[:107] + "..."
		}
		fmt.Fprintf(&b, "%-10s %-3d %-4s %-8s %s\n", r.ID, r.MS, r.Tier, r.Status, first)
		if verbose && r.Status == Fail && strings.Contains(r.Detail, "\n") {
			b.WriteString("    " + strings.ReplaceAll(r.Detail, "\n", "\n    ") + "\n")
		}
	}

	byMS := map[int][]Result{}
	for _, r := range results {
		byMS[r.MS] = append(byMS[r.MS], r)
	}
	var ms []int
	for k := range byMS {
		ms = append(ms, k)
	}
	sort.Ints(ms)
	b.WriteString("\nProgress\n")
	line := func(label string, rs []Result) {
		t := Totals(rs)
		auto := len(rs) - t[Manual]
		pct := 0.0
		if auto > 0 {
			pct = 100 * float64(t[Pass]) / float64(auto)
		}
		fmt.Fprintf(&b, "  %-4s %3d/%-3d pass (%5.1f%%)   fail %-3d missing %-3d error %-3d manual %d\n",
			label, t[Pass], auto, pct, t[Fail], t[Missing], t[Error], t[Manual])
	}
	for _, m := range ms {
		line(fmt.Sprintf("M%d", m), byMS[m])
	}
	line("ALL", results)
	return b.String()
}
