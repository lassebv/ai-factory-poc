package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

// Engine is the part of the session the HTTP layer needs.
type Engine interface {
	// Add adds a torrent; it returns session.ErrExists for a duplicate.
	Add(m *metainfo.MetaInfo, downloadDir string, paused bool) (session.Stats, error)
	// Get returns one torrent by hex info-hash.
	Get(id string) (session.Stats, bool)
	// List returns every torrent.
	List() []session.Stats
}

// Torrent is the Torrent schema (§5.4 as amended by §16.5).
type Torrent struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	State            string   `json:"state"`
	StopReason       *string  `json:"stopReason"`
	Private          bool     `json:"private"`
	HasMetadata      bool     `json:"hasMetadata"`
	SizeBytes        *int64   `json:"sizeBytes"`
	CompletedBytes   *int64   `json:"completedBytes"`
	Progress         float64  `json:"progress"`
	CheckingProgress *float64 `json:"checkingProgress"`
	DownloadRate     int64    `json:"downloadRate"`
	UploadRate       int64    `json:"uploadRate"`
	UploadedTotal    int64    `json:"uploadedTotal"`
	DownloadedTotal  int64    `json:"downloadedTotal"`
	WastedTotal      int64    `json:"wastedTotal"`
	Ratio            float64  `json:"ratio"`
	PeersConnected   int      `json:"peersConnected"`
	SeedsConnected   int      `json:"seedsConnected"`
	SwarmSeeders     *int     `json:"swarmSeeders"`
	SwarmLeechers    *int     `json:"swarmLeechers"`
	EtaSeconds       *int64   `json:"etaSeconds"`
	DownloadDir      string   `json:"downloadDir"`
	AddedAt          string   `json:"addedAt"`
	CompletedAt      *string  `json:"completedAt"`
	SeedingSeconds   int64    `json:"seedingSeconds"`
	RatioLimit       *float64 `json:"ratioLimit"`
	SeedTimeLimitMin *int     `json:"seedTimeLimitMin"`
	DownloadLimit    int64    `json:"downloadLimit"`
	UploadLimit      int64    `json:"uploadLimit"`
	ErrorMessage     *string  `json:"errorMessage"`
}

// AlreadyExistsBody is the AlreadyExists schema.
type AlreadyExistsBody struct {
	Error ErrorDetail `json:"error"`
	ID    string      `json:"id"`
}

// NewTorrent converts session stats to the API object.
func NewTorrent(s session.Stats) Torrent {
	t := Torrent{
		ID:              s.ID,
		Name:            s.Name,
		State:           string(s.State),
		Private:         s.Private,
		HasMetadata:     true,
		SizeBytes:       &s.SizeBytes,
		CompletedBytes:  &s.CompletedBytes,
		Progress:        min(max(s.Progress, 0), 1),
		DownloadRate:    s.DownloadRate,
		UploadRate:      s.UploadRate,
		UploadedTotal:   s.UploadedTotal,
		DownloadedTotal: s.DownloadedTotal,
		WastedTotal:     s.WastedTotal,
		PeersConnected:  s.PeersConnected,
		SeedsConnected:  s.SeedsConnected,
		DownloadDir:     s.DownloadDir,
		AddedAt:         s.AddedAt.UTC().Format(time.RFC3339),
		SeedingSeconds:  s.SeedingSeconds,
	}
	// §5.4: uploadedTotal / max(downloadedTotal, completedBytes), 0 when
	// the denominator is 0 (A6).
	if d := max(s.DownloadedTotal, s.CompletedBytes); d > 0 {
		t.Ratio = float64(s.UploadedTotal) / float64(d)
	}
	if s.SwarmSeeders >= 0 {
		t.SwarmSeeders = &s.SwarmSeeders
	}
	if s.SwarmLeechers >= 0 {
		t.SwarmLeechers = &s.SwarmLeechers
	}
	if s.State == session.StateDownloading && s.DownloadRate > 0 {
		eta := (s.SizeBytes - s.CompletedBytes + s.DownloadRate - 1) / s.DownloadRate
		t.EtaSeconds = &eta
	}
	if !s.CompletedAt.IsZero() {
		c := s.CompletedAt.UTC().Format(time.RFC3339)
		t.CompletedAt = &c
	}
	if s.State == session.StateChecking {
		zero := 0.0
		t.CheckingProgress = &zero
	}
	if s.ErrorMessage != "" {
		m := s.ErrorMessage
		t.ErrorMessage = &m
	}
	return t
}

