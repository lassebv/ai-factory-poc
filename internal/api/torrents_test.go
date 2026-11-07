package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

// fakeEngine is an in-memory Engine.
type fakeEngine struct {
	added  []session.Stats
	err    error
	paused bool
}

func (e *fakeEngine) Add(m *metainfo.MetaInfo, dir string, paused bool) (session.Stats, error) {
	if e.err != nil {
		return session.Stats{}, e.err
	}
	e.paused = paused
	st := session.Stats{
		ID: m.HexHash(), Name: m.Info.Name, State: session.StateChecking, SizeBytes: m.Info.VisibleLength(),
		DownloadDir: dir, AddedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SwarmSeeders: -1, SwarmLeechers: -1,
	}
	e.added = append(e.added, st)
	return st, nil
}

func (e *fakeEngine) Get(id string) (session.Stats, bool) {
	for _, s := range e.added {
		if s.ID == id {
			return s, true
		}
	}
	return session.Stats{}, false
}

func (e *fakeEngine) List() []session.Stats { return e.added }

func engineServer(e Engine, defaultDir string) http.Handler {
	return New(Options{AllowedHosts: []string{"127.0.0.1:8080"}, Logger: quiet, Engine: e, DefaultDownloadDir: defaultDir}).Handler()
}

func postAdd(t *testing.T, h http.Handler, fields map[string]string, file []byte) resp {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if file != nil {
		p, _ := w.CreateFormFile("file", "x.torrent")
		p.Write(file)
	}
	for k, v := range fields {
		w.WriteField(k, v)
	}
	w.Close()
	r := httptest.NewRequest("POST", "/api/v1/torrents", bytes.NewReader(buf.Bytes()))
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return resp{rec.Code, rec.Header(), rec.Body.String()}
}

func TestAddListGet(t *testing.T) {
	e := &fakeEngine{}
	h := engineServer(e, "")
	dir := filepath.Join(t.TempDir(), "new", "dir")
	r := postAdd(t, h, map[string]string{"downloadDir": dir, "startPaused": "true", "ignored": "x"}, []byte(tinyTorrent))
	if r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	var got Torrent
	json.Unmarshal([]byte(r.body), &got)
	if got.DownloadDir != dir || got.State != "checking" || !got.HasMetadata || got.AddedAt != "2026-01-02T03:04:05Z" ||
		got.CheckingProgress == nil || got.SizeBytes == nil || *got.SizeBytes != 20000 || !e.paused {
		t.Fatalf("added %+v", got)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatal("download dir not created")
	}

	dup := postAdd(t, h, map[string]string{"downloadDir": dir}, []byte(tinyTorrent))
	var ae AlreadyExistsBody
	if dup.status != http.StatusConflict || json.Unmarshal([]byte(dup.body), &ae) != nil || ae.ID != got.ID || ae.Error.Code != CodeAlreadyExists {
		t.Fatalf("duplicate: %d %s", dup.status, dup.body)
	}

	list := do(t, h, "GET", "/api/v1/torrents", nil)
	var all []Torrent
	if list.status != 200 || json.Unmarshal([]byte(list.body), &all) != nil || len(all) != 1 {
		t.Fatalf("list: %d %s", list.status, list.body)
	}
	one := do(t, h, "GET", "/api/v1/torrents/"+got.ID, nil)
	if one.status != 200 {
		t.Fatalf("get: %d %s", one.status, one.body)
	}
	if r := do(t, h, "GET", "/api/v1/torrents/"+string(bytes.Repeat([]byte("ab"), 20)), nil); r.status != 404 || errCode(t, r) != CodeNotFound {
		t.Fatalf("get unknown: %d", r.status)
	}
	for _, tc := range []struct{ method, path, allow string }{
		{"DELETE", "/api/v1/torrents", "GET, HEAD, POST"},
		{"PUT", "/api/v1/torrents/" + got.ID, "GET, HEAD"},
		{"PUT", "/api/v1/torrents/preview", "POST"},
		{"GET", "/api/v1/torrents/preview", "POST"},
	} {
		r := do(t, h, tc.method, tc.path, nil)
		if r.status != 405 || r.header.Get("Allow") != tc.allow || errCode(t, r) != CodeInvalidRequest {
			t.Errorf("%s %s = %d Allow %q", tc.method, tc.path, r.status, r.header.Get("Allow"))
		}
	}
}

