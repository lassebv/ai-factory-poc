// Package torrentgen builds deterministic test torrents and their content,
// and describes any v1 torrent as a flat byte stream (pad files as zeros) so
// the fake peer can serve it.
package torrentgen

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"torrentd.local/harness/acceptance/internal/benc"
)

// File is one file in a torrent, in metainfo order.
type File struct {
	Path []string // components below the torrent name (multi-file) or nil (single-file)
	Size int64
	Pad  bool // BEP 47 pad file (served as zeros, never on disk)
}

// Torrent is a v1 torrent plus everything needed to serve and verify it.
type Torrent struct {
	Bytes       []byte // the .torrent file
	InfoHash    [20]byte
	Name        string
	Multi       bool
	PieceLength int64
	Total       int64 // including pad files
	Pieces      [][20]byte
	Files       []File
	ContentDir  string // directory that holds Name (or the single file)
}

// ID returns the info-hash as lowercase hex.
func (t *Torrent) ID() string { return hex.EncodeToString(t.InfoHash[:]) }

// NumPieces returns the number of pieces.
func (t *Torrent) NumPieces() int { return len(t.Pieces) }

// PieceSize returns the length of piece i.
func (t *Torrent) PieceSize(i int) int64 {
	if i == len(t.Pieces)-1 {
		if r := t.Total % t.PieceLength; r != 0 {
			return r
		}
	}
	return t.PieceLength
}

// FilePath returns where a file lives below a download (or content) dir.
func (t *Torrent) FilePath(dir string, f File) string {
	if !t.Multi {
		return filepath.Join(dir, t.Name)
	}
	return filepath.Join(append([]string{dir, t.Name}, f.Path...)...)
}

