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

// maxFormField bounds a non-file multipart field (downloadDir etc.).
const maxFormField = 64 << 10

// readTorrentUpload returns the contents of the "file" part of a
// multipart/form-data request. It writes the error response itself and
// returns ok=false on failure.
func readTorrentUpload(w http.ResponseWriter, r *http.Request) (data []byte, ok bool) {
	data, fields, ok := readUploadForm(w, r)
	if ok && data == nil {
		missingFile(w, fields)
		return nil, false
	}
	return data, ok
}

// missingFile writes the 400 for a form without a "file" part.
func missingFile(w http.ResponseWriter, fields map[string]string) {
	if _, ok := fields["magnet"]; ok {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "magnet links are not supported yet; upload a .torrent file")
		return
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "missing file part")
}

// readUploadForm reads a multipart/form-data request: the "file" part (nil
// if absent) and every other field as a string. It writes the error
// response itself and returns ok=false on failure.
func readUploadForm(w http.ResponseWriter, r *http.Request) (data []byte, fields map[string]string, ok bool) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "expected a multipart/form-data body with a file part")
		return nil, nil, false
	}
	body := http.MaxBytesReader(w, r.Body, MaxTorrentFileSize+multipartOverhead)
	mr := multipart.NewReader(body, params["boundary"])
	fields = map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return data, fields, true
		}
		if err != nil {
			uploadError(w, err)
			return nil, nil, false
		}
		name := part.FormName()
		if name != "file" {
			v, err := io.ReadAll(io.LimitReader(part, maxFormField+1))
			if err == nil && len(v) > maxFormField {
				_, err = io.Copy(io.Discard, part)
			}
			_ = part.Close()
			if err == nil && len(v) > maxFormField {
				WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "form field "+name+" is too large")
				return nil, nil, false
			}
			if err != nil {
				uploadError(w, err)
				return nil, nil, false
			}
			if name != "" {
				fields[name] = string(v)
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, MaxTorrentFileSize+1))
		_ = part.Close()
		if err == nil && len(b) > MaxTorrentFileSize {
			err = errTooLarge
		}
		if err != nil {
			uploadError(w, err)
			return nil, nil, false
		}
		if b == nil {
			b = []byte{}
		}
		data = b
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
