#!/usr/bin/env python3
"""libtorrent driver for the M5 oracle checks (harness/acceptance/oraclecheck).

  python3 lt_m5.py udp    <torrent> <save> <port>
  python3 lt_m5.py dht    <torrent> <save> <port> <bootstrap host:port>
  python3 lt_m5.py magnet <magnet-uri> <save> <port> <peer host:port>
  python3 lt_m5.py seed   <torrent> <save> <port>
  python3 lt_m5.py leech  <torrent> <save> <port>

Prints one line per interesting event (TRACKER_REPLY, SCRAPE, PEER, METADATA,
FINISHED, DHT_PORT, ...) and runs until killed. Everything listens on
127.0.0.1 inside the container; the Go fakes run next to it.
"""
import sys
import time

import libtorrent as lt

mode, target, save, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
settings = {
    "listen_interfaces": f"127.0.0.1:{port}",
    "enable_dht": mode == "dht",
    "enable_lsd": False, "enable_upnp": False, "enable_natpmp": False,
    "enable_incoming_utp": False, "enable_outgoing_utp": False,
    "allow_multiple_connections_per_ip": True,
    # Every fake lives on 127.0.0.1: don't collapse nodes/peers by IP.
    "dht_restrict_routing_ips": False, "dht_restrict_search_ips": False,
    "dht_bootstrap_nodes": sys.argv[5] if mode == "dht" else "",
    "alert_mask": lt.alert.category_t.all_categories,
}
ses = lt.session(settings)

if mode == "magnet":
    atp = lt.parse_magnet_uri(target)
else:
    atp = lt.add_torrent_params()
    atp.ti = lt.torrent_info(target)
atp.save_path = save
h = ses.add_torrent(atp)
if mode == "magnet":
    host, p = sys.argv[5].rsplit(":", 1)
    h.connect_peer((host, int(p)))

print("STARTED", flush=True)
scraped = False
seeding = False
deadline = time.time() + 600
while time.time() < deadline:
    st = h.status()
    if mode == "seed" and st.is_seeding and not seeding:
        seeding = True
        print("SEEDING", flush=True)
    for a in ses.pop_alerts():
        if isinstance(a, lt.tracker_reply_alert):
            print("TRACKER_REPLY", a.num_peers, flush=True)
            if not scraped:
                scraped = True
                h.scrape_tracker()
        elif isinstance(a, lt.scrape_reply_alert):
            print("SCRAPE", a.complete, a.incomplete, flush=True)
        elif isinstance(a, lt.scrape_failed_alert):
            print("SCRAPE_FAILED", a.error_message(), flush=True)
        elif isinstance(a, lt.tracker_error_alert):
            print("TRACKER_ERROR", a.message(), flush=True)
        elif isinstance(a, lt.peer_connect_alert):
            print("PEER", a.endpoint[0], a.endpoint[1], flush=True)
        elif isinstance(a, lt.metadata_received_alert):
            print("METADATA", flush=True)
        elif isinstance(a, lt.metadata_failed_alert):
            print("METADATA_FAILED", a.message(), flush=True)
        elif isinstance(a, lt.torrent_finished_alert):
            print("FINISHED", flush=True)
        elif isinstance(a, lt.dht_bootstrap_alert):
            print("DHT_BOOTSTRAP", flush=True)
        elif isinstance(a, lt.dht_reply_alert):
            print("DHT_REPLY", a.num_peers, flush=True)
        elif isinstance(a, lt.listen_succeeded_alert):
            print("LISTEN", a.socket_type, a.port, flush=True)
    time.sleep(0.2)
