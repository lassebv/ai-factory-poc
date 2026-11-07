// Package policy defines which factory role may write which paths in the
// repository. It is the single source of truth for both the PreToolUse hook
// (fast feedback while an agent works) and the integrity check (enforcement
// at the merge gate).
package policy

import (
	"path"
	"path/filepath"
	"strings"
)

// Role is a factory role. Agent definitions in .claude/agents/ use these
// names, and headless workers set them through FACTORY_ROLE.
type Role string

// The factory roles.
const (
	Human       Role = "human"
	Architect   Role = "architect"
	Verifier    Role = "verifier"
	Implementer Role = "implementer"
	Reviewer    Role = "reviewer"
	RedTeam     Role = "red-team"
	// Unknown is any agent that is not a factory role (for example a built-in
	// general-purpose subagent). It may only write to shared areas.
	Unknown Role = "unknown"
)

// KnownRoles lists every role except Unknown.
var KnownRoles = []Role{Human, Architect, Verifier, Implementer, Reviewer, RedTeam}

// ParseRole maps an agent type or FACTORY_ROLE value to a Role. A plugin
// namespace prefix ("plugin:implementer") is ignored.
func ParseRole(s string) Role {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	for _, r := range KnownRoles {
		if Role(s) == r {
			return r
		}
	}
	return Unknown
}

// Area is a set of paths with the same owners.
type Area struct {
	Name     string
	Owners   []Role
	Exact    []string // exact repo-relative paths
	Prefixes []string // directory prefixes, each ending in "/"
	Suffixes []string // file-name suffixes
}

// Allows reports whether role may write in the area.
func (a Area) Allows(role Role) bool {
	for _, r := range a.Owners {
		if r == role {
			return true
		}
	}
	return false
}

var everyone = []Role{Human, Architect, Verifier, Implementer, Reviewer, RedTeam, Unknown}

// Areas are matched in order; the first match wins. The last area is the
// default for everything else (product code).
var Areas = []Area{
	{
		Name:   "intent",
		Owners: []Role{Human},
		Exact:  []string{"project-description.md", "factory-approaches.md", "spec/spec-audit.md"},
	},
	{
		Name:     "factory-machinery",
		Owners:   []Role{Human},
		Exact:    []string{"CLAUDE.md", "harness/go.mod", "harness/go.sum", "harness/factory", "harness/README.md"},
		Prefixes: []string{".claude/", ".git/", "harness/cmd/", "harness/internal/", "harness/hooks/", "harness/prompts/", "retros/"},
	},
	{
		Name:     "shared",
		Owners:   everyone,
		Prefixes: []string{"escalations/", "reviews/", "runs/"},
	},
	{
		Name:     "red-team",
		Owners:   []Role{Human, Verifier, RedTeam},
		Prefixes: []string{"harness/redteam/"},
	},
	{
		Name:     "acceptance",
		Owners:   []Role{Human, Verifier},
		Exact:    []string{"spec/acceptance.yaml"},
		Prefixes: []string{"harness/", "e2e/"},
		Suffixes: []string{"_acceptance_test.go", ".acceptance.test.ts", ".acceptance.test.tsx"},
	},
	{
		Name:     "contracts",
		Owners:   []Role{Human, Architect},
		Prefixes: []string{"spec/"},
	},
	{
		Name:   "product",
		Owners: []Role{Human, Architect, Implementer},
	},
}

// Normalize turns a path into a clean, slash-separated repo-relative path.
func Normalize(rel string) string {
	p := path.Clean(filepath.ToSlash(rel))
	return strings.TrimPrefix(p, "./")
}

// AreaFor returns the area that owns the repo-relative path rel.
func AreaFor(rel string) Area {
	p := Normalize(rel)
	base := path.Base(p)
	for _, a := range Areas[:len(Areas)-1] {
		for _, e := range a.Exact {
			if p == e {
				return a
			}
		}
		for _, pre := range a.Prefixes {
			if strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/") {
				return a
			}
		}
		for _, s := range a.Suffixes {
			if strings.HasSuffix(base, s) {
				return a
			}
		}
	}
	return Areas[len(Areas)-1]
}

// CanWrite reports whether role may create, modify or delete rel.
func CanWrite(role Role, rel string) (bool, Area) {
	a := AreaFor(rel)
	return a.Allows(role), a
}

// Owners renders an area's owners for messages.
func Owners(a Area) string {
	s := make([]string, len(a.Owners))
	for i, r := range a.Owners {
		s[i] = string(r)
	}
	return strings.Join(s, ", ")
}
