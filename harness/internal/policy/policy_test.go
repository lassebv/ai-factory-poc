package policy

import "testing"

func TestAreaFor(t *testing.T) {
	cases := map[string]string{
		"project-description.md":                         "intent",
		"spec/spec-audit.md":                             "intent",
		"CLAUDE.md":                                      "factory-machinery",
		".claude/settings.json":                          "factory-machinery",
		"harness/internal/policy/policy.go":              "factory-machinery",
		"harness/hooks/pre-tool-use.sh":                  "factory-machinery",
		"escalations/M1-healthz.md":                      "shared",
		"runs/gate/latest.json":                          "shared",
		"harness/redteam/x_test.go":                      "red-team",
		"spec/acceptance.yaml":                           "acceptance",
		"harness/fixtures/torrents/a.torrent":            "acceptance",
		"harness/acceptance/skeleton/healthz_test.go":    "acceptance",
		"e2e/startup.spec.ts":                            "acceptance",
		"internal/bt/bencode/bencode_acceptance_test.go": "acceptance",
		"web/src/lib/format.acceptance.test.ts":          "acceptance",
		"spec/openapi.yaml":                              "contracts",
		"spec/decisions/0011-x.md":                       "contracts",
		"internal/bt/bencode/decode.go":                  "product",
		"internal/bt/bencode/decode_test.go":             "product",
		"./cmd/torrentd/main.go":                         "product",
		"web/src/lib/format.test.ts":                     "product",
		"harness":                                        "acceptance",
	}
	for p, want := range cases {
		if got := AreaFor(p).Name; got != want {
			t.Errorf("AreaFor(%q) = %s, want %s", p, got, want)
		}
	}
}

func TestCanWrite(t *testing.T) {
	cases := []struct {
		role Role
		path string
		want bool
	}{
		{Human, "spec/acceptance.yaml", true},
		{Implementer, "spec/acceptance.yaml", false},
		{Implementer, "internal/bt/bencode/decode.go", true},
		{Implementer, "internal/bt/bencode/bencode_acceptance_test.go", false},
		{Implementer, "internal/bt/bencode/decode_test.go", true},
		{Implementer, "escalations/x.md", true},
		{Implementer, ".claude/settings.json", false},
		{Verifier, "spec/acceptance.yaml", true},
		{Verifier, "harness/fixtures/x", true},
		{Verifier, "harness/internal/policy/policy.go", false},
		{Verifier, "internal/bt/bencode/decode.go", false},
		{Architect, "spec/openapi.yaml", true},
		{Architect, "spec/acceptance.yaml", false},
		{Architect, "internal/clock/clock.go", true},
		{Reviewer, "internal/x.go", false},
		{Reviewer, "reviews/M1.md", true},
		{RedTeam, "harness/redteam/peer_test.go", true},
		{RedTeam, "harness/fixtures/x", false},
		{Unknown, "internal/x.go", false},
		{Unknown, "escalations/x.md", true},
	}
	for _, c := range cases {
		if got, _ := CanWrite(c.role, c.path); got != c.want {
			t.Errorf("CanWrite(%s, %q) = %v, want %v", c.role, c.path, got, c.want)
		}
	}
}

