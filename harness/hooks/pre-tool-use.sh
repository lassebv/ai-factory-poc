#!/usr/bin/env bash
# Claude Code PreToolUse hook: enforce the factory write policy
# (harness/internal/policy). Exit 2 blocks the tool call; stderr goes to Claude.
set -u
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INPUT="$(cat)"
if ! err="$("$HARNESS/hooks/build.sh")"; then
  agent="$(printf '%s' "$INPUT" | jq -r '.agent_type // empty' 2>/dev/null)"
  if [ -z "$agent" ] && [ "${FACTORY_ROLE:-human}" = "human" ]; then
    echo "warning: factory policy hook failed to build; allowing (human session): $err" >&2
    exit 0
  fi
  echo "BLOCKED: the factory policy hook failed to build, so writes are blocked for agents: $err" >&2
  exit 2
fi
printf '%s' "$INPUT" | "$HARNESS/bin/factory" hook
