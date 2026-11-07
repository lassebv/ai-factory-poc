#!/usr/bin/env python3
"""Seed a torrent with libtorrent (the interop oracle for SP-01).

  python3 seed.py <torrent> <save_path> <listen_port>

Prints "SEEDING" once the data has been verified, then keeps seeding.
DHT, LSD, UPnP, NAT-PMP and uTP are off: torrentd only speaks plain TCP at M3.
"""
import sys
import time

import libtorrent as lt

torrent, save, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
ses = lt.session({
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
    if st.errc.value() != 0:
        print("ERROR", st.errc.message(), flush=True)
    for a in ses.pop_alerts():
        if a.category() & lt.alert.category_t.error_notification:
            print("ALERT", a.message(), flush=True)
    time.sleep(0.5)
