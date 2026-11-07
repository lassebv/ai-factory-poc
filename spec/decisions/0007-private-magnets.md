# ADR-0007: Private magnets may use DHT/PEX until metadata arrives

- **Status:** Accepted (2026-11-06)
- **Audit finding:** G15 · **Spec:** §16.2

## Context
Whether a magnet is private is unknown until its info dict arrives. Using DHT and PEX in the meantime leaks the info-hash outside the private tracker. Avoiding them entirely makes magnets without trackers unusable.

## Decision
- While a torrent is in `metadata`, DHT, PEX and LSD may be used, as libtorrent does.
- When the metadata shows `private=1`: disconnect all peers whose source is `dht`, `pex` or `lsd`, and stop DHT, PEX and LSD for that torrent permanently.

## Consequences
- Magnets behave like they do in mainstream clients.
- There is a short metadata-phase leak for private magnets. Users of private trackers normally add `.torrent` files, which are not affected.
