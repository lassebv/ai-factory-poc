package fakes

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LSDAnnounce is one BEP 14 BT-SEARCH message seen on the group.
type LSDAnnounce struct {
	Time       time.Time
	From       netip.AddrPort
	Host       string
	Port       int
	InfoHashes []string // as sent (BEP 14: 40 hex chars)
	Cookie     string
	Raw        string
	Err        string // malformed message
}

// LSD listens on a BEP 14 multicast group on one interface and can send
// announces to it.
type LSD struct {
	group *net.UDPAddr
	ln    *net.UDPConn
	out   *net.UDPConn

	mu   sync.Mutex
	seen []LSDAnnounce
}

// NewLSD joins group ("239.192.152.143:port") on the named interface.
func NewLSD(group, iface string) (*LSD, error) {
	g, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return nil, err
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenMulticastUDP("udp4", ifi, g)
	if err != nil {
		return nil, err
	}
	out, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		ln.Close()
		return nil, err
	}
	// Send through the loopback interface, never the LAN.
	addrs, _ := ifi.Addrs()
	var ip4 [4]byte
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			copy(ip4[:], ipn.IP.To4())
		}
	}
	rc, err := out.SyscallConn()
	if err == nil {
		rc.Control(func(fd uintptr) {
			err = syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, ip4)
		})
	}
	if err != nil {
		ln.Close()
		out.Close()
		return nil, fmt.Errorf("IP_MULTICAST_IF: %w", err)
	}
	l := &LSD{group: g, ln: ln, out: out}
	go l.loop()
	return l, nil
}

// Close leaves the group.
func (l *LSD) Close() { l.ln.Close(); l.out.Close() }

// Announces returns every message seen on the group (including our own).
func (l *LSD) Announces() []LSDAnnounce {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LSDAnnounce(nil), l.seen...)
}

// WaitFor waits until pred holds for some message.
func (l *LSD) WaitFor(timeout time.Duration, pred func(LSDAnnounce) bool) (LSDAnnounce, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, a := range l.Announces() {
			if pred(a) {
				return a, true
			}
		}
		if time.Now().After(deadline) {
			return LSDAnnounce{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Send multicasts a BT-SEARCH for the info-hashes (hex) advertising port.
func (l *LSD) Send(port int, cookie string, infoHashes ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "BT-SEARCH * HTTP/1.1\r\nHost: %s\r\nPort: %d\r\n", l.group.String(), port)
	for _, h := range infoHashes {
		fmt.Fprintf(&b, "Infohash: %s\r\n", h)
	}
	if cookie != "" {
		fmt.Fprintf(&b, "cookie: %s\r\n", cookie)
	}
	b.WriteString("\r\n\r\n")
	_, err := l.out.WriteToUDP([]byte(b.String()), l.group)
	return err
}

func (l *LSD) loop() {
	buf := make([]byte, 8192)
	for {
		n, from, err := l.ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		a := parseLSD(string(buf[:n]))
		a.Time, a.From = time.Now(), from
		l.mu.Lock()
		l.seen = append(l.seen, a)
		l.mu.Unlock()
	}
}

func parseLSD(s string) LSDAnnounce {
	a := LSDAnnounce{Raw: s}
	sc := bufio.NewScanner(strings.NewReader(s))
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if line != "BT-SEARCH * HTTP/1.1" {
				a.Err = fmt.Sprintf("first line %q", line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "host":
			a.Host = v
		case "port":
			a.Port, _ = strconv.Atoi(v)
		case "infohash":
			a.InfoHashes = append(a.InfoHashes, v)
		case "cookie":
			a.Cookie = v
		}
	}
	return a
}
