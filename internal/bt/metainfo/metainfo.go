// Package metainfo parses and validates .torrent files (BEP 3, §4.1,
// §16.1) and magnet links (BEP 9).
//
// A torrent is handled through its v1 view only (ADR-0001): hybrid torrents
// are accepted, v2-only torrents are rejected, and BEP 47 pad files are kept
// for piece mapping but hidden from the user-visible file list. The info
// dict is decoded in strict bencode mode, and the info-hash is the SHA-1 of
// its original bytes.
package metainfo

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"torrentd/internal/bt/bencode"
)

// Limits from §16.1 A2 and §16.2 T6.
const (
	MinPieceLength = 16 << 10
	MaxPieceLength = 256 << 20
	MaxPieces      = 1 << 21
)

// ErrInvalidTorrent wraps every validation error returned by Parse and
// ParseInfo.
var ErrInvalidTorrent = errors.New("invalid torrent")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTorrent, fmt.Sprintf(format, args...))
}

// File is one entry of a torrent's file list.
type File struct {
	// Path holds the sanitized path components relative to the download
	// dir. For multi-file torrents the first component is the torrent name.
	Path []string
	// Length is the file size in bytes.
	Length int64
	// Offset is the byte offset of the file within the torrent's data.
	Offset int64
	// Pad marks a BEP 47 padding file: never written to disk, hidden from
	// the user and treated as zeros when hashing.
	Pad bool
}

// PathString returns the components joined with "/".
func (f File) PathString() string { return strings.Join(f.Path, "/") }

// Info is the validated v1 view of an info dict.
type Info struct {
	// Name is the sanitized torrent name.
	Name string
	// PieceLength is the nominal piece size.
	PieceLength int64
	// Pieces holds the concatenated 20-byte SHA-1 piece hashes.
	Pieces []byte
	// Private is BEP 27's private flag.
	Private bool
	// MultiFile is true when the info dict has a "files" list.
	MultiFile bool
	// Files lists every file, pad files included, in metainfo order.
	Files []File
	// TotalLength is the sum of all file lengths, pad files included.
	TotalLength int64
}

// NumPieces returns the number of pieces.
func (i *Info) NumPieces() int { return len(i.Pieces) / sha1.Size }

// PieceHash returns the expected SHA-1 of piece n.
func (i *Info) PieceHash(n int) [20]byte {
	var h [20]byte
	copy(h[:], i.Pieces[n*sha1.Size:])
	return h
}

// VisibleFiles returns the non-pad files in metainfo order. Their index in
// the result is the file index ("idx") used everywhere in the API.
func (i *Info) VisibleFiles() []File {
	out := make([]File, 0, len(i.Files))
	for _, f := range i.Files {
		if !f.Pad {
			out = append(out, f)
		}
	}
	return out
}

// VisibleLength returns the total size of the non-pad files.
func (i *Info) VisibleLength() int64 {
	var n int64
	for _, f := range i.Files {
		if !f.Pad {
			n += f.Length
		}
	}
	return n
}

// PowerOfTwoPieces reports whether the piece length is a power of two. A
// non-power-of-two length is legal but worth logging (A2).
func (i *Info) PowerOfTwoPieces() bool {
	return i.PieceLength&(i.PieceLength-1) == 0
}

// Tracker is one announce URL with its BEP 12 tier.
type Tracker struct {
	// URL is the announce URL as written in the metainfo.
	URL string
	// Tier is the 0-based tier index.
	Tier int
}

// MetaInfo is a parsed .torrent file.
type MetaInfo struct {
	// InfoHash is the v1 info-hash: SHA-1 of the raw info dict bytes.
	InfoHash [20]byte
	// RawInfo holds the original bytes of the info dict.
	RawInfo []byte
	// Info is the validated info dict.
	Info *Info
	// Trackers lists announce-list tiers in file order, or announce as tier
	// 0 when there is no usable announce-list. Duplicates are removed.
	Trackers []Tracker
}

// HexHash returns the info-hash as 40 lowercase hex characters.
func (m *MetaInfo) HexHash() string { return hex.EncodeToString(m.InfoHash[:]) }

// metaDict is the top-level dict. Tracker fields are decoded separately so a
// malformed optional field doesn't make the whole file unreadable.
type metaDict struct {
	Announce     bencode.RawMessage `bencode:"announce"`
	AnnounceList bencode.RawMessage `bencode:"announce-list"`
	Info         bencode.RawMessage `bencode:"info"`
}

// Parse decodes and validates a .torrent file.
func Parse(data []byte) (*MetaInfo, error) {
	var md metaDict
	if err := bencode.Unmarshal(data, &md); err != nil {
		return nil, invalid("%v", err)
	}
	if len(md.Info) == 0 {
		return nil, invalid("missing info dict")
	}
	info, err := ParseInfo(md.Info)
	if err != nil {
		return nil, err
	}
	m := &MetaInfo{
		InfoHash: sha1.Sum(md.Info),
		RawInfo:  md.Info,
		Info:     info,
		Trackers: trackers(md.Announce, md.AnnounceList),
	}
	return m, nil
}

