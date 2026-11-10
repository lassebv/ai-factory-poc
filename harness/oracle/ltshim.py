#!/usr/bin/env python3
"""libtorrent behind torrentd's CLI + HTTP contract (harness self-validation).

This is NOT a torrentd implementation and never runs in the gate. It lets
the verifier run the black-box discovery suite against an independent,
mature client, to check that the checks themselves can pass:

  HARNESS_TORRENTD_BIN=/oracle/ltshim.py go test ./acceptance/discovery/

(run inside the oracle container; see acceptance/discovery/VALIDATION.md).
Only what the M5 checks read is implemented: /healthz, POST /api/v1/torrents
(file | magnet), GET /api/v1/torrents/{id} (Torrent + files/peers/trackers),
the ADR-0020 flags and their TORRENTD_ variables, and SIGTERM. Known gaps:
LSD (libtorrent's group and port are fixed), DHT state in torrentd.db (G3),
BEP 15 retransmission timing (libtorrent's own).
"""
import base64
import datetime
import email.parser
import email.policy
import json
import os
import signal
import sqlite3
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import libtorrent as lt

FLAGS = {"addr", "data-dir", "peer-port", "log-level", "dht-bootstrap", "lsd-addr", "lsd-interface"}


def parse_args(argv):
    cfg = {}
    i = 0
    while i < len(argv):
        a = argv[i]
        if not a.startswith("--"):
            sys.exit(2)
        k, eq, v = a[2:].partition("=")
        if k not in FLAGS:
            print("unknown flag", a, file=sys.stderr)
            sys.exit(2)
        if not eq:
            i += 1
            v = argv[i]
        cfg[k] = v
        i += 1
    for k in FLAGS:
        env = "TORRENTD_" + k.upper().replace("-", "_")
        if k not in cfg and env in os.environ:
            cfg[k] = os.environ[env]
    return cfg


cfg = parse_args(sys.argv[1:])
if os.environ.get("SHIM_LOGDIR"):
    sys.stderr = open(os.path.join(os.environ["SHIM_LOGDIR"], f"shim-{os.getpid()}.log"), "w", buffering=1)
