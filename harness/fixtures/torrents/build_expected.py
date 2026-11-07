#!/usr/bin/env python3
"""Build harness/fixtures/torrents/expected.json.

Inputs:
  * oracle.jsonl - produced by the oracle image (libtorrent + transmission-show),
    see regen.sh. Valid fixtures take their expected values from libtorrent,
    cross-checked against transmission's info-hash.
  * VERDICTS below - which fixtures torrentd must accept or reject, according
    to project-description.md (§4.1, §16.1). This is the verifier's reading of
    the spec; where it differs from libtorrent's behavior, the spec wins.

Strictness (canonical integers, sorted unique keys in the info dict) is
checked here by a small independent parser and must agree with the verdicts.
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

ACCEPT = "accept"
VERDICTS = {
    # Real-world torrents (webtorrent.io, releases.ubuntu.com).
    "real/big-buck-bunny.torrent": ACCEPT,
    "real/cosmos-laundromat.torrent": ACCEPT,
    "real/sintel.torrent": ACCEPT,
    "real/ubuntu-24.04.3-live-server-amd64.iso.torrent": ACCEPT,
    # libtorrent's test corpus (BSD, see libtorrent/LICENSE.libtorrent).
    "libtorrent/base.torrent": ACCEPT,
    "libtorrent/sample.torrent": ACCEPT,             # BEP 47 pad file hidden
    "libtorrent/single_multi_file.torrent": ACCEPT,
    "libtorrent/v2_hybrid.torrent": ACCEPT,          # C2: hybrid, v1 view, 9 pad files hidden
    "libtorrent/v2.torrent": ACCEPT,                 # has both v1 and v2 metadata -> hybrid
    "libtorrent/v2_only.torrent": ("META-08", "v2-only torrent (no v1 pieces)"),
    "libtorrent/absolute_filename.torrent": ("META-02", "path component '/foobar' contains a separator"),
    "libtorrent/backslash_path.torrent": ("META-02", "path components contain a backslash separator"),
    "libtorrent/parent_path.torrent": ("META-02", "'..' path components"),
    "libtorrent/hidden_parent_path.torrent": ("META-02", "component 'foo/../../../bar' contains separators and '..'"),
    "libtorrent/slash_path.torrent": ("META-02", "path components are '/'"),
    "libtorrent/invalid_filename.torrent": ("META-02", "'.' path component and backslash in name"),
    "libtorrent/invalid_filename2.torrent": ("META-02", "'.' and '/' path components"),
    "libtorrent/empty_path.torrent": ("META-02", "empty path components"),
    "libtorrent/empty_path_multi.torrent": ("META-02", "empty path components"),
    "libtorrent/duplicate_files.torrent": ("META-03", "two files with the same path"),
    "libtorrent/unordered.torrent": ("BENC-02", "info dict keys are not sorted"),
    "libtorrent/pad_file.torrent": ("BENC-02", "file dict keys are not sorted"),
    "libtorrent/pad_file_no_path.torrent": ("META-02", "file without a usable path"),
    "libtorrent/invalid_file_size.torrent": ("META-STRUCT", "file length is not an integer"),
    "libtorrent/large_piece_size.torrent": ("META-06", "piece length 536854528 exceeds 256 MiB"),
    "libtorrent/invalid_name.torrent": ("META-STRUCT", "name is not a string"),
    "libtorrent/no_name.torrent": ("META-STRUCT", "name is missing"),
    "libtorrent/invalid_piece_len.torrent": ("META-06", "piece length invalid"),
    "libtorrent/missing_piece_len.torrent": ("META-06", "piece length missing"),
    "libtorrent/negative_piece_len.torrent": ("META-06", "negative piece length"),
    "libtorrent/invalid_pieces.torrent": ("META-06", "pieces invalid"),
    "libtorrent/unaligned_pieces.torrent": ("META-06", "wrong number of piece hashes"),
    "libtorrent/negative_file_size.torrent": ("META-STRUCT", "negative file length"),
    "libtorrent/no_files.torrent": ("META-STRUCT", "no files"),
    "libtorrent/string.torrent": ("META-STRUCT", "top level is not a dict"),
    "libtorrent/zero.torrent": ("META-STRUCT", "zero-length content"),
    # Synthetic fixtures (synth/generate.py).
    "synth/private_announce_list.torrent": ACCEPT,
    "synth/unknown_info_keys.torrent": ACCEPT,       # BENC-04: hash covers unknown keys
    "synth/utf8_names.torrent": ACCEPT,              # META-05
    "synth/windows_names.torrent": ACCEPT,           # META-04 (paths overridden below)
    "synth/non_power_of_two_piece.torrent": ACCEPT,  # A2: allowed, only logged
    "synth/min_piece_length.torrent": ACCEPT,
    "synth/piece_length_too_small.torrent": ("META-06", "piece length 8192 < 16 KiB"),
    "synth/duplicate_case.torrent": ("META-03", "Readme.txt vs README.txt after case-folding"),
    "synth/duplicate_nfc.torrent": ("META-03", "café.txt NFC vs NFD"),
    "synth/pieces_not_multiple_of_20.torrent": ("META-06", "len(pieces) = 39"),
    "synth/pieces_count_mismatch.torrent": ("META-06", "1 hash for 2 pieces"),
    "synth/noncanonical_int.torrent": ("BENC-02", "i0425e in info dict"),
    "synth/negative_zero.torrent": ("BENC-02", "i-0e in info dict"),
    "synth/duplicate_info_key.torrent": ("BENC-02", "duplicate key in info dict"),
    "synth/not_bencode.torrent": ("META-STRUCT", "not bencode at all"),
}

# META-04: expected sanitized paths. "NOT_RESERVED" means the name must be
# changed so that it is no longer a Windows reserved device name; the exact
# spelling is left to the implementation.
PATH_OVERRIDES = {
    "synth/windows_names.torrent": [
        "win/a_b_c_d_e_f_g_h.txt", "win/trail_", "win/space_",
        "NOT_RESERVED:win/CON", "NOT_RESERVED:win/nul.txt", "win/ok.txt",
    ],
}


class NotCanonical(Exception):
    pass


def strict_parse(b, i=0, depth=0):
    """Parse one value strictly; return end index. Raises on non-canonical input."""
    if depth > 64:
        raise NotCanonical("too deep")
    c = b[i:i + 1]
    if c == b"i":
        j = b.index(b"e", i)
        s = b[i + 1:j]
        if s in (b"", b"-", b"-0") or (s.startswith(b"0") and s != b"0") or s.startswith(b"-0"):
            raise NotCanonical("non-canonical integer %r" % s)
        int(s)
        return j + 1
    if c == b"l":
        i += 1
        while b[i:i + 1] != b"e":
            i = strict_parse(b, i, depth + 1)
        return i + 1
    if c == b"d":
        i += 1
        prev = None
        while b[i:i + 1] != b"e":
            j = b.index(b":", i)
            n = int(b[i:j])
            key = b[j + 1:j + 1 + n]
            if prev is not None and key <= prev:
                raise NotCanonical("unsorted or duplicate key %r" % key)
            prev = key
            i = strict_parse(b, j + 1 + n, depth + 1)
        return i + 1
    if c.isdigit():
        j = b.index(b":", i)
        s = b[i:j]
        if s.startswith(b"0") and s != b"0":
            raise NotCanonical("non-canonical length")
        return j + 1 + int(s)
    raise NotCanonical("bad token %r" % c)


def info_span(b):
    """Find the raw info dict inside a top-level dict (lenient walk)."""
    def skip(i):
        c = b[i:i + 1]
        if c == b"i":
            return b.index(b"e", i) + 1
        if c in (b"l", b"d"):
            i += 1
            while b[i:i + 1] != b"e":
                i = skip(i)
            return i + 1
        j = b.index(b":", i)
        return j + 1 + int(b[i:j])
    if b[:1] != b"d":
        return None
    i = 1
    while b[i:i + 1] != b"e":
        j = b.index(b":", i)
        n = int(b[i:j])
        key = b[j + 1:j + 1 + n]
        start = j + 1 + n
        end = skip(start)
        if key == b"info":
            return b[start:end]
        i = end
    return None


def strict_info_ok(path):
    data = open(path, "rb").read()
    try:
        span = info_span(data)
    except Exception:  # noqa: BLE001
        return None
    if span is None:
        return None
    try:
        strict_parse(span)
        return True
    except (NotCanonical, ValueError):
        return False


def main():
    oracle = {}
    for line in open(os.path.join(HERE, "oracle.jsonl")):
        rec = json.loads(line)
        rel = os.path.relpath(rec["file"], "/h/fixtures/torrents")
        oracle[rel] = rec

    out = {}
    problems = []
    for rel, verdict in sorted(VERDICTS.items()):
        if rel not in oracle:
            problems.append(f"{rel}: no oracle output")
            continue
        lt = oracle[rel]["libtorrent"]
        tr = oracle[rel]["transmission"]
        strict = strict_info_ok(os.path.join(HERE, rel))
        if verdict == ACCEPT:
            if not lt["valid"] or not lt["hasV1"]:
                problems.append(f"{rel}: accepted but libtorrent rejects it / no v1")
                continue
            if strict is not True:
                problems.append(f"{rel}: accepted but its info dict is not canonical")
            if tr["infoHashV1"] and tr["infoHashV1"] != lt["infoHashV1"]:
                problems.append(f"{rel}: oracles disagree on the info-hash")
            files = [f for f in lt["files"] if not f["pad"]]
            exp_files = [{"idx": i, "path": f["path"], "sizeBytes": f["size"]} for i, f in enumerate(files)]
            for i, p in enumerate(PATH_OVERRIDES.get(rel, [])):
                if p.startswith("NOT_RESERVED:"):
                    exp_files[i]["path"] = None
                    exp_files[i]["notReservedFrom"] = p.split(":", 1)[1]
                else:
                    exp_files[i]["path"] = p
            name = lt["name"]
            out[rel] = {
                "accept": True,
                "id": lt["infoHashV1"],
                "name": name,
                "private": bool(lt["private"]),
                "pieceLength": lt["pieceLength"],
                "pieceCount": lt["pieceCount"],
                "sizeBytes": sum(f["size"] for f in files),
                "files": exp_files,
                "trackers": lt["trackers"],
                "oracles": {"libtorrent": True, "transmission": tr["infoHashV1"] == lt["infoHashV1"]},
            }
        else:
            rule, why = verdict
            if rule == "BENC-02" and strict is not False:
                problems.append(f"{rel}: expected a strictness violation, parser found none")
            out[rel] = {"accept": False, "rule": rule, "why": why}

    on_disk = set()
    for sub in ("real", "libtorrent", "synth"):
        for f in os.listdir(os.path.join(HERE, sub)):
            if f.endswith(".torrent"):
                on_disk.add(f"{sub}/{f}")
    for missing in sorted(on_disk - set(VERDICTS)):
        problems.append(f"{missing}: fixture on disk without a verdict")

    if problems:
        print("\n".join(problems), file=sys.stderr)
        sys.exit(1)
    with open(os.path.join(HERE, "expected.json"), "w") as f:
        json.dump(out, f, indent=2, ensure_ascii=False, sort_keys=True)
        f.write("\n")
    print(f"expected.json: {sum(v['accept'] for v in out.values())} accept, "
          f"{sum(not v['accept'] for v in out.values())} reject")


if __name__ == "__main__":
    main()