func TestAddErrors(t *testing.T) {
	e := &fakeEngine{}
	h := engineServer(e, "")
	abs := t.TempDir()
	blocker := filepath.Join(abs, "file")
	os.WriteFile(blocker, nil, 0o644)
	for _, tc := range []struct {
		name   string
		fields map[string]string
		file   []byte
		status int
		code   string
	}{
		{"relative dir", map[string]string{"downloadDir": "rel/dir"}, []byte(tinyTorrent), 400, CodeInvalidDownloadDir},
		{"no dir and no default", nil, []byte(tinyTorrent), 400, CodeInvalidDownloadDir},
		{"dir under a file", map[string]string{"downloadDir": filepath.Join(blocker, "x")}, []byte(tinyTorrent), 400, CodeInvalidDownloadDir},
		{"bad torrent", map[string]string{"downloadDir": abs}, []byte("nope"), 400, CodeInvalidTorrent},
		{"bad startPaused", map[string]string{"downloadDir": abs, "startPaused": "maybe"}, []byte(tinyTorrent), 400, CodeInvalidRequest},
		{"magnet", map[string]string{"magnet": "magnet:?xt=urn:btih:" + string(bytes.Repeat([]byte("a"), 40))}, nil, 400, CodeInvalidRequest},
		{"no file", map[string]string{"downloadDir": abs}, nil, 400, CodeInvalidRequest},
		{"field too large", map[string]string{"downloadDir": string(bytes.Repeat([]byte("a"), maxFieldSize+1))}, nil, 400, CodeInvalidRequest},
	} {
		r := postAdd(t, h, tc.fields, tc.file)
		if r.status != tc.status || errCode(t, r) != tc.code {
			t.Errorf("%s: %d %s", tc.name, r.status, r.body)
		}
	}
	if r := do(t, h, "POST", "/api/v1/torrents", map[string]string{"Content-Type": "application/json"}); r.status != 400 {
		t.Errorf("non-multipart: %d", r.status)
	}

	// The default dir is used when none is given; engine errors map to 409/500.
	hd := engineServer(e, abs)
	if r := postAdd(t, hd, nil, []byte(tinyTorrent)); r.status != 201 {
		t.Fatalf("default dir: %d %s", r.status, r.body)
	}
	e.added = nil
	e.err = session.ErrExists
	if r := postAdd(t, hd, nil, []byte(tinyTorrent)); r.status != 409 {
		t.Errorf("ErrExists: %d", r.status)
	}
	e.err = errors.New("boom")
	if r := postAdd(t, hd, nil, []byte(tinyTorrent)); r.status != 500 || errCode(t, r) != CodeInternal {
		t.Errorf("engine error: %d", r.status)
	}
	// Without an engine nothing can be added and the list is empty.
	none := engineServer(nil, abs)
	if r := postAdd(t, none, nil, []byte(tinyTorrent)); r.status != 500 {
		t.Errorf("no engine add: %d", r.status)
	}
	if r := do(t, none, "GET", "/api/v1/torrents", nil); r.status != 200 || r.body != "[]\n" {
		t.Errorf("no engine list: %d %q", r.status, r.body)
	}
}

func TestNewTorrent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tor := NewTorrent(session.Stats{
		ID: "x", State: session.StateDownloading, SizeBytes: 1000, CompletedBytes: 400, Progress: 0.4,
		DownloadRate: 100, UploadedTotal: 50, DownloadedTotal: 500, SwarmSeeders: 2, SwarmLeechers: 0,
		CompletedAt: now, ErrorMessage: "e", AddedAt: now,
	})
	if tor.EtaSeconds == nil || *tor.EtaSeconds != 6 || tor.Ratio != 0.1 || *tor.SwarmSeeders != 2 ||
		*tor.SwarmLeechers != 0 || *tor.CompletedAt != "2026-01-01T00:00:00Z" || *tor.ErrorMessage != "e" || tor.CheckingProgress != nil {
		t.Fatalf("%+v", tor)
	}
	idle := NewTorrent(session.Stats{State: session.StateSeeding, SwarmSeeders: -1, SwarmLeechers: -1, Progress: 1.5})
	if idle.EtaSeconds != nil || idle.SwarmSeeders != nil || idle.Ratio != 0 || idle.CompletedAt != nil || idle.Progress != 1 {
		t.Fatalf("%+v", idle)
	}
}
