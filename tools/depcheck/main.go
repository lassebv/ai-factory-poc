// Command depcheck enforces the §2 dependency allowlist ("no torrent
// libraries") on a Go module graph, as pinned in the depcheck contract
// (§16.10 T5).
//
// Usage:
//
//	go run ./tools/depcheck [-graph FILE] [-modfile FILE] [DIR]
//
// Without flags it checks the module in DIR (default ".") using
// `go mod graph`. -graph reads the graph from FILE instead, and -modfile
// reads the root go.mod from FILE. Exit code 0 means clean, 1 means
// violations (one per line on stdout), 2 means a usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole program; it returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("depcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	graphFile := fs.String("graph", "", "read `go mod graph` output from `FILE` instead of running go")
	modFile := fs.String("modfile", "", "read the root go.mod from `FILE` (default DIR/go.mod)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: depcheck [-graph FILE] [-modfile FILE] [DIR]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	violations, err := check(dir, *graphFile, *modFile)
	if err != nil {
		fmt.Fprintln(stderr, "depcheck:", err)
		return 2
	}
	for _, v := range violations {
		fmt.Fprintln(stdout, v)
	}
	if len(violations) > 0 {
		fmt.Fprintf(stderr, "depcheck: %d violation(s) of the §2 dependency allowlist\n", len(violations))
		return 1
	}
	return 0
}

// check loads the inputs and returns the violations.
func check(dir, graphFile, modFile string) ([]Violation, error) {
	if modFile == "" {
		modFile = filepath.Join(dir, "go.mod")
	}
	modData, err := os.ReadFile(modFile)
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	mod, err := ParseModFile(modData)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", modFile, err)
	}

	var graphData []byte
	if graphFile != "" {
		if graphData, err = os.ReadFile(graphFile); err != nil {
			return nil, fmt.Errorf("read graph: %w", err)
		}
	} else {
		cmd := exec.Command("go", "mod", "graph")
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		if graphData, err = cmd.Output(); err != nil {
			return nil, fmt.Errorf("go mod graph: %w", err)
		}
	}
	graph, err := ParseGraph(graphData)
	if err != nil {
		return nil, fmt.Errorf("graph: %w", err)
	}
	return Check(mod, graph), nil
}
