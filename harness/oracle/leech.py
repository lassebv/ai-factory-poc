#!/usr/bin/env python3
"""Download a torrent with libtorrent from one given peer (interop oracle, SEED-02).

  python3 leech.py <torrent> <save_path> <peer_host> <peer_port>

Prints "COMPLETE" when every piece has been downloaded and verified.
"""
import socket
import sys
import time

import libtorrent as lt

torrent, save, host, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
ses = lt.session({
    "listen_interfaces": "0.0.0.0:0",
    "enable_dht": False, "enable_lsd": False, "enable_upnp": False, "enable_natpmp": False,
    "enable_incoming_utp": False, "enable_outgoing_utp": False,
    "alert_mask": lt.alert.category_t.error_notification,
})
atp = lt.add_torrent_params()
atp.ti = lt.torrent_info(torrent)
atp.save_path = save
h = ses.add_torrent(atp)
ip = socket.gethostbyname(host)
deadline = time.time() + 600
while time.time() < deadline:
    st = h.status()
    if not st.num_peers:
        h.connect_peer((ip, port))
    if st.is_seeding:
        print("COMPLETE", st.total_wanted_done, flush=True)
        break
    for a in ses.pop_alerts():
        print("ALERT", a.message(), flush=True)
    time.sleep(0.5)
else:
    print("TIMEOUT", h.status().progress, flush=True)
