package app

import (
	"fmt"
	"net"
	"runtime"
	"sync"
)

// Hermetic defaults (T3, ADR-0020). From milestone 5 on, torrentd starts DHT
// and LSD by default. Without these defaults every black-box test would
// bootstrap from the public D4 routers and multicast on the LAN. Every
// instance therefore gets, through environment variables (which a pre-M5
// binary simply ignores, unlike unknown flags):
//
//   - TORRENTD_DHT_BOOTSTRAP: a harness-owned UDP socket that swallows every
//     packet (not an empty value: "empty" vs "unset" is easy to confuse);
//   - TORRENTD_LSD_INTERFACE: the loopback interface;
//   - TORRENTD_LSD_ADDR: the BEP 14 group on a private port per instance.
//
// Tests override any of these with Options.Env.

var (
	discardOnce sync.Once
	discardAddr string
)

// DiscardUDP returns the address of a UDP socket that reads and drops
// everything. It lives as long as the test process.
func DiscardUDP() string {
	discardOnce.Do(func() {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			panic(err)
		}
		discardAddr = c.LocalAddr().String()
		go func() {
			buf := make([]byte, 65536)
			for {
				if _, _, err := c.ReadFrom(buf); err != nil {
					return
				}
			}
		}()
	})
	return discardAddr
}

// LoopbackInterface is the name of the loopback interface (lo0 on macOS and
// the BSDs, lo on Linux).
func LoopbackInterface() string {
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifi := range ifs {
			if ifi.Flags&net.FlagLoopback != 0 && ifi.Flags&net.FlagUp != 0 {
				return ifi.Name
			}
		}
	}
	if runtime.GOOS == "linux" {
		return "lo"
	}
	return "lo0"
}

// FreeUDPPort returns a UDP port on 127.0.0.1 that was free a moment ago.
func FreeUDPPort() int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func hermeticLSDGroup() string {
	return fmt.Sprintf("239.192.152.143:%d", FreeUDPPort())
}

func hermeticEnv(lsdGroup string) []string {
	return []string{
		"TORRENTD_DHT_BOOTSTRAP=" + DiscardUDP(),
		"TORRENTD_LSD_INTERFACE=" + LoopbackInterface(),
		"TORRENTD_LSD_ADDR=" + lsdGroup,
	}
}