addr = cfg.get("addr", "127.0.0.1:8080")
port = int(cfg.get("peer-port", "42069"))
data_dir = cfg.get("data-dir", os.path.expanduser("~/.local/share/torrentd"))
os.makedirs(data_dir, exist_ok=True)
db = sqlite3.connect(os.path.join(data_dir, "torrentd.db"), check_same_thread=False)
db.execute("CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
db.execute("CREATE TABLE IF NOT EXISTS dht_nodes (node_id BLOB PRIMARY KEY, addr TEXT NOT NULL, last_seen INTEGER NOT NULL)")
db.commit()

boot = cfg.get("dht-bootstrap", "router.bittorrent.com:6881")
settings = {
    "listen_interfaces": f"127.0.0.1:{port}",  # all fakes are on loopback
    "enable_dht": True, "dht_bootstrap_nodes": boot,
    "enable_lsd": False,  # libtorrent can't move LSD to --lsd-addr; keep the container quiet
    "enable_upnp": False, "enable_natpmp": False,
    "enable_incoming_utp": False, "enable_outgoing_utp": False,
    "allow_multiple_connections_per_ip": True,
    "dht_restrict_routing_ips": False, "dht_restrict_search_ips": False,
    "peer_fingerprint": "-TD0100-", "user_agent": "torrentd 0.1.0",
    "max_allowed_in_request_queue": 250, "max_metadata_size": 10 << 20,
    "stop_tracker_timeout": 3, "announce_to_all_trackers": False, "announce_to_all_tiers": False,
    "alert_mask": lt.alert.category_t.all_categories,
}

# G3 approximation: node id in settings.dht_node_id_v4, endpoints in dht_nodes.
params = lt.session_params()
row = db.execute("SELECT value FROM settings WHERE key='dht_node_id_v4'").fetchone()
if row:
    nodes = [r[0] for r in db.execute("SELECT addr FROM dht_nodes")]
    state = {b"dht state": {b"node-id": [bytes.fromhex(i) for i in json.loads(row[0])], b"nodes": [
        bytes(map(int, a.split(":")[0].split("."))) + int(a.split(":")[1]).to_bytes(2, "big") for a in nodes]}}
    try:
        params = lt.read_session_params(lt.bencode(state))
    except Exception as e:  # noqa
        print("dht state:", e, file=sys.stderr)
else:
    db.execute("DELETE FROM dht_nodes")
    db.commit()
params.settings = settings
ses = lt.session(params)

lock = threading.Lock()
torrents = {}  # id -> dict(handle, added, dir, scraped)


def iso(ts):
    return datetime.datetime.fromtimestamp(ts, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


SOURCES = [("tracker", 0x1), ("dht", 0x2), ("pex", 0x4), ("lsd", 0x8)]


def torrent_json(tid):
    t = torrents[tid]
    h = t["h"]
    st = h.status()
    meta = st.has_metadata
    ti = h.torrent_file() if meta else None
    state = {lt.torrent_status.downloading_metadata: "metadata", lt.torrent_status.checking_files: "checking",
             lt.torrent_status.checking_resume_data: "checking", lt.torrent_status.downloading: "downloading",
             lt.torrent_status.finished: "seeding", lt.torrent_status.seeding: "seeding"}.get(st.state, "downloading")
    if meta and st.is_seeding:
        state = "seeding"
    files, peers = [], []
    if ti:
        fs = ti.files()
        prog = h.file_progress()
        for i in range(fs.num_files()):
            if fs.file_flags(i) & lt.file_storage.flag_pad_file:
                continue
            p = fs.file_path(i).replace("\\", "/").split("/", 1)
            path = p[1] if ti.num_files() > 1 and len(p) > 1 else p[0]
            sz = fs.file_size(i)
            files.append({"idx": len(files), "path": path, "sizeBytes": sz, "completedBytes": prog[i],
                          "progress": (prog[i] / sz) if sz else 1, "priority": "normal"})
    for p in h.get_peer_info():
        src = "incoming"
        if p.flags & lt.peer_info.outgoing_connection:
            src = next((n for n, bit in SOURCES if p.source & bit), "magnet")
        peers.append({"addr": f"{p.ip[0]}:{p.ip[1]}", "client": p.client.decode("utf-8", "replace") if isinstance(p.client, bytes) else p.client,
                      "progress": p.progress, "downloadRate": p.down_speed, "uploadRate": p.up_speed, "source": src,
                      "flags": {"choked": bool(p.flags & lt.peer_info.choked), "interested": bool(p.flags & lt.peer_info.interesting),
                                "peerChoked": bool(p.flags & lt.peer_info.remote_choked), "peerInterested": bool(p.flags & lt.peer_info.remote_interested),
                                "snubbed": bool(p.flags & lt.peer_info.snubbed), "optimistic": bool(p.flags & lt.peer_info.optimistic_unchoke),
                                "incoming": not (p.flags & lt.peer_info.outgoing_connection)}})
    seeders = leechers = None
    for tr in h.trackers():
        for ep in tr.get("endpoints", []):
            for ih in ep.get("info_hashes", [ep]):
                c, i = ih.get("scrape_complete", -1), ih.get("scrape_incomplete", -1)
                if c >= 0:
                    seeders = max(seeders or 0, c)
                if i >= 0:
                    leechers = max(leechers or 0, i)
    size = ti.total_size() if ti else None
    return {
        "id": tid, "name": st.name or tid, "state": state, "stopReason": None, "private": bool(ti and ti.priv()),
        "hasMetadata": bool(meta), "sizeBytes": size, "completedBytes": st.total_wanted_done if meta else None,
        "progress": min(1.0, st.progress) if meta else 0, "checkingProgress": None,
        "downloadRate": st.download_payload_rate, "uploadRate": st.upload_payload_rate,
        "uploadedTotal": st.all_time_upload, "downloadedTotal": st.all_time_download, "wastedTotal": st.total_failed_bytes,
        "ratio": 0, "peersConnected": st.num_peers, "seedsConnected": st.num_seeds,
        "swarmSeeders": seeders, "swarmLeechers": leechers, "etaSeconds": None,
        "downloadDir": t["dir"], "addedAt": iso(t["added"]), "completedAt": None, "seedingSeconds": int(getattr(st.seeding_duration, "total_seconds", lambda: st.seeding_duration)()),
        "ratioLimit": None, "seedTimeLimitMin": None, "downloadLimit": 0, "uploadLimit": 0, "errorMessage": None,
        "files": files, "peers": peers,
        "trackers": [{"url": tr["url"], "tier": tr["tier"], "status": "working", "userAdded": False} for tr in h.trackers()],
        "pieces": "", "pieceLength": ti.piece_length() if ti else None, "pieceCount": ti.num_pieces() if ti else None,
        "maxPeers": None, "limitOverride": False, "queuePosition": 0, "magnetUri": lt.make_magnet_uri(h),
    }


def error(w, status, code, msg):
    w.reply(status, {"error": {"code": code, "message": msg}})


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, status, obj, ctype="application/json"):
        body = obj if isinstance(obj, bytes) else json.dumps(obj, default=str).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        try:
            self.get()
        except Exception:  # noqa
            import traceback
            self.reply(500, {"error": {"code": "internal", "message": traceback.format_exc()}})

    def get(self):
        if self.path == "/healthz":
            return self.reply(200, b"ok", "text/plain")
        if self.path.startswith("/api/v1/torrents/"):
            tid = self.path.rsplit("/", 1)[1]
            with lock:
                if tid in torrents:
                    return self.reply(200, torrent_json(tid))
        error(self, 404, "not_found", "no such torrent")

    def do_POST(self):
        if self.path != "/api/v1/torrents":
            return error(self, 404, "not_found", "no route")
        n = int(self.headers.get("Content-Length", "0"))
        raw = b"Content-Type: " + self.headers["Content-Type"].encode() + b"\r\n\r\n" + self.rfile.read(n)
        msg = email.parser.BytesParser(policy=email.policy.HTTP).parsebytes(raw)
        fields = {}
        for part in msg.iter_parts():
            fields[part.get_param("name", header="content-disposition")] = part.get_payload(decode=True)
        save = (fields.get("downloadDir") or b"").decode() or os.path.join(data_dir, "dl")
        atp = lt.add_torrent_params()
        try:
            if "file" in fields:
                atp.ti = lt.torrent_info(lt.bdecode(fields["file"]))
                tid = str(atp.ti.info_hash())
            else:
                atp = lt.parse_magnet_uri(fields["magnet"].decode())
                tid = str(atp.info_hash)
        except Exception as e:  # noqa
            return error(self, 400, "invalid_torrent", str(e))
        with lock:
            if tid in torrents:
                h = torrents[tid]["h"]
                if atp.ti is not None and not h.status().has_metadata:
                    h.set_metadata(atp.ti.metadata())  # G14
                    for tr in atp.ti.trackers():
                        h.add_tracker({"url": tr.url, "tier": tr.tier})
                return self.reply(409, {"error": {"code": "already_exists", "message": "exists"}, "id": tid})
            os.makedirs(save, exist_ok=True)
            atp.save_path = save
            h = ses.add_torrent(atp)
            torrents[tid] = {"h": h, "added": time.time(), "dir": save, "scraped": False}
            return self.reply(201, torrent_json(tid))


def alerts():
    while True:
        ses.wait_for_alert(500)
        for a in ses.pop_alerts():
            if os.environ.get("SHIM_DEBUG"):
                print("alert:", type(a).__name__, a.message()[:200], file=sys.stderr, flush=True)
            if isinstance(a, lt.tracker_reply_alert):  # A10: scrape once after the first announce
                tid = str(a.handle.info_hash())
                with lock:
                    t = torrents.get(tid)
                    if t and not t["scraped"]:
                        t["scraped"] = True
                        threading.Timer(1.0, a.handle.scrape_tracker, args=(0,)).start()


def shutdown(*_):
    dht = ses.save_state().get(b"dht state", {})
    ids = dht.get(b"node-id", [])
    ids = [ids] if isinstance(ids, bytes) else ids
    if ids:
        db.execute("INSERT OR REPLACE INTO settings VALUES ('dht_node_id_v4', ?)", (json.dumps([i.hex() for i in ids]),))
        db.execute("DELETE FROM dht_nodes")
        for i, n in enumerate(dht.get(b"nodes", [])):
            if len(n) == 6:
                db.execute("INSERT OR REPLACE INTO dht_nodes VALUES (?, ?, ?)",
                           (n, f"{n[0]}.{n[1]}.{n[2]}.{n[3]}:{int.from_bytes(n[4:], 'big')}", int(time.time())))
        db.commit()
    for t in list(torrents.values()):
        t["h"].pause()
    time.sleep(1)
    os._exit(0)


signal.signal(signal.SIGTERM, shutdown)
signal.signal(signal.SIGINT, shutdown)
threading.Thread(target=alerts, daemon=True).start()
host, hp = addr.rsplit(":", 1)
ThreadingHTTPServer((host, int(hp)), H).serve_forever()