// ReadAt reads len(p) bytes of the concatenated content at off.
func (t *Torrent) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	var pos int64
	for _, f := range t.Files {
		if off+int64(n) >= pos+f.Size || n == len(p) {
			pos += f.Size
			if n == len(p) {
				break
			}
			continue
		}
		start := off + int64(n) - pos
		want := min(int64(len(p)-n), f.Size-start)
		if f.Pad {
			for i := int64(0); i < want; i++ {
				p[n+int(i)] = 0
			}
		} else {
			fh, err := os.Open(t.FilePath(t.ContentDir, f))
			if err != nil {
				return n, err
			}
			_, err = fh.ReadAt(p[n:n+int(want)], start)
			fh.Close()
			if err != nil && err != io.EOF {
				return n, err
			}
		}
		n += int(want)
		pos += f.Size
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Piece returns the data of piece i.
func (t *Torrent) Piece(i int) ([]byte, error) {
	buf := make([]byte, t.PieceSize(i))
	_, err := t.ReadAt(buf, int64(i)*t.PieceLength)
	return buf, err
}

// Spec describes a torrent to generate.
type Spec struct {
	Name         string
	Files        []File // nil Path + one file = single-file torrent
	PieceLength  int64
	Announce     string
	AnnounceList [][]string
	Seed         uint64
	Private      bool
}

// Generate writes deterministic content for spec under dir and builds the
// matching v1 .torrent.
func Generate(dir string, s Spec) (*Torrent, error) {
	rng := rand.New(rand.NewPCG(s.Seed, s.Seed^0x9e3779b97f4a7c15))
	multi := !(len(s.Files) == 1 && s.Files[0].Path == nil)
	t := &Torrent{Name: s.Name, Multi: multi, PieceLength: s.PieceLength, Files: s.Files, ContentDir: dir}
	for _, f := range s.Files {
		if f.Pad {
			return nil, errors.New("torrentgen: Generate does not create pad files")
		}
		p := t.FilePath(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		data := make([]byte, f.Size)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, err
		}
		t.Total += f.Size
	}
	var pieces bytes.Buffer
	n := int((t.Total + s.PieceLength - 1) / s.PieceLength)
	t.Pieces = make([][20]byte, n)
	for i := 0; i < n; i++ {
		data, err := t.Piece(i)
		if err != nil {
			return nil, err
		}
		t.Pieces[i] = sha1.Sum(data)
		pieces.Write(t.Pieces[i][:])
	}
	info := map[string]any{"name": s.Name, "piece length": s.PieceLength, "pieces": pieces.Bytes()}
	if s.Private {
		info["private"] = 1
	}
	if multi {
		var files []any
		for _, f := range s.Files {
			files = append(files, map[string]any{"length": f.Size, "path": f.Path})
		}
		info["files"] = files
	} else {
		info["length"] = s.Files[0].Size
	}
	infoBytes := benc.Encode(info)
	t.InfoHash = sha1.Sum(infoBytes)
	meta := map[string]any{"announce": s.Announce}
	if len(s.AnnounceList) > 0 {
		var tiers []any
		for _, tier := range s.AnnounceList {
			tiers = append(tiers, tier)
		}
		meta["announce-list"] = tiers
	}
	// Splice the exact info bytes in so the hash matches what we computed.
	top := benc.Encode(meta)
	t.Bytes = append(append(append([]byte{}, top[:len(top)-1]...), []byte("4:info")...), infoBytes...)
	t.Bytes = append(t.Bytes, 'e')
	// "info" sorts after "announce"/"announce-list", so the dict stays canonical.
	return t, nil
}

// Load describes an existing v1 (or hybrid) .torrent whose content lives in
// contentDir. The announce URL is replaced with announce.
func Load(torrentPath, contentDir, announce string) (*Torrent, error) {
	data, err := os.ReadFile(torrentPath)
	if err != nil {
		return nil, err
	}
	v, infoRaw, err := benc.Decode(data)
	if err != nil {
		return nil, err
	}
	top, _ := v.(map[string]any)
	info, _ := top["info"].(map[string]any)
	if info == nil || infoRaw == nil {
		return nil, errors.New("torrentgen: no info dict")
	}
	t := &Torrent{ContentDir: contentDir, InfoHash: sha1.Sum(infoRaw)}
	t.Name, _ = info["name"].(string)
	t.PieceLength, _ = info["piece length"].(int64)
	pieces, _ := info["pieces"].(string)
	for i := 0; i+20 <= len(pieces); i += 20 {
		var h [20]byte
		copy(h[:], pieces[i:i+20])
		t.Pieces = append(t.Pieces, h)
	}
	if files, ok := info["files"].([]any); ok {
		t.Multi = true
		for _, fv := range files {
			fm := fv.(map[string]any)
			var p []string
			for _, c := range fm["path"].([]any) {
				p = append(p, c.(string))
			}
			attr, _ := fm["attr"].(string)
			size := fm["length"].(int64)
			t.Files = append(t.Files, File{Path: p, Size: size, Pad: strings.Contains(attr, "p")})
			t.Total += size
		}
	} else {
		size := info["length"].(int64)
		t.Files = []File{{Size: size}}
		t.Total = size
	}
	// Rebuild the .torrent with our announce URL and the original info bytes.
	meta := benc.Encode(map[string]any{"announce": announce})
	t.Bytes = append(append(append([]byte{}, meta[:len(meta)-1]...), []byte("4:info")...), infoRaw...)
	t.Bytes = append(t.Bytes, 'e')
	return t, nil
}

// Verify compares a download dir against the content: every non-pad file
// must match byte-for-byte, and no pad file may exist on disk.
func (t *Torrent) Verify(downloadDir string) error {
	var errs []error
	for _, f := range t.Files {
		got := t.FilePath(downloadDir, f)
		if f.Pad {
			if _, err := os.Stat(got); err == nil {
				errs = append(errs, fmt.Errorf("pad file %s exists on disk", got))
			}
			continue
		}
		want, err := os.ReadFile(t.FilePath(t.ContentDir, f))
		if err != nil {
			return err
		}
		have, err := os.ReadFile(got)
		if err != nil {
			errs = append(errs, fmt.Errorf("missing %s: %v", got, err))
			continue
		}
		if !bytes.Equal(want, have) {
			errs = append(errs, fmt.Errorf("%s differs from the source (%d vs %d bytes)", got, len(have), len(want)))
		}
	}
	return errors.Join(errs...)
}