func TestParseRole(t *testing.T) {
	for in, want := range map[string]Role{
		"implementer": Implementer, " Verifier ": Verifier, "factory:red-team": RedTeam,
		"general-purpose": Unknown, "fork": Unknown, "": Unknown,
	} {
		if got := ParseRole(in); got != want {
			t.Errorf("ParseRole(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBashViolation(t *testing.T) {
	const proj = "/repo"
	blocked := []string{
		"echo x >> spec/acceptance.yaml",
		"sed -i '' 's/a/b/' spec/acceptance.yaml",
		"rm -rf harness/fixtures",
		"rm -rf harness",
		"cd harness && rm -rf fixtures",
		"cd /repo/harness; rm -rf fixtures",
		"cp /tmp/x internal/bt/bencode/bencode_acceptance_test.go",
		"python3 -c 'open(\"CLAUDE.md\",\"w\").write(\"x\")'",
		"python3 - <<'EOF'\nfrom pathlib import Path\nPath('spec/acceptance.yaml').write_text('x')\nEOF",
		"node -e \"require('fs').writeFileSync('e2e/startup.spec.ts','')\"",
		"git checkout -- spec/acceptance.yaml",
		"tee .claude/settings.json < /tmp/x",
		"echo x | tee -a spec/acceptance.yaml",
		"mv e2e/startup.spec.ts /tmp/",
		"cat /dev/null > /repo/spec/acceptance.yaml",
		"echo hi > \"$PWD/spec/acceptance.yaml\"",
		"bash <<'EOF'\nrm spec/acceptance.yaml\nEOF",
		"bash -c 'rm -f CLAUDE.md'",
		"find harness/fixtures -name '*.torrent' -delete",
		"perl -pi -e 's/a/b/' spec/acceptance.yaml",
		"echo $(rm spec/acceptance.yaml)",
		"dd if=/dev/zero of=harness/factory bs=1 count=1",
	}
	for _, cmd := range blocked {
		if v, _, _ := BashViolation(Implementer, cmd, proj, proj); !v {
			t.Errorf("expected implementer to be blocked: %q", cmd)
		}
	}
	allowed := []string{
		"cat spec/acceptance.yaml",
		"go test ./internal/... 2>&1 | tail -20",
		"grep -n TestHealthz harness/acceptance/skeleton/*.go",
		"grep -r x harness/ > /tmp/out.txt",
		"cp spec/acceptance.yaml /tmp/copy.yaml",
		"go build -o /tmp/torrentd ./cmd/torrentd",
		"echo hi > internal/bt/bencode/notes.txt",
		"ls spec/ 2>/dev/null",
		"git commit -m 'implement bencode per spec/contracts/go-api.md'",
		"echo x > escalations/M1-question.md",
		// False positives observed in the approach-a-m1 run (factory bug #1):
		"cat > internal/config/config.go <<'EOF'\n// Default data dir: see spec/contracts/go-api.md\npackage config\nEOF",
		"python3 - <<'EOF'\np='tools/depcheck/main.go'\ns=open(p).read()\nopen(p,'w').write(s.replace('a','b'))\nEOF\ngo build ./... && harness/factory gate --id DEP-01 -v 2>&1 | tail -12",
		"printf '%s\\n' 'gate:' '\\tharness/factory gate --ms 1' > Makefile",
		"git commit -q -m 'M1: depcheck' && harness/factory integrity --base main --role implementer",
		"lsof -iTCP:8080 -sTCP:LISTEN; harness/factory gate --ms 1 -v 2>&1 | tail -80",
		"cat >> PROGRESS.md <<'EOF'\nRan harness/factory gate; spec/acceptance.yaml QUAL-01 fails\nEOF",
		"echo 'see harness/README.md' >&2",
	}
	for _, cmd := range allowed {
		if v, a, p := BashViolation(Implementer, cmd, proj, proj); v {
			t.Errorf("expected implementer to be allowed: %q (matched %s in %s)", cmd, p, a.Name)
		}
	}
	if v, _, _ := BashViolation(Human, "rm -rf spec/acceptance.yaml", proj, proj); v {
		t.Error("human must never be blocked")
	}
	if v, _, _ := BashViolation(Verifier, "echo x >> spec/acceptance.yaml", proj, proj); v {
		t.Error("verifier owns the acceptance matrix")
	}
	if v, _, _ := BashViolation(Verifier, "echo x >> harness/internal/policy/policy.go", proj, proj); !v {
		t.Error("verifier must not modify the policy")
	}
	if v, _, _ := BashViolation(Implementer, "cd /repo/internal && echo x > ../spec/acceptance.yaml", proj, proj); !v {
		t.Error("relative paths must resolve against the tracked cwd")
	}
}

func TestWriteTargetsIgnoresHeredocContent(t *testing.T) {
	refs := WriteTargets("cat > a.go <<'EOF'\nrm -rf spec\necho > CLAUDE.md\nEOF\necho done > b.txt", "/r")
	var got []string
	for _, r := range refs {
		got = append(got, r.Path)
	}
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.txt" {
		t.Fatalf("targets = %v, want [a.go b.txt]", got)
	}
}
