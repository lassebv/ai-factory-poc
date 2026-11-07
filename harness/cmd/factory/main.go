// Command factory is the software-factory harness for torrentd: it runs the
// acceptance gate, enforces the write policy, and implements static checks.
//
// Usage:
//
//	factory gate [--ms 1,2 | --upto 2] [--tier L0,L1] [--id A,B] [--report] [-v]
//	factory validate
//	factory integrity --base main --role implementer
//	factory hook                      (Claude Code PreToolUse hook; JSON on stdin)
//	factory policy <role> <path>...   (explain the write policy)
//	factory promote --ms 2            (move staged acceptance tests into the product tree)
//	factory lint-clock | quality | coverage [--upto N]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"torrentd.local/harness/internal/acceptance"
	"torrentd.local/harness/internal/gate"
	"torrentd.local/harness/internal/hook"
	"torrentd.local/harness/internal/integrity"
	"torrentd.local/harness/internal/policy"
	"torrentd.local/harness/internal/static"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "gate":
		code = runGate(ctx, args)
	case "validate":
		code = runValidate(args)
	case "integrity":
		code = runIntegrity(ctx, args)
	case "hook":
		code = runHook(os.Stdin)
	case "policy":
		code = runPolicy(args)
	case "promote":
		code = runPromote(args)
	case "lint-clock", "quality", "coverage":
		code = runStatic(ctx, cmd, args)
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: factory <command> [flags]

