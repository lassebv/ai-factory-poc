# ADR-0006: Peer ban policy: 3 strikes, 24 h expiry

- **Status:** Accepted (2026-11-06)
- **Audit finding:** G10 · **Spec:** §16.2, §16.6

## Context
§4.1 says to ban peers that "repeatedly" send bad data, tracked per IP. It defines no threshold, no rule for who gets blamed when several peers contributed to a failed piece, no expiry and no UI.

## Decision
- Record which peers contributed blocks to each piece. When a piece fails its hash check, **every contributor gets a strike**.
- Ban an IP at **3 strikes**.
- Bans are persisted in `banned_peers` with `expires_at` = banned_at + **24 h**.
- No ban-management UI in v1. `/stats` reports `bannedPeers`.

## Consequences
- An innocent peer that shared pieces with a bad one may collect strikes. Requiring 3 strikes and expiring bans limits the damage.
- Peers behind a shared NAT IP can be banned together for up to 24 h. This is accepted for v1.
- Schema change: `banned_peers.expires_at` (see ADR-0010).
