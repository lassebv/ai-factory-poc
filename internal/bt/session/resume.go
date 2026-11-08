package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/storage"
)

// fileStat is one file's entry in the resume fingerprint (§7.1
// resume_fingerprint: per-file size and mtime at the last flush).
type fileStat struct {
	Size    int64 `json:"size"`
	MtimeNs int64 `json:"mtimeNs"`
	exists  bool
}

// fileStats are the stats of a torrent's non-pad files, in info order.
type fileStats []fileStat

// statFiles stats every non-pad file of info under dir, under its final
// name or its .part name.
func statFiles(info *metainfo.Info, dir string) fileStats {
	var out fileStats
	for _, f := range info.Files {
		if f.Pad {
			continue
		}
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		fi, err := os.Stat(p)
		if err != nil {
			fi, err = os.Stat(p + storage.PartSuffix)
		}
		if err != nil || !fi.Mode().IsRegular() {
			out = append(out, fileStat{})
			continue
		}
		out = append(out, fileStat{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), exists: true})
	}
	return out
}

// allMissing reports whether no file exists (G8). A torrent without files
// is never "missing".
func (fs fileStats) allMissing() bool {
	for _, f := range fs {
		if f.exists {
			return false
		}
	}
	return len(fs) > 0
}

// anyData reports whether some file already exists with data in it.
func (fs fileStats) anyData() bool {
	for _, f := range fs {
		if f.exists && f.Size > 0 {
			return true
		}
	}
	return false
}

// matches compares the files with a saved fingerprint: sizes always, and
// mtimes too after a clean shutdown (ADR-0002). An unknown or malformed
// fingerprint never matches.
func (fs fileStats) matches(fingerprint string, clean bool) bool {
	var saved []fileStat
	if fingerprint == "" || json.Unmarshal([]byte(fingerprint), &saved) != nil || len(saved) != len(fs) {
		return false
	}
	for i, f := range fs {
		if !f.exists || f.Size != saved[i].Size || (clean && f.MtimeNs != saved[i].MtimeNs) {
			return false
		}
	}
	return true
}

// fingerprint returns the current resume fingerprint of the files.
func fingerprint(info *metainfo.Info, dir string) string {
	b, err := json.Marshal(statFiles(info, dir))
	if err != nil {
		return ""
	}
	return string(b)
}

// wantedBytes is storage.WantedBytes without an open Storage: the bytes of
// piece index that belong to non-pad files.
func wantedBytes(info *metainfo.Info, index int) int64 {
	if index < 0 || index >= info.NumPieces() {
		return 0
	}
	start := int64(index) * info.PieceLength
	end := min(start+info.PieceLength, info.TotalLength)
	var n int64
	for _, f := range info.Files {
		if f.Pad || f.Length == 0 {
			continue
		}
		a, b := max(start, f.Offset), min(end, f.Offset+f.Length)
		if b > a {
			n += b - a
		}
	}
	return n
}
