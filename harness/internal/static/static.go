// Package static implements source-level checks on the product module:
// the clock lint (T1), doc comments and file length (ADR-0009) and the
// coverage floor (ADR-0009).
package static

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Finding is one violation, with a file:line position when available.
type Finding struct {
	Pos string
	Msg string
}

func (f Finding) String() string { return f.Pos + ": " + f.Msg }

// RequireProductModule returns an error when the repo has no product go.mod.
func RequireProductModule(root string) error {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return errors.New("no go.mod at the repo root: the product module does not exist yet")
	}
	return nil
}

// skipDir reports directories that are not product Go code.
func skipDir(rel, name string) bool {
	if rel == "." {
		return false
	}
	if strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules" {
		return true
	}
	switch rel {
	case "harness", "web", "e2e", "runs", "spec", "escalations", "reviews":
		return true
	}
	return false
}

// goFiles lists product .go files under root/sub (sub may be ".").
func goFiles(root, sub string, includeTests bool) ([]string, error) {
	var files []string
	start := filepath.Join(root, sub)
	if _, err := os.Stat(start); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if skipDir(filepath.ToSlash(rel), d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(p, "_test.go") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	sort.Strings(files)
	return files, err
}

var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// ClockDirs are the packages where wall-clock access is forbidden (T1).
var ClockDirs = []string{"internal/bt", "internal/stats", "internal/store"}

var forbiddenTime = map[string]bool{
	"Now": true, "Since": true, "Until": true, "After": true, "AfterFunc": true,
	"Tick": true, "NewTimer": true, "NewTicker": true, "Sleep": true,
}

// LintClock reports direct use of wall-clock functions from package time in
// non-test files under ClockDirs. Time must come from the injected clock.
func LintClock(root string) ([]Finding, error) {
	var findings []Finding
	fset := token.NewFileSet()
	for _, dir := range ClockDirs {
		files, err := goFiles(root, dir, false)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			local := ""
			for _, imp := range file.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == "time" {
					local = "time"
					if imp.Name != nil {
						local = imp.Name.Name
					}
				}
			}
			switch local {
			case "":
				continue
			case ".":
				findings = append(findings, Finding{rel(root, fset.Position(file.Package)), `dot-import of "time" is forbidden`})
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && forbiddenTime[sel.Sel.Name] {
					findings = append(findings, Finding{rel(root, fset.Position(sel.Pos())),
						fmt.Sprintf("time.%s is forbidden here; use the injected clock (internal/clock)", sel.Sel.Name)})
				}
				return true
			})
		}
	}
	return findings, nil
}

func rel(root string, pos token.Position) string {
	r, err := filepath.Rel(root, pos.Filename)
	if err != nil {
		r = pos.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(r), pos.Line)
}

// MaxFileLines is the per-file limit from ADR-0009.
const MaxFileLines = 800

// Quality reports exported identifiers without doc comments (non-test files)
// and Go files longer than MaxFileLines (all files), skipping generated code.
func Quality(root string) ([]Finding, error) {
	var findings []Finding
	files, err := goFiles(root, ".", true)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if generatedRE.Match(data) {
			continue
		}
		if n := bytes.Count(data, []byte("\n")); n > MaxFileLines {
			findings = append(findings, Finding{rel(root, token.Position{Filename: f, Line: 1}),
				fmt.Sprintf("file has %d lines (limit %d)", n, MaxFileLines)})
		}
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, data, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		missing := func(pos token.Pos, what string) {
			findings = append(findings, Finding{rel(root, fset.Position(pos)), what + " has no doc comment"})
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Doc != nil {
					continue
				}
				if d.Recv != nil && !exportedRecv(d.Recv) {
					continue
				}
				missing(d.Pos(), "exported func "+d.Name.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && s.Doc == nil && d.Doc == nil {
							missing(s.Pos(), "exported type "+s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() && s.Doc == nil && d.Doc == nil && s.Comment == nil {
								missing(n.Pos(), "exported "+d.Tok.String()+" "+n.Name)
							}
						}
					}
				}
			}
		}
	}
	return findings, nil
}

func exportedRecv(fl *ast.FieldList) bool {
	if len(fl.List) == 0 {
		return false
	}
	t := fl.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.IsExported()
		default:
			return false
		}
	}
}

// CoverageFloor returns the required coverage for a package directory, or -1
// when the package is exempt (ADR-0009).
func CoverageFloor(relDir string) float64 {
	switch {
	case relDir == "cmd" || strings.HasPrefix(relDir, "cmd/"):
		return -1 // main packages are covered by black-box acceptance tests
	case strings.HasPrefix(relDir, "internal/bt/"):
		return 80
	default:
		return 60
	}
}

var coverRE = regexp.MustCompile(`coverage:\s+([0-9.]+)% of statements`)

// Coverage runs `go test -cover ./...` in root and checks every package
// against its floor. Packages without tests count as 0%. A floor is enforced
// from the milestone that introduces the package (intro, from the acceptance
// matrix): when upto > 0, packages introduced after milestone upto are
// reported but not enforced.
func Coverage(ctx context.Context, root string, upto int, intro map[string]int) ([]Finding, string, error) {
	cmdMod := exec.CommandContext(ctx, "go", "list", "-m")
	cmdMod.Dir = root
	modOut, err := cmdMod.Output()
	if err != nil {
		return nil, "", fmt.Errorf("go list -m: %w", err)
	}
	module := strings.TrimSpace(string(modOut))

	cmd := exec.CommandContext(ctx, "go", "test", "-cover", "-count=1", "./...")
	cmd.Dir = root
	out, runErr := cmd.CombinedOutput()

	var findings []Finding
	var report strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var pkg string
		for _, f := range fields {
			if f == module || strings.HasPrefix(f, module+"/") {
				pkg = f
				break
			}
		}
		if pkg == "" {
			continue
		}
		relDir := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		if relDir == "" {
			relDir = "."
		}
		floor := CoverageFloor(relDir)
		var pct float64
		status := "ok"
		switch {
		case fields[0] == "FAIL":
			status = "tests fail"
		case strings.Contains(line, "[no test files]"):
			status = "no tests"
		default:
			m := coverRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pct, _ = strconv.ParseFloat(m[1], 64)
		}
		if ms := intro[relDir]; upto > 0 && ms > upto {
			fmt.Fprintf(&report, "%-40s %6.1f%%  not enforced before M%d  %s\n", relDir, pct, ms, status)
			continue
		}
		verdict := status
		if floor >= 0 && status == "ok" && pct < floor {
			verdict = "BELOW FLOOR"
		}
		fmt.Fprintf(&report, "%-40s %6.1f%%  floor %4.0f%%  %s\n", relDir, pct, floor, verdict)
		if floor < 0 {
			continue
		}
		switch {
		case status != "ok":
			findings = append(findings, Finding{relDir, status})
		case pct < floor:
			findings = append(findings, Finding{relDir, fmt.Sprintf("coverage %.1f%% is below the %.0f%% floor", pct, floor)})
		}
	}
	if runErr != nil && len(findings) == 0 {
		findings = append(findings, Finding{".", "go test failed:\n" + string(out)})
	}
	return findings, report.String(), nil
}