commands:
  gate        run acceptance checks (--ms, --upto, --tier, --id, --report, -v)
  validate    validate spec/acceptance.yaml
  integrity   fail if the branch changed files its role may not write (--base, --role)
  hook        Claude Code PreToolUse hook (reads JSON on stdin)
  policy      explain the write policy: factory policy <role> <path>...
  promote     move harness/_staged/m<N>/ acceptance tests into the product tree (--ms N)
  lint-clock  forbid direct wall-clock use in engine packages (T1)
  quality     doc comments on exported identifiers, file length (ADR-0009)
  coverage    coverage floors per package (ADR-0009)`)
}

// findRoot walks up from the working directory to the repo root, identified
// by spec/acceptance.yaml.
func findRoot(flagRoot string) (string, error) {
	if flagRoot != "" {
		return filepath.Abs(flagRoot)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "spec", "acceptance.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find the repo root (no spec/acceptance.yaml above the working directory)")
		}
		dir = parent
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	root := fs.String("root", "", "repo root")
	_ = fs.Parse(args)
	r, err := findRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	entries, err := acceptance.Load(filepath.Join(r, "spec", "acceptance.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("spec/acceptance.yaml: %d entries, valid\n", len(entries))
	return 0
}

func runGate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	root := fs.String("root", "", "repo root (default: found from the working directory)")
	msList := fs.String("ms", "", "comma-separated milestones to run, e.g. 1,2")
	upto := fs.Int("upto", 0, "run all milestones up to and including N")
	tiers := fs.String("tier", "", "comma-separated tiers, e.g. L0,L1")
	ids := fs.String("id", "", "comma-separated entry ids")
	report := fs.Bool("report", false, "always exit 0 (progress report instead of a gate)")
	verbose := fs.Bool("v", false, "print full failure details")
	timeout := fs.Duration("timeout", 10*time.Minute, "default per-check timeout")
	noWrite := fs.Bool("no-write", false, "do not write results to runs/gate/")
	_ = fs.Parse(args)

	r, err := findRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	entries, err := acceptance.Load(filepath.Join(r, "spec", "acceptance.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid acceptance matrix:", err)
		return 2
	}

	msSet := map[int]bool{}
	for _, s := range splitList(*msList) {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(s), "M"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad --ms value:", s)
			return 2
		}
		msSet[n] = true
	}
	tierSet := map[string]bool{}
	for _, s := range splitList(*tiers) {
		tierSet[strings.ToUpper(s)] = true
	}
	idSet := map[string]bool{}
	for _, s := range splitList(*ids) {
		idSet[s] = true
	}
	var selected []acceptance.Entry
	for _, e := range entries {
		if len(msSet) > 0 && !msSet[e.MS] {
			continue
		}
		if *upto > 0 && e.MS > *upto {
			continue
		}
		if len(tierSet) > 0 && !tierSet[e.Tier] {
			continue
		}
		if len(idSet) > 0 && !idSet[e.ID] {
			continue
		}
		selected = append(selected, e)
	}
	if len(selected) == 0 {
		fmt.Fprintln(os.Stderr, "no entries match the filter")
		return 2
	}

	started := time.Now().UTC()
	maxMS := 0
	for _, e := range selected {
		if e.MS > maxMS {
			maxMS = e.MS
		}
	}
	runner := &gate.Runner{Root: r, DefaultTimeout: *timeout, MaxMS: maxMS, Log: func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, "· "+f+"\n", a...)
	}}
	results := runner.Run(ctx, selected)
	fmt.Print(gate.Summary(results, *verbose))

	if !*noWrite {
		if path, err := writeResults(r, started, args, results); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not write results:", err)
		} else {
			fmt.Printf("\nresults: %s\n", path)
		}
	}
	if *report || gate.Passed(results) {
		return 0
	}
	return 1
}

func writeResults(root string, started time.Time, args []string, results []gate.Result) (string, error) {
	dir := filepath.Join(root, "runs", "gate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	head, _ := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	doc := map[string]any{
		"startedAt":  started.Format(time.RFC3339),
		"finishedAt": time.Now().UTC().Format(time.RFC3339),
		"gitHead":    strings.TrimSpace(string(head)),
		"args":       args,
		"role":       os.Getenv("FACTORY_ROLE"),
		"totals":     gate.Totals(results),
		"passed":     gate.Passed(results),
		"results":    results,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	name := filepath.Join(dir, started.Format("20060102T150405Z")+".json")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(dir, "latest.json"), data, 0o644)
	return name, nil
}

func runIntegrity(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("integrity", flag.ExitOnError)
	root := fs.String("root", "", "repo root")
	base := fs.String("base", "main", "base ref to compare against")
	roleFlag := fs.String("role", os.Getenv("FACTORY_ROLE"), "role that produced the branch")
	_ = fs.Parse(args)
	if *roleFlag == "" {
		fmt.Fprintln(os.Stderr, "--role is required (or set FACTORY_ROLE)")
		return 2
	}
	role := policy.ParseRole(*roleFlag)
	r, err := findRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	files, err := integrity.ChangedFiles(ctx, r, *base)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	vs := integrity.Check(role, files)
	if len(vs) == 0 {
		fmt.Printf("integrity ok: %d changed files, all writable by role %s\n", len(files), role)
		return 0
	}
	fmt.Printf("integrity FAILED: role %s changed %d protected file(s):\n", role, len(vs))
	for _, v := range vs {
		fmt.Println("  " + v.String())
	}
	return 1
}

func runHook(stdin io.Reader) int {
	envRole := os.Getenv("FACTORY_ROLE")
	data, err := io.ReadAll(stdin)
	var in hook.Input
	if err == nil {
		err = json.Unmarshal(data, &in)
	}
	if err != nil {
		// Fail closed for agents, open for the human session.
		if hook.ResolveRole("", envRole) == policy.Human {
			return 0
		}
		fmt.Fprintln(os.Stderr, "BLOCKED: factory hook could not parse its input:", err)
		return 2
	}
	projectDir := os.Getenv("CLAUDE_PROJECT_DIR")
	if projectDir == "" {
		if out, err := exec.Command("git", "-C", in.Cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
			projectDir = strings.TrimSpace(string(out))
		} else {
			projectDir = in.Cwd
		}
	}
	if resolved, err := filepath.EvalSymlinks(projectDir); err == nil {
		projectDir = resolved
	}
	d := hook.Decide(in, envRole, projectDir)
	if d.Allow {
		return 0
	}
	logDenial(projectDir, in, d)
	fmt.Fprintln(os.Stderr, hook.Message(d))
	return 2
}

// logDenial appends to runs/policy-denials.jsonl: a gaming indicator that the
// factory tracks per run.
func logDenial(projectDir string, in hook.Input, d hook.Decision) {
	dir := filepath.Join(projectDir, "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "policy-denials.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339), "session": in.SessionID,
		"agentId": in.AgentID, "agentType": in.AgentType, "decision": d,
	})
	_, _ = f.Write(append(line, '\n'))
}

func runPolicy(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: factory policy <role> <path>...")
		return 2
	}
	role := policy.ParseRole(args[0])
	for _, p := range args[1:] {
		ok, a := policy.CanWrite(role, p)
		verdict := "DENY "
		if ok {
			verdict = "ALLOW"
		}
		fmt.Printf("%s %-12s %-45s area=%s owners=%s\n", verdict, role, p, a.Name, policy.Owners(a))
	}
	return 0
}

func runStatic(ctx context.Context, cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	root := fs.String("root", "", "repo root")
	defUpto, _ := strconv.Atoi(os.Getenv("GATE_MS_MAX"))
	upto := fs.Int("upto", defUpto, "coverage: enforce floors only for packages introduced up to milestone N (0 = all; default $GATE_MS_MAX)")
	_ = fs.Parse(args)
	r, err := findRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := static.RequireProductModule(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var findings []static.Finding
	switch cmd {
	case "lint-clock":
		findings, err = static.LintClock(r)
	case "quality":
		findings, err = static.Quality(r)
	case "coverage":
		var report string
		entries, lerr := acceptance.Load(filepath.Join(r, "spec", "acceptance.yaml"))
		if lerr != nil {
			fmt.Fprintln(os.Stderr, lerr)
			return 2
		}
		findings, report, err = static.Coverage(ctx, r, *upto, acceptance.PackageMilestones(entries))
		fmt.Print(report)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, f := range findings {
		fmt.Println(f.String())
	}
	if len(findings) > 0 {
		fmt.Printf("%s: %d finding(s)\n", cmd, len(findings))
		return 1
	}
	fmt.Printf("%s: ok\n", cmd)
	return 0
}

// runPromote moves staged acceptance tests for a milestone from
// harness/_staged/m<N>/ to the same paths in the product tree.
func runPromote(args []string) int {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	root := fs.String("root", "", "repo root")
	ms := fs.Int("ms", 0, "milestone to promote")
	force := fs.Bool("force", false, "overwrite differing files in the product tree")
	_ = fs.Parse(args)
	if *ms <= 0 {
		fmt.Fprintln(os.Stderr, "--ms is required")
		return 2
	}
	r, err := findRoot(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	stage := filepath.Join(r, "harness", "_staged", fmt.Sprintf("m%d", *ms))
	var moved int
	err = filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), "~") {
			return nil // editor swap/backup files stay behind
		}
		rel, _ := filepath.Rel(stage, p)
		dst := filepath.Join(r, rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if old, err := os.ReadFile(dst); err == nil && string(old) != string(data) && !*force {
			return fmt.Errorf("%s already exists and differs (use --force)", rel)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		fmt.Println("promoted", filepath.ToSlash(rel))
		moved++
		return os.Remove(p)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%d file(s) promoted for milestone %d; commit them before the milestone starts\n", moved, *ms)
	return 0
}
