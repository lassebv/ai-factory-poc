package hook

import (
	"encoding/json"
	"testing"

	"torrentd.local/harness/internal/policy"
)

func input(tool, agentType string, ti map[string]string) Input {
	raw, _ := json.Marshal(ti)
	return Input{ToolName: tool, ToolInput: raw, Cwd: "/repo", AgentType: agentType}
}

func TestResolveRole(t *testing.T) {
	cases := []struct {
		agent, env string
		want       policy.Role
	}{
		{"", "", policy.Human},
		{"", "implementer", policy.Implementer},
		{"", "nonsense", policy.Unknown},
		{"verifier", "implementer", policy.Verifier},
		{"general-purpose", "", policy.Unknown},
	}
	for _, c := range cases {
		if got := ResolveRole(c.agent, c.env); got != c.want {
			t.Errorf("ResolveRole(%q,%q) = %s, want %s", c.agent, c.env, got, c.want)
		}
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name  string
		in    Input
		env   string
		allow bool
	}{
		{"human edits acceptance", input("Edit", "", map[string]string{"file_path": "/repo/spec/acceptance.yaml"}), "", true},
		{"implementer edits acceptance", input("Edit", "", map[string]string{"file_path": "/repo/spec/acceptance.yaml"}), "implementer", false},
		{"implementer subagent writes product", input("Write", "implementer", map[string]string{"file_path": "/repo/internal/x.go"}), "", true},
		{"relative path resolved from cwd", input("Write", "implementer", map[string]string{"file_path": "spec/acceptance.yaml"}), "", false},
		{"dotdot cleaned", input("Write", "implementer", map[string]string{"file_path": "/repo/internal/../CLAUDE.md"}), "", false},
		{"outside project allowed", input("Write", "implementer", map[string]string{"file_path": "/tmp/scratch/x.md"}), "", true},
		{"unknown subagent blocked", input("Write", "general-purpose", map[string]string{"file_path": "/repo/internal/x.go"}), "", false},
		{"notebook", input("NotebookEdit", "implementer", map[string]string{"notebook_path": "/repo/harness/x.ipynb"}), "", false},
		{"bash write blocked", input("Bash", "implementer", map[string]string{"command": "echo > spec/acceptance.yaml"}), "", false},
		{"bash read allowed", input("Bash", "implementer", map[string]string{"command": "cat spec/acceptance.yaml"}), "", true},
		{"other tools allowed", input("Read", "implementer", map[string]string{"file_path": "/repo/spec/acceptance.yaml"}), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.in, c.env, "/repo")
			if d.Allow != c.allow {
				t.Fatalf("allow = %v, want %v (%+v)", d.Allow, c.allow, d)
			}
			if !d.Allow && d.Reason == "" {
				t.Fatal("a denial must carry a reason")
			}
		})
	}
}
