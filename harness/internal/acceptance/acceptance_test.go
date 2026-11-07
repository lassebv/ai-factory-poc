package acceptance

import (
	"strings"
	"testing"
)

func TestRealMatrixIsValid(t *testing.T) {
	entries, err := Load("../../../spec/acceptance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 100 {
		t.Fatalf("expected the full matrix, got %d entries", len(entries))
	}
}

func TestRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"unknown field": `- {id: A-1, src: x, ms: 1, tier: L1, text: t, status: planned, check: {manual: m}}`,
		"two kinds":     `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {manual: m, script: harness/checks/a.sh}}`,
		"bad tier":      `- {id: A-1, src: x, ms: 1, tier: L9, text: t, check: {manual: m}}`,
		"dup id": `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {manual: m}}
- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {manual: m}}`,
		"bad test name":              `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {go: ./x, run: "TestA|TestB"}}`,
		"vitest location":            `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {vitest: web/src/a.test.ts}}`,
		"harness outside acceptance": `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {go: ./internal/x, module: harness, run: TestA}}`,
		"script location":            `- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {script: tools/a.sh}}`,
		"fuzz time":                  `- {id: A-1, src: x, ms: 1, tier: L2, text: t, check: {fuzz: ./x, target: FuzzA, time: soon}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("expected an error for:\n%s", doc)
			}
		})
	}
}

func TestAcceptsAllKinds(t *testing.T) {
	doc := strings.Join([]string{
		`- {id: A-1, src: x, ms: 1, tier: L1, text: t, check: {go: ./internal/x, run: TestA}}`,
		`- {id: A-2, src: x, ms: 1, tier: L1, text: t, check: {go: ./acceptance/skeleton, module: harness, run: TestA}}`,
		`- {id: A-3, src: x, ms: 1, tier: L2, text: t, check: {fuzz: ./x, target: FuzzA, time: 10s}}`,
		`- {id: A-4, src: x, ms: 1, tier: L1, text: t, check: {vitest: web/src/a.acceptance.test.tsx}}`,
		`- {id: A-5, src: x, ms: 1, tier: L6, text: t, check: {playwright: e2e/a.spec.ts}, timeout: 5m}`,
		`- {id: A-6, src: x, ms: 1, tier: L0, text: t, check: {script: harness/checks/a.sh}}`,
		`- {id: A-7, src: x, ms: 1, tier: L5, text: t, check: {manual: do it}}`,
	}, "\n")
	entries, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Fatalf("got %d entries", len(entries))
	}
}
