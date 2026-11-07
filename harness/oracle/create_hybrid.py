#!/usr/bin/env python3
"""Create the hybrid (v1+v2) fixture with BEP 47 pad files using libtorrent.

  python3 create_hybrid.py <out_dir>

Writes <out_dir>/content/hybrid/... (deterministic random data) and
<out_dir>/hybrid.torrent. Piece size 32 KiB; file sizes are chosen so v2 file
alignment forces pad files between them.
"""
import os
import random
import sys

import libtorrent as lt

out = sys.argv[1]
root = os.path.join(out, "content", "hybrid")
rng = random.Random(20261107)
for rel, size in [("a.bin", 100_000), ("b.bin", 70_000), ("sub/c.bin", 50_000), ("tiny.txt", 13)]:
    p = os.path.join(root, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "wb") as f:
        f.write(bytes(rng.getrandbits(8) for _ in range(size)))
fs = lt.file_storage()
lt.add_files(fs, root)
ct = lt.create_torrent(fs, 32768)  # libtorrent 2.x default: hybrid v1+v2
ct.set_creator("torrentd harness")
lt.set_piece_hashes(ct, os.path.join(out, "content"))
with open(os.path.join(out, "hybrid.torrent"), "wb") as f:
    f.write(lt.bencode(ct.generate()))
ti = lt.torrent_info(os.path.join(out, "hybrid.torrent"))
pads = sum(1 for i in range(ti.files().num_files()) if ti.files().file_flags(i) & lt.file_storage.flag_pad_file)
print("v1", ti.info_hashes().v1, "v2", ti.info_hashes().has_v2(), "files", ti.files().num_files(), "pads", pads)
