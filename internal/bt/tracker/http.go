// Package tracker announces to BitTorrent trackers: HTTP and HTTPS
// announces (BEP 3, BEP 23, BEP 7 peers6) and the BEP 12 tiered announcer.
// UDP trackers (BEP 15) arrive in milestone 5.
package tracker

import (
	"context"
	"encoding/binary"
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

// Announce events.
const (
	EventNone      = ""
	EventStarted   = "started"
	EventCompleted = "completed"
	EventStopped   = "stopped"
)

// DefaultNumWant is the number of peers asked for (D7).
const DefaultNumWant = 50

// maxResponse bounds a tracker response body.
const maxResponse = 4 << 20

// Request is one announce.
type Request struct {
	InfoHash   [20]byte
	PeerID     [20]byte
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      string
	// Key is the per-session random key (D7).
	Key string
	// NumWant is the number of peers wanted; 0 means DefaultNumWant.
	NumWant int
	// TrackerID is echoed back when a previous response carried one.
	TrackerID string
}

// Response is a successful announce response.
type Response struct {
	Interval    time.Duration
	MinInterval time.Duration
	TrackerID   string
	Warning     string
	// Seeders and Leechers are the tracker's complete/incomplete counts,
	// -1 when absent.
	Seeders, Leechers int
	Peers             []netip.AddrPort
}

// FailureError is a tracker's "failure reason".
type FailureError struct{ Reason string }

func (e *FailureError) Error() string { return "tracker failure: " + e.Reason }

// ErrMalformed is returned for a response that isn't a valid announce
// response.
var ErrMalformed = errors.New("tracker: malformed response")

// AnnounceURL builds the GET URL for an HTTP announce.
func AnnounceURL(base string, r Request) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("tracker: unsupported scheme %q", u.Scheme)
	}
	numWant := r.NumWant
	if numWant == 0 {
		numWant = DefaultNumWant
	}
	q := []string{
		"info_hash=" + url.QueryEscape(string(r.InfoHash[:])),
		"peer_id=" + url.QueryEscape(string(r.PeerID[:])),
		"port=" + strconv.Itoa(r.Port),
		"uploaded=" + strconv.FormatInt(r.Uploaded, 10),
		"downloaded=" + strconv.FormatInt(r.Downloaded, 10),
		"left=" + strconv.FormatInt(r.Left, 10),
		"compact=1",
		"numwant=" + strconv.Itoa(numWant),
	}
	if r.Key != "" {
		q = append(q, "key="+url.QueryEscape(r.Key))
	}
	if r.Event != EventNone {
		q = append(q, "event="+r.Event)
	}
	if r.TrackerID != "" {
		q = append(q, "trackerid="+url.QueryEscape(r.TrackerID))
	}
	s := strings.Join(q, "&")
	if u.RawQuery != "" {
		s = u.RawQuery + "&" + s
	}
	u.RawQuery = s
	u.Fragment = ""
	return u.String(), nil
}

// AnnounceHTTP sends one HTTP(S) announce.
func AnnounceHTTP(ctx context.Context, client *http.Client, base string, r Request) (*Response, error) {
	full, err := AnnounceURL(base, r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "torrentd/0.1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponse {
		return nil, fmt.Errorf("%w: response too large", ErrMalformed)
	}
	out, perr := ParseResponse(body)
	if resp.StatusCode != http.StatusOK {
		var fe *FailureError
		if errors.As(perr, &fe) {
			return nil, perr
		}
		return nil, fmt.Errorf("tracker: HTTP %d", resp.StatusCode)
	}
	return out, perr
}

type rawResponse struct {
	Failure     *string            `bencode:"failure reason"`
	Warning     *string            `bencode:"warning message"`
	Interval    *int64             `bencode:"interval"`
	MinInterval *int64             `bencode:"min interval"`
	TrackerID   *string            `bencode:"tracker id"`
	Complete    *int64             `bencode:"complete"`
	Incomplete  *int64             `bencode:"incomplete"`
	Peers       bencode.RawMessage `bencode:"peers"`
	Peers6      bencode.RawMessage `bencode:"peers6"`
}

type dictPeer struct {
	IP   string `bencode:"ip"`
	Port int64  `bencode:"port"`
}

// ParseResponse decodes an announce response (lenient bencode, A1).
func ParseResponse(body []byte) (*Response, error) {
	var raw rawResponse
	if err := bencode.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if raw.Failure != nil {
		return nil, &FailureError{Reason: *raw.Failure}
	}
	r := &Response{Seeders: -1, Leechers: -1}
	if raw.Interval != nil && *raw.Interval > 0 {
		r.Interval = time.Duration(min(*raw.Interval, 1<<31)) * time.Second
	}
	if raw.MinInterval != nil && *raw.MinInterval > 0 {
		r.MinInterval = time.Duration(min(*raw.MinInterval, 1<<31)) * time.Second
	}
	if raw.Warning != nil {
		r.Warning = *raw.Warning
	}
	if raw.TrackerID != nil {
		r.TrackerID = *raw.TrackerID
	}
	if raw.Complete != nil {
		r.Seeders = int(max(0, min(*raw.Complete, 1<<31)))
	}
	if raw.Incomplete != nil {
		r.Leechers = int(max(0, min(*raw.Incomplete, 1<<31)))
	}
	var err error
	if r.Peers, err = parsePeers(raw.Peers, 4); err != nil {
		return nil, err
	}
	p6, err := parsePeers(raw.Peers6, 16)
	if err != nil {
		return nil, err
	}
	r.Peers = append(r.Peers, p6...)
	return r, nil
}

// parsePeers decodes a compact peer string (addrLen-byte addresses plus a
// 2-byte port) or a BEP 3 list of peer dicts.
func parsePeers(raw bencode.RawMessage, addrLen int) ([]netip.AddrPort, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []netip.AddrPort
	if raw[0] == 'l' {
		var list []dictPeer
		if err := bencode.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%w: peers: %v", ErrMalformed, err)
		}
		for _, p := range list {
			ip, err := netip.ParseAddr(p.IP)
			if err != nil || p.Port <= 0 || p.Port > 65535 {
				continue // a hostname or a bad entry: skip it
			}
			out = append(out, netip.AddrPortFrom(ip.Unmap(), uint16(p.Port)))
		}
		return out, nil
	}
	var b []byte
	if err := bencode.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%w: peers: %v", ErrMalformed, err)
	}
	step := addrLen + 2
	if len(b)%step != 0 {
		return nil, fmt.Errorf("%w: compact peers length %d", ErrMalformed, len(b))
	}
	for i := 0; i+step <= len(b); i += step {
		ip, _ := netip.AddrFromSlice(b[i : i+addrLen])
		port := binary.BigEndian.Uint16(b[i+addrLen:])
		if port == 0 {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), port))
	}
	return out, nil
}
