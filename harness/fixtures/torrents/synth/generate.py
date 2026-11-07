#!/usr/bin/env python3
"""Generate synthetic .torrent fixtures for rules that real-world files don't
cover. Run from anywhere:  python3 harness/fixtures/torrents/synth/generate.py

The encoder below writes canonical bencode unless a fixture deliberately
breaks a rule (`raw` values are inserted verbatim). Piece hashes are arbitrary
bytes of the right length: the preview endpoint validates structure, not data.
"""
import hashlib
import os
import unicodedata

OUT = os.path.dirname(os.path.abspath(__file__))


class Raw(bytes):
    """Bytes inserted into the output verbatim (for deliberately broken input)."""


def enc(v):
    if isinstance(v, Raw):
        return bytes(v)
    if isinstance(v, bool):
        raise TypeError("no bools in bencode")
    if isinstance(v, int):
        return b"i%de" % v
    if isinstance(v, str):
        v = v.encode()
    if isinstance(v, bytes):
        return b"%d:%s" % (len(v), v)
    if isinstance(v, list):
        return b"l" + b"".join(enc(x) for x in v) + b"e"
    if isinstance(v, dict):
        items = sorted(((k.encode() if isinstance(k, str) else k), x) for k, x in v.items())
        return b"d" + b"".join(enc(k) + enc(x) for k, x in items) + b"e"
    if isinstance(v, tuple):  # ordered dict as a tuple of pairs, written as given
        return b"d" + b"".join(enc(k) + enc(x) for k, x in v) + b"e"
    raise TypeError(type(v))


def pieces(total, plen, seed):
    n = -(-total // plen)
    return b"".join(hashlib.sha1(b"%s-%d" % (seed.encode(), i)).digest() for i in range(n))


def multi(name, files, plen=16384, **extra):
    total = sum(size for _, size in files)
    info = {"name": name, "piece length": plen, "pieces": pieces(total, plen, name),
            "files": [{"length": size, "path": path} for path, size in files]}
    info.update(extra)
    return info


def single(name, length, plen=16384, **extra):
    info = {"name": name, "length": length, "piece length": plen, "pieces": pieces(length, plen, name)}
    info.update(extra)
    return info


def write(fname, info, **top):
    meta = {"info": info, "announce": "http://tracker.example/announce"}
    meta.update(top)
    with open(os.path.join(OUT, fname), "wb") as f:
        f.write(enc(meta))


nfc = unicodedata.normalize("NFC", "café.txt")
nfd = unicodedata.normalize("NFD", "café.txt")

# --- valid -----------------------------------------------------------------
write("private_announce_list.torrent",
      multi("private-set", [(["a.bin"], 40000), (["sub", "b.bin"], 1000)], private=1),
      **{"announce": "http://primary.example/announce",
         "announce-list": [["http://t1a.example/announce", "udp://t1b.example:6969/announce"],
                           ["http://t2.example/announce"]]})
write("unknown_info_keys.torrent",
      single("unknown-keys.bin", 30000, **{"x-custom": "kept", "source": "fixture"}))
write("utf8_names.torrent",
      multi("ascii-name", [(["a.txt"], 100)], **{"name.utf-8": "Ünïcødé"})
      | {"files": [{"length": 100, "path": ["a.txt"], "path.utf-8": ["ä.txt"]}]})
write("windows_names.torrent",
      multi("win", [(["a<b>c:d\"e|f?g*h.txt"], 10), (["trail."], 10), (["space "], 10),
                    (["CON"], 10), (["nul.txt"], 10), (["ok.txt"], 10)]))
write("non_power_of_two_piece.torrent", single("np2.bin", 100000, plen=49152))
write("min_piece_length.torrent", single("min.bin", 20000, plen=16384))

# --- invalid ---------------------------------------------------------------
write("piece_length_too_small.torrent", single("small.bin", 20000, plen=8192))
write("duplicate_case.torrent", multi("dups", [(["Readme.txt"], 10), (["README.txt"], 10)]))
write("duplicate_nfc.torrent", multi("dups", [([nfc], 10), ([nfd], 10)]))
write("pieces_not_multiple_of_20.torrent",
      single("p.bin", 20000) | {"pieces": b"x" * 39})
write("pieces_count_mismatch.torrent",
      single("p.bin", 20000) | {"pieces": pieces(20000, 16384, "x")[:20]})

# Non-canonical integer inside the info dict (strict mode must reject it).
noncanon = single("nc.bin", 425)
noncanon["length"] = Raw(b"i0425e")
write("noncanonical_int.torrent", noncanon)
neg_zero = single("nz.bin", 425)
neg_zero["private"] = Raw(b"i-0e")
write("negative_zero.torrent", neg_zero)

# Duplicate key inside the info dict, written as an ordered tuple.
dup = single("dup.bin", 425)
dup_items = tuple(sorted(((k, v) for k, v in dup.items()))) + (("piece length", 16384),)
with open(os.path.join(OUT, "duplicate_info_key.torrent"), "wb") as f:
    f.write(b"d4:info" + enc(dup_items) + b"e")

with open(os.path.join(OUT, "not_bencode.torrent"), "wb") as f:
    f.write(b"this is not a torrent\n")

print("wrote fixtures to", OUT)
