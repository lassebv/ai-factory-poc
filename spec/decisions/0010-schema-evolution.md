# ADR-0010: Schema evolution before and after v1

- **Status:** Accepted (2026-11-06)
- **Audit finding:** T7 · **Spec:** §16.6

## Context
Several decisions change the §7.1 schema: `global_stats` (G4) and `banned_peers.expires_at` (ADR-0006). Nothing has been released yet.

## Decision
- **Until the first release tag**, the factory may edit `0001_init.sql` directly.
- **After the first release tag**, every change is a new numbered migration.
- Every schema change goes through the architect role and is recorded as an ADR or a §16 amendment.

## Consequences
- The schema stays clean before v1, with no pile of migrations from the build itself.
- Implementer agents must not change the schema on their own. They raise a contract-change request instead.
