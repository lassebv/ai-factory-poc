// Package acceptance loads and validates spec/acceptance.yaml, the matrix that
// maps every requirement to an executable check.
package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Check describes how the gate verifies one entry. Exactly one kind is set.
type Check struct {
	// Go runs a Go test function. Module selects the module the package
	// belongs to: "" (the product module at the repo root) or "harness".
	Go     string `yaml:"go"`
	Module string `yaml:"module"`
	Run    string `yaml:"run"`

	// Fuzz runs a Go fuzz target in the product module for a fixed time.
	Fuzz   string `yaml:"fuzz"`
	Target string `yaml:"target"`
	Time   string `yaml:"time"`

	Vitest     string `yaml:"vitest"`
	Playwright string `yaml:"playwright"`
	Script     string `yaml:"script"`
	Manual     string `yaml:"manual"`
}

// Kind returns the check kind, or "" if none or several kinds are set.
func (c Check) Kind() string {
	kinds := []string{}
	if c.Go != "" {
		kinds = append(kinds, "go")
	}
	if c.Fuzz != "" {
		kinds = append(kinds, "fuzz")
	}
	if c.Vitest != "" {
		kinds = append(kinds, "vitest")
	}
	if c.Playwright != "" {
		kinds = append(kinds, "playwright")
	}
	if c.Script != "" {
		kinds = append(kinds, "script")
	}
	if c.Manual != "" {
		kinds = append(kinds, "manual")
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// Entry is one requirement in the acceptance matrix.
type Entry struct {
	ID      string `yaml:"id"`
	Src     string `yaml:"src"`
	MS      int    `yaml:"ms"`
	Tier    string `yaml:"tier"`
	Text    string `yaml:"text"`
	Check   Check  `yaml:"check"`
	Timeout string `yaml:"timeout"`
}

// TimeoutOr returns the entry's timeout, or def when none is set.
func (e Entry) TimeoutOr(def time.Duration) time.Duration {
	if d, err := time.ParseDuration(e.Timeout); err == nil && d > 0 {
		return d
	}
	return def
}

var (
	idRE         = regexp.MustCompile(`^[A-Z0-9]+(-[A-Z0-9]+)*$`)
	tierRE       = regexp.MustCompile(`^L[0-7]$`)
	testNameRE   = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)
	fuzzNameRE   = regexp.MustCompile(`^Fuzz[A-Za-z0-9_]*$`)
	goPkgRE      = regexp.MustCompile(`^\./[A-Za-z0-9_./-]+$`)
	vitestRE     = regexp.MustCompile(`^web/.+\.acceptance\.test\.tsx?$`)
	playwrightRE = regexp.MustCompile(`^e2e/.+\.spec\.ts$`)
	scriptRE     = regexp.MustCompile(`^harness/checks/[A-Za-z0-9_.-]+\.sh$`)
)

// Load reads and validates the acceptance matrix at path.
func Load(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates an acceptance matrix. Unknown fields are errors.
func Parse(data []byte) ([]Entry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var entries []Entry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode acceptance matrix: %w", err)
	}
	if err := Validate(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Validate checks every entry and returns all problems found, joined.
func Validate(entries []Entry) error {
	var errs []error
	seen := map[string]bool{}
	for i, e := range entries {
		where := fmt.Sprintf("entry %d (%s)", i+1, e.ID)
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: %s", where, fmt.Sprintf(format, args...)))
		}
		if !idRE.MatchString(e.ID) {
			bad("invalid id %q", e.ID)
		}
		if seen[e.ID] {
			bad("duplicate id")
		}
		seen[e.ID] = true
		if e.MS < 1 || e.MS > 9 {
			bad("ms must be 1..9, got %d", e.MS)
		}
		if !tierRE.MatchString(e.Tier) {
			bad("tier must be L0..L7, got %q", e.Tier)
		}
		if strings.TrimSpace(e.Text) == "" || strings.TrimSpace(e.Src) == "" {
			bad("text and src are required")
		}
		if e.Timeout != "" {
			if d, err := time.ParseDuration(e.Timeout); err != nil || d <= 0 {
				bad("invalid timeout %q", e.Timeout)
			}
		}
		c := e.Check
		switch c.Kind() {
		case "go":
			if !goPkgRE.MatchString(c.Go) {
				bad("go package must look like ./path, got %q", c.Go)
			}
			if c.Module != "" && c.Module != "harness" {
				bad("module must be empty or \"harness\", got %q", c.Module)
			}
			if c.Module == "harness" && !strings.HasPrefix(c.Go, "./acceptance/") {
				bad("harness-module checks must live under ./acceptance/, got %q", c.Go)
			}
			if !testNameRE.MatchString(c.Run) {
				bad("run must be a single Test function name, got %q", c.Run)
			}
		case "fuzz":
			if !goPkgRE.MatchString(c.Fuzz) {
				bad("fuzz package must look like ./path, got %q", c.Fuzz)
			}
			if !fuzzNameRE.MatchString(c.Target) {
				bad("fuzz target must be a Fuzz function name, got %q", c.Target)
			}
			if d, err := time.ParseDuration(c.Time); err != nil || d <= 0 {
				bad("fuzz time must be a duration, got %q", c.Time)
			}
		case "vitest":
			if !vitestRE.MatchString(c.Vitest) {
				bad("vitest file must match web/**/*.acceptance.test.ts(x), got %q", c.Vitest)
			}
		case "playwright":
			if !playwrightRE.MatchString(c.Playwright) {
				bad("playwright file must match e2e/**/*.spec.ts, got %q", c.Playwright)
			}
		case "script":
			if !scriptRE.MatchString(c.Script) {
				bad("script must be harness/checks/<name>.sh, got %q", c.Script)
			}
		case "manual":
		default:
			bad("check must set exactly one of go, fuzz, vitest, playwright, script, manual")
		}
		if c.Kind() != "go" && (c.Run != "" || c.Module != "") {
			bad("run/module are only valid for go checks")
		}
		if c.Kind() != "fuzz" && (c.Target != "" || c.Time != "") {
			bad("target/time are only valid for fuzz checks")
		}
	}
	return errors.Join(errs...)
}

// PackageMilestones maps each product package referenced by a go or fuzz
// check (repo-relative, e.g. "internal/bt/bencode") to the earliest
// milestone that checks it, i.e. the milestone that introduces it.
func PackageMilestones(entries []Entry) map[string]int {
	m := map[string]int{}
	for _, e := range entries {
		pkg := ""
		switch e.Check.Kind() {
		case "go":
			if e.Check.Module == "" {
				pkg = e.Check.Go
			}
		case "fuzz":
			pkg = e.Check.Fuzz
		}
		if pkg == "" {
			continue
		}
		pkg = strings.TrimPrefix(pkg, "./")
		if cur, ok := m[pkg]; !ok || e.MS < cur {
			m[pkg] = e.MS
		}
	}
	return m
}
