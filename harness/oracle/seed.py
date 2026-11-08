#!/usr/bin/env python3
"""Seed a torrent with libtorrent (the interop oracle for SP-01).

  python3 seed.py <torrent> <save_path> <listen_port> [unchoke_slots]

Prints "SEEDING" once the data has been verified, then keeps seeding.
DHT, LSD, UPnP, NAT-PMP and uTP are off: torrentd only speaks plain TCP at M3.
"""
import sys
import time

import libtorrent as lt

torrent, save, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
extra = {}
if len(sys.argv) > 4:  # choke-observation validation: N regular slots + 1 optimistic on top
    extra = {"unchoke_slots_limit": int(sys.argv[4]), "num_optimistic_unchoke_slots": 1,
             "optimistic_unchoke_interval": 30, "unchoke_interval": 10}
ses = lt.session({**extra, 
    "listen_interfaces": f"0.0.0.0:{port}",
    "enable_dht": False, "enable_lsd": False, "enable_upnp": False, "enable_natpmp": False,
    "enable_incoming_utp": False, "enable_outgoing_utp": False,
    "allow_multiple_connections_per_ip": True,
    "alert_mask": lt.alert.category_t.error_notification | lt.alert.category_t.status_notification,
})
atp = lt.add_torrent_params()
atp.ti = lt.torrent_info(torrent)
atp.save_path = save
h = ses.add_torrent(atp)
announced = False
while True:
    st = h.status()
    if st.is_seeding and not announced:
        print("SEEDING", flush=True)
        announced = True
    if extra and announced:
        unchoked = sum(1 for p in h.get_peer_info() if not (p.flags & lt.peer_info.choked))
        print("UNCHOKED", unchoked, len(h.get_peer_info()), flush=True)
    if st.errc.value() != 0:
        print("ERROR", st.errc.message(), flush=True)
    for a in ses.pop_alerts():
        if a.category() & lt.alert.category_t.error_notification:
            print("ALERT", a.message(), flush=True)
    time.sleep(0.5)