func trackers(announce, announceList bencode.RawMessage) []Tracker {
	var out []Tracker
	seen := map[string]bool{}
	var tiers [][]string
	if len(announceList) > 0 && bencode.Unmarshal(announceList, &tiers) == nil {
		tier := 0
		for _, t := range tiers {
			added := false
			for _, u := range t {
				u = strings.TrimSpace(u)
				if u == "" || seen[u] {
					continue
				}
				seen[u] = true
				out = append(out, Tracker{URL: u, Tier: tier})
				added = true
			}
			if added {
				tier++
			}
		}
	}
	if len(out) == 0 && len(announce) > 0 {
		var u string
		if bencode.Unmarshal(announce, &u) == nil {
			if u = strings.TrimSpace(u); u != "" {
				out = append(out, Tracker{URL: u, Tier: 0})
			}
		}
	}
	return out
}

// infoDict is the info dict as stored. Pointers distinguish missing keys.
type infoDict struct {
	Name        *string     `bencode:"name"`
	NameUTF8    *string     `bencode:"name.utf-8"`
	PieceLength *int64      `bencode:"piece length"`
	Pieces      *[]byte     `bencode:"pieces"`
	Private     *int64      `bencode:"private"`
	Length      *int64      `bencode:"length"`
	Files       *[]fileDict `bencode:"files"`
	MetaVersion *int64      `bencode:"meta version"`
}

type fileDict struct {
	Length   *int64   `bencode:"length"`
	Path     []string `bencode:"path"`
	PathUTF8 []string `bencode:"path.utf-8"`
	Attr     string   `bencode:"attr"`
}

// ParseInfo decodes (strictly, A1) and validates a raw info dict. It is also
// the entry point for metadata fetched with ut_metadata.
func ParseInfo(raw []byte) (*Info, error) {
	var d infoDict
	if err := bencode.UnmarshalStrict(raw, &d); err != nil {
		return nil, invalid("info dict: %v", err)
	}
	if d.Pieces == nil {
		if d.MetaVersion != nil && *d.MetaVersion >= 2 {
			return nil, invalid("v2-only torrents are not supported")
		}
		return nil, invalid("missing pieces")
	}
	if d.PieceLength == nil {
		return nil, invalid("missing piece length")
	}
	pl := *d.PieceLength
	if pl < MinPieceLength || pl > MaxPieceLength {
		return nil, invalid("piece length %d is outside %d..%d", pl, MinPieceLength, MaxPieceLength)
	}

	name := ""
	switch {
	case d.NameUTF8 != nil && *d.NameUTF8 != "":
		name = *d.NameUTF8
	case d.Name != nil:
		name = *d.Name
	}
	if name == "" {
		return nil, invalid("missing name")
	}
	if err := checkComponent(name); err != nil {
		return nil, invalid("name: %v", err)
	}
	info := &Info{
		Name:        sanitize(name),
		PieceLength: pl,
		Pieces:      *d.Pieces,
		Private:     d.Private != nil && *d.Private == 1,
	}

	switch {
	case d.Files != nil:
		info.MultiFile = true
		if err := info.addFiles(*d.Files); err != nil {
			return nil, err
		}
	case d.Length != nil:
		if *d.Length <= 0 {
			return nil, invalid("length %d must be positive", *d.Length)
		}
		info.Files = []File{{Path: []string{info.Name}, Length: *d.Length}}
		info.TotalLength = *d.Length
	default:
		return nil, invalid("neither length nor files")
	}
	if info.VisibleLength() <= 0 {
		return nil, invalid("torrent has no content")
	}

	if len(info.Pieces)%sha1.Size != 0 {
		return nil, invalid("pieces length %d is not a multiple of 20", len(info.Pieces))
	}
	want := (info.TotalLength + pl - 1) / pl
	if want > MaxPieces {
		return nil, invalid("%d pieces exceeds the limit of %d", want, MaxPieces)
	}
	if int64(info.NumPieces()) != want {
		return nil, invalid("%d piece hashes for %d pieces", info.NumPieces(), want)
	}
	return info, nil
}

func (info *Info) addFiles(files []fileDict) error {
	if len(files) == 0 {
		return invalid("empty files list")
	}
	seen := map[string]bool{}
	var offset int64
	for i, fd := range files {
		if fd.Length == nil {
			return invalid("file %d: missing length", i)
		}
		if *fd.Length < 0 {
			return invalid("file %d: negative length", i)
		}
		if *fd.Length > math.MaxInt64-offset {
			return invalid("total size overflows")
		}
		comps := fd.Path
		if len(fd.PathUTF8) > 0 {
			comps = fd.PathUTF8
		}
		if len(comps) == 0 {
			return invalid("file %d: missing path", i)
		}
		p := make([]string, 0, len(comps)+1)
		p = append(p, info.Name)
		for _, c := range comps {
			if err := checkComponent(c); err != nil {
				return invalid("file %d: %v", i, err)
			}
			p = append(p, sanitize(c))
		}
		f := File{Path: p, Length: *fd.Length, Offset: offset, Pad: strings.Contains(fd.Attr, "p")}
		if !f.Pad {
			k := foldKey(f.PathString())
			if seen[k] {
				return invalid("duplicate path %q", f.PathString())
			}
			seen[k] = true
		}
		info.Files = append(info.Files, f)
		offset += f.Length
	}
	info.TotalLength = offset
	return nil
}

// checkComponent rejects path components that could escape the download dir
// or are otherwise unusable (A3).
func checkComponent(c string) error {
	switch {
	case c == "":
		return errors.New("empty path component")
	case c == "." || c == "..":
		return fmt.Errorf("path component %q", c)
	case strings.ContainsAny(c, "/\\"):
		return fmt.Errorf("path component %q contains a separator", c)
	case strings.ContainsRune(c, 0):
		return fmt.Errorf("path component %q contains NUL", c)
	}
	return nil
}
