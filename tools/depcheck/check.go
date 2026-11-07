package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Require is one requirement from the root go.mod.
type Require struct {
	// Path is the module path.
	Path string
	// Version is the required version.
	Version string
	// Indirect is true when the line carries a "// indirect" comment.
	Indirect bool
}

// ModFile is the part of a go.mod file that depcheck needs.
type ModFile struct {
	// Module is the root module path.
	Module string
	// Require lists every require directive, direct and indirect.
	Require []Require
}

// Edge is one line of `go mod graph` output: From requires To. Both are
// "path@version", except the root module, which has no version.
type Edge struct {
	// From is the requiring module.
	From string
	// To is the required module.
	To string
}

// Violation is one broken rule for one module.
type Violation struct {
	// Module is the offending module path.
	Module string
	// Reason explains the rule that was broken.
	Reason string
}

// String formats the violation as one output line naming the module.
func (v Violation) String() string { return v.Module + ": " + v.Reason }

// ParseModFile extracts the module path and require directives from go.mod
// content. It understands single-line and block require directives and
// "// indirect" comments; everything else is ignored.
func ParseModFile(data []byte) (*ModFile, error) {
	mf := &ModFile{}
	inRequire := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		code, comment, _ := strings.Cut(line, "//")
		fields := strings.Fields(code)
		indirect := strings.TrimSpace(comment) == "indirect" || strings.HasPrefix(strings.TrimSpace(comment), "indirect;")
		switch {
		case inRequire:
			if len(fields) == 1 && fields[0] == ")" {
				inRequire = false
				continue
			}
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 {
				return nil, fmt.Errorf("line %d: malformed requirement %q", n, line)
			}
			mf.Require = append(mf.Require, Require{Path: unquote(fields[0]), Version: fields[1], Indirect: indirect})
		case len(fields) == 0:
		case fields[0] == "module" && len(fields) == 2:
			mf.Module = unquote(fields[1])
		case fields[0] == "require":
			switch {
			case len(fields) == 2 && fields[1] == "(":
				inRequire = true
			case len(fields) == 3:
				mf.Require = append(mf.Require, Require{Path: unquote(fields[1]), Version: fields[2], Indirect: indirect})
			default:
				return nil, fmt.Errorf("line %d: malformed require directive %q", n, line)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if inRequire {
		return nil, errors.New("unterminated require block")
	}
	if mf.Module == "" {
		return nil, errors.New("no module directive")
	}
	return mf, nil
}

func unquote(s string) string { return strings.Trim(s, "\"`") }

// ParseGraph parses `go mod graph` output.
func ParseGraph(data []byte) ([]Edge, error) {
	var edges []Edge
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want two fields, got %q", n, sc.Text())
		}
		edges = append(edges, Edge{From: fields[0], To: fields[1]})
	}
	return edges, sc.Err()
}

// modulePath strips the "@version" from a graph node.
func modulePath(node string) string {
	if i := strings.LastIndexByte(node, '@'); i >= 0 {
		return node[:i]
	}
	return node
}

// isPseudo reports whether a graph node is the go or toolchain pseudo-module.
func isPseudo(node string) bool {
	p := modulePath(node)
	return p == "go" || p == "toolchain"
}

// Allowed reports whether path may be a direct requirement (§2).
func Allowed(path string) bool {
	return strings.HasPrefix(path, "golang.org/x/") || strings.HasPrefix(path, "modernc.org/")
}

// denyExact are known BitTorrent-related modules; subpaths (such as major
// version suffixes) are denied too. A trailing "/" denies a whole namespace.
var denyExact = []string{
	"github.com/anacrolix/",
	"github.com/arvidn/",
	"github.com/cenkalti/rain",
	"github.com/jackpal/bencode-go",
	"github.com/zeebo/bencode",
	"github.com/nictuku/dht",
	"github.com/IncSW/go-bencode",
	"github.com/marksamman/bencode",
}

// denySubstrings deny any module path that contains them.
var denySubstrings = []string{"bencode", "torrent", "/dht", "magnet"}

// Denylisted reports whether path is a known or likely BitTorrent module.
func Denylisted(path string) bool {
	lower := strings.ToLower(path)
	for _, d := range denyExact {
		dl := strings.ToLower(d)
		if strings.HasSuffix(dl, "/") {
			if strings.HasPrefix(lower, dl) {
				return true
			}
		} else if lower == dl || strings.HasPrefix(lower, dl+"/") {
			return true
		}
	}
	for _, s := range denySubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// Check applies the allowlist and denylist rules to a module and its graph
// and returns the violations, at most one per module, sorted by module.
func Check(mod *ModFile, graph []Edge) []Violation {
	root := mod.Module
	found := map[string]Violation{}
	add := func(path, reason string) {
		if _, dup := found[path]; !dup {
			found[path] = Violation{Module: path, Reason: reason}
		}
	}

	// Every module mentioned anywhere, except the root and pseudo-modules.
	all := map[string]bool{}
	note := func(node string) {
		if node == root || isPseudo(node) {
			return
		}
		all[modulePath(node)] = true
	}
	for _, r := range mod.Require {
		all[r.Path] = true
	}
	for _, e := range graph {
		note(e.From)
		note(e.To)
	}

	// Denylist: at any depth.
	for p := range all {
		if Denylisted(p) {
			add(p, "denylisted BitTorrent-related module (§2)")
		}
	}

	// Rule 1: direct requirements must be golang.org/x/* or modernc.org/*.
	var starts []string
	for _, r := range mod.Require {
		if r.Indirect {
			continue
		}
		if !Allowed(r.Path) {
			add(r.Path, "direct requirement outside the allowlist (golang.org/x/*, modernc.org/*)")
			continue
		}
		starts = append(starts, r.Path)
	}

	// Rule 2: every other module is reachable from the allowed direct
	// requirements. Edges out of the root don't count: the root lists all of
	// its indirect requirements too.
	adj := map[string][]string{}
	for _, e := range graph {
		if e.From == root || isPseudo(e.From) || isPseudo(e.To) {
			continue
		}
		from := modulePath(e.From)
		adj[from] = append(adj[from], modulePath(e.To))
	}
	reach := map[string]bool{}
	queue := append([]string(nil), starts...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if reach[p] {
			continue
		}
		reach[p] = true
		queue = append(queue, adj[p]...)
	}
	for p := range all {
		if !reach[p] {
			add(p, "not reachable from an allowed direct requirement (golang.org/x/*, modernc.org/*)")
		}
	}

	out := make([]Violation, 0, len(found))
	for _, v := range found {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}
