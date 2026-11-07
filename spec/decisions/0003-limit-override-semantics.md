# ADR-0003: `limit_override` suspends both seeding limits

- **Status:** Accepted (2026-11-06)
- **Audit finding:** C4 · **Spec:** §16.4

## Context
§5.2 says that after an automatic stop, "the limit that triggered it" no longer applies until the user changes it. The schema has a single boolean, `limit_override`. It was unclear whether the *other* limit still applies, and which user change clears the override.

## Decision
- `limit_override = 1` suspends **both** the ratio limit and the seeding-time limit for that torrent.
- It is cleared only when the user PATCHes either of *that torrent's* limit fields (`ratioLimit`, `seedTimeLimitMin`).
- Changes to global limits do **not** clear it.

## Consequences
- A torrent the user explicitly resumed never stops unexpectedly because of an unrelated global change.
- The schema stays as it is: one flag.
- The UI shows that the limits are suspended (`limitOverride` in the detail object).
