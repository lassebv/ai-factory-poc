package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sqliteMod = `module torrentd

go 1.25

require (
	golang.org/x/time v0.12.0
	modernc.org/sqlite v1.38.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	golang.org/x/sys v0.34.0 // indirect
)
`

const sqliteGraph = `torrentd go@1.25
torrentd toolchain@go1.25.7
torrentd github.com/dustin/go-humanize@v1.0.1
torrentd golang.org/x/sys@v0.34.0
torrentd modernc.org/sqlite@v1.38.2
torrentd golang.org/x/time@v0.12.0
modernc.org/sqlite@v1.38.2 github.com/dustin/go-humanize@v1.0.1
modernc.org/sqlite@v1.38.2 golang.org/x/sys@v0.34.0
go@1.25 toolchain@go1.25
`

func mustCheck(t *testing.T, mod, graph string) []Violation {
	t.Helper()
	mf, err := ParseModFile([]byte(mod))
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	return Check(mf, g)
}

func TestParseModFile(t *testing.T) {
	mf, err := ParseModFile([]byte(sqliteMod + "require \"github.com/a/b\" v1.0.0 // indirect; for tests\nrequire c.io/d v2.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if mf.Module != "torrentd" || len(mf.Require) != 6 {
		t.Fatalf("got %+v", mf)
	}
	want := []Require{
		{"golang.org/x/time", "v0.12.0", false},
		{"modernc.org/sqlite", "v1.38.2", false},
		{"github.com/dustin/go-humanize", "v1.0.1", true},
		{"golang.org/x/sys", "v0.34.0", true},
		{"github.com/a/b", "v1.0.0", true},
		{"c.io/d", "v2.0.0", false},
	}
	for i, r := range want {
		if mf.Require[i] != r {
			t.Errorf("require %d = %+v, want %+v", i, mf.Require[i], r)
		}
	}
	for name, bad := range map[string]string{
		"no module":    "go 1.25\n",
		"unterminated": "module m\nrequire (\n a v1\n",
		"bad block":    "module m\nrequire (\n a\n)\n",
		"bad single":   "module m\nrequire a\n",
	} {
		if _, err := ParseModFile([]byte(bad)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseGraph(t *testing.T) {
	g, err := ParseGraph([]byte("a b@v1\n\nb@v1 c@v2\n"))
	if err != nil || len(g) != 2 || g[1] != (Edge{"b@v1", "c@v2"}) {
		t.Fatalf("got %v, %v", g, err)
	}
	if _, err := ParseGraph([]byte("a b c\n")); err == nil {
		t.Error("three fields: no error")
	}
}

func TestDenylisted(t *testing.T) {
	for _, p := range []string{
		"github.com/anacrolix/torrent", "github.com/anacrolix/dht/v2", "github.com/anacrolix/missinggo",
		"github.com/cenkalti/rain", "github.com/cenkalti/rain/v2", "github.com/jackpal/bencode-go",
		"github.com/zeebo/bencode", "github.com/nictuku/dht", "github.com/IncSW/go-bencode",
		"github.com/incsw/go-bencode", "github.com/marksamman/bencode", "github.com/arvidn/libtorrent",
		"github.com/x/fastbencode", "example.com/BitTorrent", "example.com/x/dht", "example.com/magnetlink",
	} {
		if !Denylisted(p) {
			t.Errorf("%s not denylisted", p)
		}
	}
	for _, p := range []string{"golang.org/x/time", "modernc.org/sqlite", "github.com/cenkalti/rainbow", "github.com/google/uuid"} {
		if Denylisted(p) {
			t.Errorf("%s denylisted", p)
		}
	}
}

func TestAccepts(t *testing.T) {
	if v := mustCheck(t, sqliteMod, sqliteGraph); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
	if v := mustCheck(t, "module torrentd\n\ngo 1.25\n", ""); len(v) != 0 {
		t.Fatalf("stdlib only: %v", v)
	}
	// The root may be named after torrents.
	if v := mustCheck(t, "module example.com/torrentd\n", "example.com/torrentd go@1.25\n"); len(v) != 0 {
		t.Fatalf("root exempt from denylist: %v", v)
	}
	// Transitive dependencies of golang.org/x are allowed.
	if v := mustCheck(t, "module m\nrequire golang.org/x/net v0.42.0\n",
		"m golang.org/x/net@v0.42.0\ngolang.org/x/net@v0.42.0 github.com/some/helper@v1.0.0\n"); len(v) != 0 {
		t.Fatalf("transitive via x: %v", v)
	}
}

func TestRejects(t *testing.T) {
	cases := []struct {
		name, mod, graph, module, reason string
	}{
		{"unknown direct", "module m\nrequire github.com/gorilla/mux v1.8.1\n", "m github.com/gorilla/mux@v1.8.1\n",
			"github.com/gorilla/mux", "direct requirement"},
		{"denylisted direct", "module m\nrequire github.com/anacrolix/torrent v1.0.0\n",
			"m github.com/anacrolix/torrent@v1.0.0\ngithub.com/anacrolix/torrent@v1.0.0 github.com/anacrolix/dht/v2@v2.0.0\n",
			"github.com/anacrolix/dht/v2", "denylisted"},
		{"denylisted deep", sqliteMod, sqliteGraph + "golang.org/x/sys@v0.34.0 github.com/zeebo/bencode@v1.0.0\n",
			"github.com/zeebo/bencode", "denylisted"},
		{"indirect orphan", "module m\nrequire golang.org/x/time v0.12.0\nrequire github.com/gorilla/mux v1.8.1 // indirect\n",
			"m golang.org/x/time@v0.12.0\nm github.com/gorilla/mux@v1.8.1\n", "github.com/gorilla/mux", "not reachable"},
		{"orphan only in go.mod", "module m\nrequire golang.org/x/time v0.12.0\nrequire github.com/a/b v1.0.0 // indirect\n",
			"m golang.org/x/time@v0.12.0\n", "github.com/a/b", "not reachable"},
		{"modernc dep required directly", strings.Replace(sqliteMod, "go-humanize v1.0.1 // indirect", "go-humanize v1.0.1", 1),
			sqliteGraph, "github.com/dustin/go-humanize", "direct requirement"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vs := mustCheck(t, c.mod, c.graph)
			var hit *Violation
			for i := range vs {
				if vs[i].Module == c.module {
					hit = &vs[i]
				}
			}
			if hit == nil {
				t.Fatalf("no violation for %s: %v", c.module, vs)
			}
			if !strings.Contains(hit.String(), c.module) || !strings.Contains(hit.Reason, c.reason) {
				t.Errorf("violation %q, want reason containing %q", hit, c.reason)
			}
		})
	}
}

func writeFixture(t *testing.T, mod, graph string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph.txt"), []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRun(t *testing.T) {
	clean := writeFixture(t, sqliteMod, sqliteGraph)
	dirty := writeFixture(t, "module m\nrequire github.com/gorilla/mux v1.8.1\n", "m github.com/gorilla/mux@v1.8.1\n")
	cases := []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"clean", []string{"-graph", filepath.Join(clean, "graph.txt"), clean}, 0, ""},
		{"clean with modfile", []string{"-graph", filepath.Join(clean, "graph.txt"), "-modfile", filepath.Join(clean, "go.mod")}, 0, ""},
		{"violation", []string{"-graph", filepath.Join(dirty, "graph.txt"), dirty}, 1, "github.com/gorilla/mux"},
		{"missing graph", []string{"-graph", filepath.Join(clean, "nope.txt"), clean}, 2, ""},
		{"missing go.mod", []string{"-graph", filepath.Join(clean, "graph.txt"), t.TempDir()}, 2, ""},
		{"bad go.mod", []string{"-graph", filepath.Join(clean, "graph.txt"), "-modfile", filepath.Join(clean, "graph.txt")}, 2, ""},
		{"bad flag", []string{"-nope"}, 2, ""},
		{"two dirs", []string{"a", "b"}, 2, ""},
		{"help", []string{"-h"}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(c.args, &out, &errOut); code != c.code {
				t.Fatalf("exit %d, want %d; stderr %s", code, c.code, errOut.String())
			}
			if !strings.Contains(out.String(), c.out) {
				t.Errorf("stdout %q lacks %q", out.String(), c.out)
			}
		})
	}
}

func TestRunGoModGraph(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{dir}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
}

func TestRunGraphFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\nrequire (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	// -modfile points at a valid go.mod, but `go mod graph` fails in dir.
	good := writeFixture(t, sqliteMod, "")
	if code := run([]string{"-modfile", filepath.Join(good, "go.mod"), dir}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
