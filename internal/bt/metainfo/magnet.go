package metainfo

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Magnet is a parsed BEP 9 magnet URI.
type Magnet struct {
	InfoHash    [20]byte // from xt=urn:btih: (40 hex chars, any case, or 32 base32 chars)
	DisplayName string   // dn, URL-decoded; "" if absent
	Trackers    []string // every tr, URL-decoded, in order, duplicates removed
	Peers       []string // every x.pe as "host:port", in order
}

// ErrInvalidMagnet wraps every ParseMagnet error.
var ErrInvalidMagnet = errors.New("invalid magnet link")

func magnetErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMagnet, fmt.Sprintf(format, args...))
}

// ParseMagnet parses a magnet URI. It returns an error if the scheme is not
// "magnet", if there is no xt=urn:btih:, or if the hash is malformed. Other
// xt values (such as urn:btmh:) are ignored.
func ParseMagnet(uri string) (*Magnet, error) {
	scheme, rest, ok := strings.Cut(uri, ":")
	if !ok || !strings.EqualFold(scheme, "magnet") {
		return nil, magnetErr("scheme is not magnet")
	}
	_, query, _ := strings.Cut(rest, "?")
	m := &Magnet{}
	var haveHash bool
	seen := map[string]bool{}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		k, raw, _ := strings.Cut(part, "=")
		val, err := url.QueryUnescape(raw)
		if err != nil {
			return nil, magnetErr("bad escaping in %q", k)
		}
		switch k {
		case "xt":
			const prefix = "urn:btih:"
			if len(val) < len(prefix) || !strings.EqualFold(val[:len(prefix)], prefix) {
				continue
			}
			if haveHash {
				continue
			}
			h, err := parseBTIH(val[len(prefix):])
			if err != nil {
				return nil, err
			}
			m.InfoHash, haveHash = h, true
		case "dn":
			m.DisplayName = val
		case "tr":
			if val != "" && !seen[val] {
				seen[val] = true
				m.Trackers = append(m.Trackers, val)
			}
		case "x.pe":
			if val != "" {
				m.Peers = append(m.Peers, val)
			}
		}
	}
	if !haveHash {
		return nil, magnetErr("no xt=urn:btih: parameter")
	}
	return m, nil
}

// parseBTIH decodes a v1 info-hash given as 40 hex or 32 base32 characters.
func parseBTIH(s string) ([20]byte, error) {
	var h [20]byte
	switch len(s) {
	case 40:
		b, err := hex.DecodeString(s)
		if err != nil {
			return h, magnetErr("info-hash is not valid hex")
		}
		copy(h[:], b)
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s))
		if err != nil || len(b) != 20 {
			return h, magnetErr("info-hash is not valid base32")
		}
		copy(h[:], b)
	default:
		return h, magnetErr("info-hash has length %d, want 40 (hex) or 32 (base32)", len(s))
	}
	return h, nil
}