// addTorrent handles POST /api/v1/torrents (§8.2, ADR-0011).
func (s *Server) addTorrent(w http.ResponseWriter, r *http.Request) {
	u, ok := readUpload(w, r)
	if !ok {
		return
	}
	if !u.hasFile {
		if _, ok := u.fields["magnet"]; ok {
			WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "adding magnet links is not supported yet")
			return
		}
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "missing file part")
		return
	}
	m, err := metainfo.Parse(u.file)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidTorrent, err.Error())
		return
	}
	if !m.Info.PowerOfTwoPieces() {
		s.log.Info("torrent piece length is not a power of two", "id", m.HexHash(), "pieceLength", m.Info.PieceLength)
	}
	paused := false
	if v, ok := u.fields["startPaused"]; ok && strings.TrimSpace(v) != "" {
		paused, err = strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "startPaused must be true or false")
			return
		}
	}
	dir := strings.TrimSpace(u.fields["downloadDir"])
	if dir == "" {
		dir = s.opts.DefaultDownloadDir
	}
	if msg := prepareDownloadDir(dir); msg != "" {
		WriteError(w, http.StatusBadRequest, CodeInvalidDownloadDir, msg)
		return
	}
	dir = filepath.Clean(dir)
	if _, ok := s.engine().Get(m.HexHash()); ok {
		s.alreadyExists(w, m.HexHash())
		return
	}
	st, err := s.engine().Add(m, dir, paused)
	if errors.Is(err, session.ErrExists) {
		s.alreadyExists(w, m.HexHash())
		return
	}
	if err != nil {
		s.log.Error("add torrent", "err", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not add the torrent")
		return
	}
	s.log.Info("torrent added", "id", st.ID, "name", st.Name, "downloadDir", dir)
	writeJSON(w, http.StatusCreated, NewTorrent(st))
}

func (s *Server) alreadyExists(w http.ResponseWriter, id string) {
	writeJSON(w, http.StatusConflict, AlreadyExistsBody{
		Error: ErrorDetail{Code: CodeAlreadyExists, Message: "this torrent has already been added"},
		ID:    id,
	})
}

// prepareDownloadDir checks that dir is absolute, creates it with its
// parents, and checks that it is a writable directory. It returns a
// message for the user, or "" when the directory is usable.
func prepareDownloadDir(dir string) string {
	if dir == "" {
		return "downloadDir is required"
	}
	if !filepath.IsAbs(dir) {
		return "downloadDir must be an absolute path"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "cannot create downloadDir: " + err.Error()
	}
	f, err := os.CreateTemp(dir, ".torrentd-write-test-*")
	if err != nil {
		return "downloadDir is not writable: " + err.Error()
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return ""
}

// listTorrents handles GET /api/v1/torrents.
func (s *Server) listTorrents(w http.ResponseWriter, _ *http.Request) {
	all := s.engine().List()
	out := make([]Torrent, 0, len(all))
	for _, st := range all {
		out = append(out, NewTorrent(st))
	}
	writeJSON(w, http.StatusOK, out)
}

// getTorrent handles GET /api/v1/torrents/{id}. At milestone 3 the body is
// the Torrent object (ADR-0011); the TorrentDetail fields arrive in M6.
func (s *Server) getTorrent(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	st, ok := s.engine().Get(id)
	if !ok {
		WriteError(w, http.StatusNotFound, CodeNotFound, "no torrent with id "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, NewTorrent(st))
}

// torrentMethodNotAllowed answers unsupported methods on
// /api/v1/torrents/{id}, including the literal "preview" path.
func torrentMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == "preview" {
		methodNotAllowed("POST")(w, r)
		return
	}
	methodNotAllowed("GET, HEAD")(w, r)
}

// engine returns the configured engine, or an empty one.
func (s *Server) engine() Engine {
	if s.opts.Engine == nil {
		return noEngine{}
	}
	return s.opts.Engine
}

// noEngine is used when the server runs without a session (tests, M1/M2
// configurations): it holds no torrents and refuses to add any.
type noEngine struct{}

func (noEngine) Add(*metainfo.MetaInfo, string, bool) (session.Stats, error) {
	return session.Stats{}, errors.New("no engine")
}
func (noEngine) Get(string) (session.Stats, bool) { return session.Stats{}, false }
func (noEngine) List() []session.Stats            { return nil }
