package api

// FROZEN CONTRACT TESTS (ADR-0011) for the M3 torrent routes. Owned by the
// architect. They use a fake TorrentService, so they need no engine.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd/internal/bt/bencode"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

type fakeTorrents struct {
	mu      sync.Mutex
	order   [][20]byte
	byID    map[[20]byte]session.Status
	lastOpt session.AddOptions
}

func (f *fakeTorrents) Add(_ context.Context, m *metainfo.MetaInfo, opts session.AddOptions) (session.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastOpt = opts
	if _, ok := f.byID[m.InfoHash]; ok {
		return session.Status{}, fmt.Errorf("add: %w", session.ErrAlreadyExists)
	}
	if !strings.HasPrefix(opts.DownloadDir, "/") {
		return session.Status{}, fmt.Errorf("add: %w", session.ErrInvalidDownloadDir)
	}
	st := session.Status{
		ID: m.InfoHash, Name: m.Info.Name, State: session.StateChecking, SizeBytes: m.Info.VisibleLength(),
		DownloadDir: opts.DownloadDir, AddedAt: time.Date(2026, 11, 7, 10, 0, 0, 0, time.FixedZone("x", 3600)),
	}
	if opts.StartPaused {
		st.State = session.StatePaused
	}
	f.byID[m.InfoHash] = st
	f.order = append(f.order, m.InfoHash)
	return st, nil
}

func (f *fakeTorrents) List() []session.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []session.Status
	for _, id := range f.order {
		out = append(out, f.byID[id])
	}
	return out
}

func (f *fakeTorrents) Get(id [20]byte) (session.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.byID[id]
	return st, ok
}

func contractServer() (*Server, *fakeTorrents) {
	f := &fakeTorrents{byID: map[[20]byte]session.Status{}}
	s := New(Options{
		AllowedHosts: []string{"127.0.0.1:8080"},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Torrents:     f,
	})
	return s, f
}

func contractTorrent(t *testing.T, name string) []byte {
	t.Helper()
	data := bytes.Repeat([]byte(name), 20000/len(name)+1)
	h := sha1.Sum(data[:16384])
	h2 := sha1.Sum(data[16384:])
	info, _ := bencode.Marshal(map[string]any{"name": name, "length": len(data), "piece length": 16384,
		"pieces": append(h[:], h2[:]...)})
	b, err := bencode.Marshal(map[string]any{"announce": "http://127.0.0.1:1/announce", "info": bencode.RawMessage(info)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func infoHashHex(t *testing.T, torrent []byte) string {
	m, err := metainfo.Parse(torrent)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(m.InfoHash[:])
}

type cresp struct {
	status int
	header http.Header
	body   []byte
}

func call(h http.Handler, method, target string, body []byte, ctype string) cresp {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.Host = "127.0.0.1:8080"
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return cresp{w.Code, w.Header(), w.Body.Bytes()}
}

func addForm(file []byte, fields map[string]string) ([]byte, string) {
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
	return buf.Bytes(), w.FormDataContentType()
}

func code(t *testing.T, r cresp) string {
	t.Helper()
	var b ErrorBody
	if err := json.Unmarshal(r.body, &b); err != nil || b.Error.Message == "" {
		t.Fatalf("not an Error body: %s", r.body)
	}
	return b.Error.Code
}

// checkTorrentJSON checks the Torrent schema's required fields and types.
func checkTorrentJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %s", raw)
	}
	num := []string{"progress", "downloadRate", "uploadRate", "uploadedTotal", "downloadedTotal", "wastedTotal",
		"ratio", "peersConnected", "seedsConnected", "seedingSeconds", "sizeBytes", "downloadLimit", "uploadLimit", "completedBytes"}
	for _, k := range num {
		if _, ok := m[k].(float64); !ok {
			t.Errorf("%s = %v, want a number", k, m[k])
		}
	}
	for _, k := range []string{"id", "state", "name", "downloadDir", "addedAt"} {
		if _, ok := m[k].(string); !ok {
			t.Errorf("%s = %v, want a string", k, m[k])
		}
	}
	for _, k := range []string{"private", "hasMetadata"} {
		if _, ok := m[k].(bool); !ok {
			t.Errorf("%s = %v, want a bool", k, m[k])
		}
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(m["addedAt"])); err != nil {
		t.Errorf("addedAt %v is not RFC 3339", m["addedAt"])
	}
	return m
}

func TestContractAddAndGet(t *testing.T) {
	s, f := contractServer()
	h := s.Handler()
	tor := contractTorrent(t, "alpha")
	id := infoHashHex(t, tor)

	if r := call(h, "GET", "/api/v1/torrents", nil, ""); r.status != 200 || strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("empty list = %d %s, want 200 []", r.status, r.body)
	}
	body, ct := addForm(tor, map[string]string{"downloadDir": "/data/dl", "startPaused": "true"})
	r := call(h, "POST", "/api/v1/torrents", body, ct)
	if r.status != 201 {
		t.Fatalf("add = %d %s, want 201", r.status, r.body)
	}
	m := checkTorrentJSON(t, r.body)
	if m["id"] != id || m["name"] != "alpha" || m["state"] != "paused" || m["downloadDir"] != "/data/dl" || m["hasMetadata"] != true {
		t.Errorf("add body = %s", r.body)
	}
	if m["addedAt"] != "2026-11-07T09:00:00Z" {
		t.Errorf("addedAt = %v, want UTC 2026-11-07T09:00:00Z", m["addedAt"])
	}
	if m["completedAt"] != nil || m["errorMessage"] != nil {
		t.Errorf("completedAt/errorMessage must be null: %s", r.body)
	}
	if f.lastOpt != (session.AddOptions{DownloadDir: "/data/dl", StartPaused: true}) {
		t.Errorf("Add got %+v", f.lastOpt)
	}

	r = call(h, "GET", "/api/v1/torrents/"+id, nil, "")
	if r.status != 200 {
		t.Fatalf("get = %d %s", r.status, r.body)
	}
	checkTorrentJSON(t, r.body)
	r = call(h, "GET", "/api/v1/torrents", nil, "")
	var list []map[string]any
	if r.status != 200 || json.Unmarshal(r.body, &list) != nil || len(list) != 1 || list[0]["id"] != id {
		t.Fatalf("list = %d %s", r.status, r.body)
	}

	dup := call(h, "POST", "/api/v1/torrents", body, ct)
	var ae AlreadyExistsBody
	if dup.status != 409 || json.Unmarshal(dup.body, &ae) != nil || ae.ID != id || ae.Error.Code != "already_exists" || ae.Error.Message == "" {
		t.Fatalf("duplicate = %d %s, want 409 already_exists with id", dup.status, dup.body)
	}
	other := contractTorrent(t, "beta")
	body, ct = addForm(other, map[string]string{"downloadDir": "rel/dir"})
	if r := call(h, "POST", "/api/v1/torrents", body, ct); r.status != 400 || code(t, r) != CodeInvalidDownloadDir {
		t.Fatalf("relative dir = %d %s, want 400 invalid_download_dir", r.status, r.body)
	}
}

