package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

// routeTorrents registers the M3 and M4 torrent routes (see contract.go).
func (s *Server) routeTorrents(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/torrents/preview", s.previewTorrent)
	mux.HandleFunc("GET /api/v1/torrents/preview", methodNotAllowed("POST"))
	mux.HandleFunc("GET /api/v1/torrents", s.listTorrents)
	mux.HandleFunc("POST /api/v1/torrents", s.addTorrent)
	mux.HandleFunc("/api/v1/torrents", methodNotAllowed("GET, POST"))
	mux.HandleFunc("GET /api/v1/torrents/{id}", s.getTorrent)
	mux.HandleFunc("/api/v1/torrents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "preview" {
			methodNotAllowed("POST")(w, r)
			return
		}
		methodNotAllowed("GET")(w, r)
	})
	for name, act := range map[string]func(TorrentService) func(context.Context, [20]byte) error{
		"pause":        func(t TorrentService) func(context.Context, [20]byte) error { return t.Pause },
		"resume":       func(t TorrentService) func(context.Context, [20]byte) error { return t.Resume },
		"stop-seeding": func(t TorrentService) func(context.Context, [20]byte) error { return t.StopSeeding },
	} {
		mux.HandleFunc("POST /api/v1/torrents/{id}/"+name, s.lifecycle(act))
		mux.HandleFunc("/api/v1/torrents/{id}/"+name, methodNotAllowed("POST"))
	}
}

// lifecycle handles POST /api/v1/torrents/{id}/{pause,resume,stop-seeding}.
func (s *Server) lifecycle(act func(TorrentService) func(context.Context, [20]byte) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := s.torrents(w)
		if !ok {
			return
		}
		id, ok := parseTorrentID(r.PathValue("id"))
		if !ok {
			WriteError(w, http.StatusNotFound, CodeNotFound, "no such torrent")
			return
		}
		err := act(svc)(r.Context(), id)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, session.ErrNotFound):
			WriteError(w, http.StatusNotFound, CodeNotFound, "no such torrent")
		case errors.Is(err, session.ErrInvalidState):
			WriteError(w, http.StatusConflict, CodeInvalidState, "this action is not valid in the torrent's current state")
		case errors.Is(err, session.ErrNotComplete):
			WriteError(w, http.StatusConflict, CodeNotComplete, "the torrent is not complete")
		default:
			s.log.Error("torrent action", "path", r.URL.Path, "err", err)
			WriteError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		}
	}
}

// torrents returns the TorrentService or writes a 500.
func (s *Server) torrents(w http.ResponseWriter) (TorrentService, bool) {
	if s.opts.Torrents == nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "the torrent engine is not available")
		return nil, false
	}
	return s.opts.Torrents, true
}

// addTorrent handles POST /api/v1/torrents.
func (s *Server) addTorrent(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.torrents(w)
	if !ok {
		return
	}
	data, fields, ok := readUploadForm(w, r)
	if !ok {
		return
	}
	if data == nil {
		missingFile(w, fields)
		return
	}
	opts := session.AddOptions{DownloadDir: fields["downloadDir"]}
	if v, ok := fields["startPaused"]; ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "startPaused must be true or false")
			return
		}
		opts.StartPaused = b
	}
	m, err := metainfo.Parse(data)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidTorrent, err.Error())
		return
	}
	st, err := svc.Add(r.Context(), m, opts)
	switch {
	case err == nil:
		s.log.Info("torrent added", "id", m.HexHash(), "name", m.Info.Name, "downloadDir", st.DownloadDir)
		writeJSON(w, http.StatusCreated, NewTorrent(st))
	case errors.Is(err, session.ErrAlreadyExists):
		writeJSON(w, http.StatusConflict, AlreadyExistsBody{
			Error: ErrorDetail{Code: CodeAlreadyExists, Message: "this torrent has already been added"},
			ID:    m.HexHash(),
		})
	case errors.Is(err, session.ErrInvalidDownloadDir):
		WriteError(w, http.StatusBadRequest, CodeInvalidDownloadDir, err.Error())
	default:
		s.log.Error("add torrent", "id", m.HexHash(), "err", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not add the torrent: "+err.Error())
	}
}

// listTorrents handles GET /api/v1/torrents.
func (s *Server) listTorrents(w http.ResponseWriter, _ *http.Request) {
	svc, ok := s.torrents(w)
	if !ok {
		return
	}
	list := svc.List()
	out := make([]Torrent, 0, len(list))
	for _, st := range list {
		out = append(out, NewTorrent(st))
	}
	writeJSON(w, http.StatusOK, out)
}

// getTorrent handles GET /api/v1/torrents/{id}.
func (s *Server) getTorrent(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.torrents(w)
	if !ok {
		return
	}
	id, ok := parseTorrentID(r.PathValue("id"))
	if !ok {
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such torrent")
		return
	}
	st, ok := svc.Get(id)
	if !ok {
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such torrent")
		return
	}
	writeJSON(w, http.StatusOK, NewTorrent(st))
}

// parseTorrentID parses a TorrentId: 40 lowercase hex characters.
func parseTorrentID(s string) ([20]byte, bool) {
	var id [20]byte
	if len(s) != 40 {
		return id, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return id, false
		}
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, false
	}
	return id, true
}

// NewTorrent maps a session.Status to the Torrent JSON (see contract.go).
func NewTorrent(st session.Status) Torrent {
	size, done := st.SizeBytes, st.CompletedBytes
	t := Torrent{
		ID:              hex.EncodeToString(st.ID[:]),
		Name:            st.Name,
		State:           string(st.State),
		Private:         st.Private,
		HasMetadata:     true,
		SizeBytes:       &size,
		CompletedBytes:  &done,
		Progress:        st.Progress,
		DownloadRate:    st.DownloadRate,
		UploadRate:      st.UploadRate,
		UploadedTotal:   st.UploadedTotal,
		DownloadedTotal: st.DownloadedTotal,
		WastedTotal:     st.WastedTotal,
		PeersConnected:  st.PeersConnected,
		SeedsConnected:  st.SeedsConnected,
		DownloadDir:     st.DownloadDir,
		AddedAt:         st.AddedAt.UTC().Format(time.RFC3339),
	}
	if den := max(st.DownloadedTotal, st.CompletedBytes); den > 0 {
		t.Ratio = float64(st.UploadedTotal) / float64(den)
	}
	if !st.CompletedAt.IsZero() {
		c := st.CompletedAt.UTC().Format(time.RFC3339)
		t.CompletedAt = &c
	}
	if st.State == session.StateError {
		msg := st.ErrorMessage
		if msg == "" {
			msg = "unknown error"
		}
		t.ErrorMessage = &msg
	}
	if st.State == session.StateChecking {
		p := min(max(st.CheckingProgress, 0), 1)
		t.CheckingProgress = &p
	}
	if st.State == session.StateStopped && st.StopReason != session.StopReasonNone {
		r := string(st.StopReason)
		t.StopReason = &r
	}
	return t
}
