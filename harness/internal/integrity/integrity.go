// Package integrity enforces the write policy at merge time: it lists every
// file a branch changed relative to a base and reports those the branch's role
// was not allowed to touch. The PreToolUse hook gives fast feedback; this is
// the check that cannot be bypassed from inside an agent session.
package integrity

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"torrentd.local/harness/internal/policy"
)

// Violation is one file a role was not allowed to change.
type Violation struct {
	Path   string
	Area   string
	Owners string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s (area %s, owned by %s)", v.Path, v.Area, v.Owners)
}

func git(ctx context.Context, root string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, ee.Stderr)
		}
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// ChangedFiles lists files changed between base and the working tree:
// committed changes since the merge base, staged and unstaged changes, and
// untracked files (honoring .gitignore).
func ChangedFiles(ctx context.Context, root, base string) ([]string, error) {
	return ChangedFilesRange(ctx, root, base, "")
}

// ChangedFilesRange is ChangedFiles for the committed range base...head only
// (no working-tree changes) when head is set. It lets one branch be checked
// per role, e.g. the architect's commits and then the implementers' commits.
func ChangedFilesRange(ctx context.Context, root, base, head string) ([]string, error) {
	set := map[string]bool{}
	cmds := [][]string{
		{"diff", "--name-only", "--no-renames", base + "...HEAD"},
		{"diff", "--name-only", "--no-renames", "HEAD"},
		{"ls-files", "--others", "--exclude-standard"},
	}
	if head != "" {
		cmds = [][]string{{"diff", "--name-only", "--no-renames", base + "..." + head}}
	}
	for _, args := range cmds {
		lines, err := git(ctx, root, args...)
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			set[l] = true
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// Check returns the files in changed that role may not write.
func Check(role policy.Role, changed []string) []Violation {
	var vs []Violation
	for _, f := range changed {
		if ok, area := policy.CanWrite(role, f); !ok {
			vs = append(vs, Violation{Path: f, Area: area.Name, Owners: policy.Owners(area)})
		}
	}
	return vs
}