func TestContractAddRejects(t *testing.T) {
	s, _ := contractServer()
	h := s.Handler()
	for _, tc := range []struct {
		name   string
		file   []byte
		fields map[string]string
		status int
		code   string
	}{
		{"garbage file", []byte("not bencode"), map[string]string{"downloadDir": "/d"}, 400, CodeInvalidTorrent},
		{"no file", nil, map[string]string{"downloadDir": "/d"}, 400, CodeInvalidRequest},
		{"magnet only", nil, map[string]string{"magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, 400, CodeInvalidRequest},
		{"bad startPaused", contractTorrent(t, "gamma"), map[string]string{"downloadDir": "/d", "startPaused": "maybe"}, 400, CodeInvalidRequest},
	} {
		body, ct := addForm(tc.file, tc.fields)
		r := call(h, "POST", "/api/v1/torrents", body, ct)
		if r.status != tc.status || code(t, r) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.code)
		}
	}
}

func TestContractTorrentRoutes(t *testing.T) {
	s, _ := contractServer() // New must not panic with the M3 route table
	h := s.Handler()
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/api/v1/torrents/" + strings.Repeat("ab", 20), 404, ""},
		{"GET", "/api/v1/torrents/" + strings.Repeat("AB", 20), 404, ""},
		{"GET", "/api/v1/torrents/xyz", 404, ""},
		{"GET", "/api/v1/torrents/preview", 405, "POST"},
		{"DELETE", "/api/v1/torrents/preview", 405, "POST"},
		{"PUT", "/api/v1/torrents", 405, "GET, POST"},
		{"DELETE", "/api/v1/torrents/" + strings.Repeat("ab", 20), 405, "GET"},
		{"GET", "/api/v1/nope", 404, ""},
	} {
		r := call(h, tc.method, tc.path, nil, "")
		if r.status != tc.status {
			t.Errorf("%s %s = %d %s, want %d", tc.method, tc.path, r.status, r.body, tc.status)
			continue
		}
		if tc.status == 404 && code(t, r) != CodeNotFound {
			t.Errorf("%s %s: code %s, want not_found", tc.method, tc.path, code(t, r))
		}
		if tc.allow != "" {
			if code(t, r) != CodeInvalidRequest || !strings.Contains(r.header.Get("Allow"), tc.allow) {
				t.Errorf("%s %s: Allow %q code %s, want Allow containing %q", tc.method, tc.path, r.header.Get("Allow"), code(t, r), tc.allow)
			}
		}
	}
	// Preview keeps working next to {id}.
	body, ct := addForm(contractTorrent(t, "delta"), nil)
	if r := call(h, "POST", "/api/v1/torrents/preview", body, ct); r.status != 200 {
		t.Errorf("preview = %d %s", r.status, r.body)
	}
}
