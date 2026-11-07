// Package hook implements the Claude Code PreToolUse hook that enforces the
// factory write policy while agents work.
package hook

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"torrentd.local/harness/internal/policy"
)

// Input is the subset of the PreToolUse hook payload the policy needs.
type Input struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
	AgentID   string          `json:"agent_id"`
	AgentType string          `json:"agent_type"`
}

// Decision is the outcome for one tool call.
type Decision struct {
	Allow  bool        `json:"allow"`
	Role   policy.Role `json:"role"`
	Tool   string      `json:"tool"`
	Target string      `json:"target,omitempty"`
	Area   string      `json:"area,omitempty"`
	Reason string      `json:"reason,omitempty"`
}

// ResolveRole picks the acting role. Inside a subagent, the subagent's type
// decides (unknown types are restricted). Otherwise FACTORY_ROLE decides, and
// an interactive session without it is the human.
func ResolveRole(agentType, envRole string) policy.Role {
	if agentType != "" {
		return policy.ParseRole(agentType)
	}
	if strings.TrimSpace(envRole) == "" {
		return policy.Human
	}
	return policy.ParseRole(envRole)
}

// Decide applies the write policy to one tool call.
func Decide(in Input, envRole, projectDir string) Decision {
	role := ResolveRole(in.AgentType, envRole)
	d := Decision{Allow: true, Role: role, Tool: in.ToolName}

	var fields struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Command      string `json:"command"`
	}
	_ = json.Unmarshal(in.ToolInput, &fields)

	switch in.ToolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		p := fields.FilePath
		if p == "" {
			p = fields.NotebookPath
		}
		if p == "" {
			return d
		}
		if !filepath.IsAbs(p) {
			base := in.Cwd
			if base == "" {
				base = projectDir
			}
			p = filepath.Join(base, p)
		}
		rel, err := filepath.Rel(projectDir, filepath.Clean(p))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return d // outside the project (e.g. a scratchpad)
		}
		rel = policy.Normalize(rel)
		d.Target = rel
		if ok, area := policy.CanWrite(role, rel); !ok {
			d.Allow = false
			d.Area = area.Name
			d.Reason = fmt.Sprintf("role %q may not modify %s (area %q, owned by: %s)",
				role, rel, area.Name, policy.Owners(area))
		}
	case "Bash":
		if v, area, lit := policy.BashViolation(role, fields.Command); v {
			d.Allow = false
			d.Target = lit
			d.Area = area.Name
			d.Reason = fmt.Sprintf("role %q may not run shell commands that write to %s (area %q, owned by: %s)",
				role, lit, area.Name, policy.Owners(area))
		}
	}
	return d
}

// Message renders the text Claude sees when a call is blocked.
func Message(d Decision) string {
	return "BLOCKED by the factory write policy: " + d.Reason + ".\n" +
		"Protected files are owned by other roles (see CLAUDE.md, \"Roles and ownership\"). " +
		"Do not try to work around this. If you believe the protected file is wrong, " +
		"write an escalation to escalations/<ticket>-<slug>.md explaining what and why, then continue with other work."
}
