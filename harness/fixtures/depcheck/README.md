# depcheck fixtures

Each directory holds a root `go.mod` and the matching `go mod graph` output (`graph.txt`). `harness/acceptance/depcheck` runs `go run ./tools/depcheck -graph graph.txt -modfile go.mod` on each one. Directories named `accept-*` must exit 0, and `reject-*` must exit 1 and name the offending module. The `accept-modernc` graph mirrors real `go mod graph` output (go/toolchain pseudo-modules, root edges to `// indirect` requirements). See `spec/contracts/go-api.md` for the rules.
