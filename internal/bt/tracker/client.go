package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"torrentd/internal/bt/bencode"
)

// defaultHTTPTimeout is the timeout of the client used when none is given.
const defaultHTTPTimeout = 30 * time.Second

// maxResponseBytes caps the size of a tracker response body we read.
const maxResponseBytes = 4 << 20

// maxIntervalSeconds clamps absurd interval values (about 68 years).
const maxIntervalSeconds = 1<<31 - 1

// String returns the event name used in the announce query.
func (e Event) String() string {
	switch e {
	case EventStarted:
		return "started"
	case EventCompleted:
		return "completed"
	case EventStopped:
		return "stopped"
	}
	return ""
}

// Error returns the failure reason.
func (e *FailureError) Error() string {
	return "tracker: failure reason: " + strconv.Quote(e.Reason)
}

type httpClient struct {
	hc *http.Client
}

func newHTTPClient(hc *http.Client) Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &httpClient{hc: hc}
}

// Announce implements Client.
func (c *httpClient) Announce(ctx context.Context, announceURL string, req AnnounceRequest) (*AnnounceResponse, error) {
	full, err := buildAnnounceURL(announceURL, req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("tracker: announce: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("tracker: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker: HTTP status %d", resp.StatusCode)
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("tracker: response too large")
	}
	return parseResponse(body)
}

// buildAnnounceURL appends the announce query to announceURL, keeping any
// query already present.
func buildAnnounceURL(announceURL string, req AnnounceRequest) (string, error) {
	u, err := url.Parse(announceURL)
	if err != nil {
		return "", fmt.Errorf("tracker: bad announce URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("tracker: unsupported announce URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("tracker: announce URL %q has no host", announceURL)
	}
	q := buildQuery(req)
	if u.RawQuery != "" {
		u.RawQuery += "&" + q
	} else {
		u.RawQuery = q
	}
	u.ForceQuery = false
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// buildQuery returns the announce query string (without "?").
func buildQuery(req AnnounceRequest) string {
	var b strings.Builder
	add := func(k, v string) {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(percentEncode([]byte(v)))
	}
	add("info_hash", string(req.InfoHash[:]))
	add("peer_id", string(req.PeerID[:]))
	add("port", strconv.Itoa(int(req.Port)))
	add("uploaded", strconv.FormatInt(req.Uploaded, 10))
	add("downloaded", strconv.FormatInt(req.Downloaded, 10))
	add("left", strconv.FormatInt(req.Left, 10))
	add("compact", "1")
	add("numwant", strconv.Itoa(NumWant))
	if req.Key != "" {
		add("key", req.Key)
	}
	if ev := req.Event.String(); ev != "" {
		add("event", ev)
	}
	if req.TrackerID != "" {
		add("trackerid", req.TrackerID)
	}
	return b.String()
}

// percentEncode encodes every byte outside [A-Za-z0-9.-_~] as %XX.
func percentEncode(p []byte) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(p) * 3)
	for _, c := range p {
		if isUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '.' || c == '-' || c == '_' || c == '~'
}

// parseResponse decodes a bencoded tracker response leniently.
func parseResponse(body []byte) (*AnnounceResponse, error) {
	if len(bytes.TrimSpace(body)) == 0 || body[0] != 'd' {
		return nil, errors.New("tracker: response is not a bencoded dict")
	}
	var m map[string]bencode.RawMessage
	if err := bencode.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("tracker: decoding response: %w", err)
	}
	if raw, ok := m["failure reason"]; ok {
		reason, ok := rawString(raw)
		if !ok {
			reason = "(unreadable failure reason)"
		}
		return nil, &FailureError{Reason: reason}
	}
	r := &AnnounceResponse{Seeders: -1, Leechers: -1}
	if n, ok := rawInt(m["interval"]); ok {
		r.Interval = seconds(n)
	}
	if n, ok := rawInt(m["min interval"]); ok {
		r.MinInterval = seconds(n)
	}
	r.TrackerID, _ = rawString(m["tracker id"])
	r.Warning, _ = rawString(m["warning message"])
	if n, ok := rawInt(m["complete"]); ok && n >= 0 && n <= 1<<31-1 {
		r.Seeders = int(n)
	}
	if n, ok := rawInt(m["incomplete"]); ok && n >= 0 && n <= 1<<31-1 {
		r.Leechers = int(n)
	}
	r.Peers = parsePeers(m["peers"])
	if s, ok := rawString(m["peers6"]); ok {
		r.Peers = append(r.Peers, compactPeers([]byte(s), 16)...)
	}
	return r, nil
}

func seconds(n int64) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(min(n, maxIntervalSeconds)) * time.Second
}

func rawInt(raw bencode.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int64
	if err := bencode.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

func rawString(raw bencode.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := bencode.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// parsePeers reads "peers" in compact (string) or dict-list form.
func parsePeers(raw bencode.RawMessage) []netip.AddrPort {
	if len(raw) == 0 {
		return nil
	}
	if s, ok := rawString(raw); ok {
		return compactPeers([]byte(s), 4)
	}
	var list []map[string]bencode.RawMessage
	if err := bencode.Unmarshal(raw, &list); err != nil {
		return nil
	}
	var out []netip.AddrPort
	for _, d := range list {
		ipText, ok := rawString(d["ip"])
		if !ok {
			continue
		}
		ip, err := netip.ParseAddr(ipText)
		if err != nil || ip.Zone() != "" {
			continue // hostnames are skipped at M3
		}
		port, ok := rawInt(d["port"])
		if !ok || port <= 0 || port > 65535 {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip, uint16(port)))
	}
	return out
}

// compactPeers decodes addrLen-byte addresses each followed by a 2-byte
// big-endian port. A trailing partial entry is ignored; port 0 is dropped.
func compactPeers(p []byte, addrLen int) []netip.AddrPort {
	step := addrLen + 2
	var out []netip.AddrPort
	for i := 0; i+step <= len(p); i += step {
		ip, _ := netip.AddrFromSlice(p[i : i+addrLen])
		port := uint16(p[i+addrLen])<<8 | uint16(p[i+addrLen+1])
		if port == 0 {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip, port))
	}
	return out
}
