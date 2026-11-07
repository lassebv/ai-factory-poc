# Independent oracles for fixture expectations: Transmission's transmission-show
# and libtorrent's Python bindings. Expected values in harness/fixtures are
# derived from these tools, never from torrentd's own code.
FROM alpine:3.22
RUN apk add --no-cache transmission-extra py3-libtorrent-rasterbar python3
WORKDIR /work
