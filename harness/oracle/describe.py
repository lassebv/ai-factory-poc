#!/usr/bin/env python3
"""Describe .torrent files using two independent oracles (libtorrent and
transmission-show) and print one JSON object per file.

Run inside the torrentd-oracle image:
  docker run --rm -v "$PWD/harness:/h" torrentd-oracle:1 \
      python3 /h/oracle/describe.py /h/fixtures/torrents/real/*.torrent
"""
import json
import re
import subprocess
import sys

import libtorrent as lt


def via_libtorrent(path):
    try:
        ti = lt.torrent_info(path)
    except Exception as e:  # noqa: BLE001 - we want the message whatever it is
        return {"valid": False, "error": str(e)}
    hashes = ti.info_hashes()
    fs = ti.files()
    files = []
    for i in range(fs.num_files()):
        files.append({
            "path": fs.file_path(i).replace("\\", "/"),
            "size": fs.file_size(i),
            "pad": bool(fs.file_flags(i) & lt.file_storage.flag_pad_file),
        })
    return {
        "valid": True,
        "hasV1": hashes.has_v1(),
        "hasV2": hashes.has_v2(),
        "infoHashV1": str(hashes.v1) if hashes.has_v1() else None,
        "name": ti.name(),
        "pieceLength": ti.piece_length(),
        "pieceCount": ti.num_pieces(),
        "private": ti.priv(),
        "files": files,
        "trackers": [{"url": t.url, "tier": t.tier} for t in ti.trackers()],
    }


def via_transmission(path):
    p = subprocess.run(["transmission-show", path], capture_output=True, text=True)
    out = p.stdout + p.stderr
    m = re.search(r"Hash(?: v1)?:\s+([0-9a-f]{40})", out)
    return {"exit": p.returncode, "infoHashV1": m.group(1) if m else None,
            "head": out.strip().splitlines()[:3]}


for path in sys.argv[1:]:
    print(json.dumps({"file": path, "libtorrent": via_libtorrent(path),
                      "transmission": via_transmission(path)}))
