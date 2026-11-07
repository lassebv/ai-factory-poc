package metainfo

// Acceptance tests for the pinned magnet API (spec/contracts/go-api.md).
// Owned by the verifier role: implementers must not edit this file.

import (
	"encoding/base32"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

const sintelHex = "08ada5a7a6183aae1e09d831df6748d566095a10"

func sintelHash(t *testing.T) [20]byte {
	t.Helper()
	var h [20]byte
	b, err := hex.DecodeString(sintelHex)
	if err != nil {
		t.Fatal(err)
	}
	copy(h[:], b)
	return h
}

func TestParseMagnet(t *testing.T) {
	h := sintelHash(t)
	b32 := base32.StdEncoding.EncodeToString(h[:])
	cases := []struct {
		name, uri string
		want      Magnet
	}{
		{
			"full hex",
			"magnet:?xt=urn:btih:" + sintelHex + "&dn=Sintel&tr=udp%3A%2F%2Fexplodie.org%3A6969&tr=wss%3A%2F%2Ftracker.btorrent.xyz" +
				"&x.pe=1.2.3.4:6881&x.pe=%5B2001:db8::1%5D:51413",
			Magnet{InfoHash: h, DisplayName: "Sintel",
				Trackers: []string{"udp://explodie.org:6969", "wss://tracker.btorrent.xyz"},
				Peers:    []string{"1.2.3.4:6881", "[2001:db8::1]:51413"}},
		},
		{"upper-case hex", "magnet:?xt=urn:btih:" + strings.ToUpper(sintelHex), Magnet{InfoHash: h}},
		{"base32", "magnet:?xt=urn:btih:" + b32, Magnet{InfoHash: h}},
		{"lower-case base32", "magnet:?xt=urn:btih:" + strings.ToLower(b32), Magnet{InfoHash: h}},
		{"xt not first, spaces in dn", "magnet:?dn=Big+Buck%20Bunny&xt=urn:btih:" + sintelHex, Magnet{InfoHash: h, DisplayName: "Big Buck Bunny"}},
		{"duplicate trackers removed", "magnet:?xt=urn:btih:" + sintelHex + "&tr=http%3A%2F%2Fa%2Fannounce&tr=http%3A%2F%2Fa%2Fannounce&tr=http%3A%2F%2Fb%2Fannounce",
			Magnet{InfoHash: h, Trackers: []string{"http://a/announce", "http://b/announce"}}},
		{"btmh ignored when btih present", "magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32) + "&xt=urn:btih:" + sintelHex, Magnet{InfoHash: h}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := ParseMagnet(c.uri)
			if err != nil {
				t.Fatalf("ParseMagnet: %v", err)
			}
			got := *m
			// nil and empty slices are equivalent.
			if len(got.Trackers) == 0 {
				got.Trackers = nil
			}
			if len(got.Peers) == 0 {
				got.Peers = nil
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ParseMagnet(%q)\n got %+v\nwant %+v", c.uri, got, c.want)
			}
		})
	}
}

func TestParseMagnetInvalid(t *testing.T) {
	for _, uri := range []string{
		"",
		"http://example.com/?xt=urn:btih:" + sintelHex,
		"magnet:",
		"magnet:?dn=only-a-name",
		"magnet:?xt=urn:sha1:" + sintelHex,
		"magnet:?xt=urn:btih:",
		"magnet:?xt=urn:btih:" + sintelHex[:39],
		"magnet:?xt=urn:btih:" + sintelHex + "0",
		"magnet:?xt=urn:btih:" + strings.Repeat("z", 40),
		"magnet:?xt=urn:btih:" + strings.Repeat("1", 32), // '1' is not in the base32 alphabet
		"magnet:?xt=urn:btih:" + strings.Repeat("A", 31),
		"magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32),
	} {
		if m, err := ParseMagnet(uri); err == nil {
			t.Errorf("ParseMagnet(%q) = %+v, want an error", uri, m)
		}
	}
}
