package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"torrentd/internal/bt/metainfo"
)

// MaxTorrentFileSize is the largest accepted .torrent upload (§8.2, §11).
const MaxTorrentFileSize = 10 << 20

// multipartOverhead is the slack allowed on top of MaxTorrentFileSize for
// multipart headers, boundaries and small extra fields.
const multipartOverhead = 1 << 20

// TorrentPreview is the TorrentPreview schema.
type TorrentPreview struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Private     bool             `json:"private"`
	SizeBytes   int64            `json:"sizeBytes"`
	PieceLength int64            `json:"pieceLength"`
	PieceCount  int64            `json:"pieceCount"`
	Files       []PreviewFile    `json:"files"`
	Trackers    []PreviewTracker `json:"trackers"`
}

// PreviewFile is one visible (non-pad) file of a TorrentPreview.
type PreviewFile struct {
	Idx       int    `json:"idx"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

// PreviewTracker is one tracker of a TorrentPreview.
type PreviewTracker struct {
	URL  string `json:"url"`
	Tier int    `json:"tier"`
}

// NewTorrentPreview builds the preview of a parsed torrent.
func NewTorrentPreview(m *metainfo.MetaInfo) TorrentPreview {
	p := TorrentPreview{
		ID:          m.HexHash(),
		Name:        m.Info.Name,
		Private:     m.Info.Private,
		SizeBytes:   m.Info.VisibleLength(),
		PieceLength: m.Info.PieceLength,
		PieceCount:  int64(m.Info.NumPieces()),
		Files:       []PreviewFile{},
		Trackers:    []PreviewTracker{},
	}
	for i, f := range m.Info.VisibleFiles() {
		p.Files = append(p.Files, PreviewFile{Idx: i, Path: f.PathString(), SizeBytes: f.Length})
	}
	for _, t := range m.Trackers {
		p.Trackers = append(p.Trackers, PreviewTracker{URL: t.URL, Tier: t.Tier})
	}
	return p
}

// errTooLarge marks an upload over MaxTorrentFileSize.
var errTooLarge = errors.New("the .torrent file is larger than 10 MiB")

// maxFieldSize bounds a non-file multipart field.
const maxFieldSize = 64 << 10

// formFields are the multipart parts that are read; others are skipped.
var formFields = map[string]bool{"file": true, "magnet": true, "downloadDir": true, "startPaused": true, "filePriorities": true}

// upload is a parsed multipart/form-data request.
type upload struct {
	file    []byte
	hasFile bool
	fields  map[string]string
}

// readTorrentUpload returns the contents of the "file" part of a
// multipart/form-data request. It writes the error response itself and
// returns ok=false on failure.
func readTorrentUpload(w http.ResponseWriter, r *http.Request) (data []byte, ok bool) {
	u, ok := readUpload(w, r)
	if !ok {
		return nil, false
	}
	if !u.hasFile {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "missing file part")
		return nil, false
	}
	return u.file, true
}

// readUpload reads every part of a multipart/form-data request: the "file"
// part (at most MaxTorrentFileSize) and small text fields. It writes the
// error response itself and returns ok=false on failure.
func readUpload(w http.ResponseWriter, r *http.Request) (u upload, ok bool) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "expected a multipart/form-data body with a file part")
		return u, false
	}
	u.fields = map[string]string{}
	body := http.MaxBytesReader(w, r.Body, MaxTorrentFileSize+multipartOverhead)
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return u, true
		}
		if err != nil {
			uploadError(w, err)
			return u, false
		}
		name := part.FormName()
		if !formFields[name] {
			_, err := io.Copy(io.Discard, part)
			_ = part.Close()
			if err != nil {
				uploadError(w, err)
				return u, false
			}
			continue
		}
		limit := int64(maxFieldSize)
		if name == "file" {
			limit = MaxTorrentFileSize
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		_ = part.Close()
		if err == nil && int64(len(data)) > limit {
			if name == "file" {
				err = errTooLarge
			} else {
				err = errors.New("field " + name + " is too large")
			}
		}
		if err != nil {
			uploadError(w, err)
			return u, false
		}
		if name == "file" {
			u.file, u.hasFile = data, true
		} else {
			u.fields[name] = string(data)
		}
	}
}

func uploadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.Is(err, errTooLarge) || errors.As(err, &mbe) {
		WriteError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, errTooLarge.Error())
		return
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "malformed multipart body: "+err.Error())
}

// previewTorrent handles POST /api/v1/torrents/preview: parse and validate a
// .torrent without storing anything.
func (s *Server) previewTorrent(w http.ResponseWriter, r *http.Request) {
	data, ok := readTorrentUpload(w, r)
	if !ok {
		return
	}
	m, err := metainfo.Parse(data)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidTorrent, err.Error())
		return
	}
	if !m.Info.PowerOfTwoPieces() {
		s.log.Info("torrent piece length is not a power of two", "id", m.HexHash(), "pieceLength", m.Info.PieceLength)
	}
	writeJSON(w, http.StatusOK, NewTorrentPreview(m))
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
