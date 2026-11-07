// Package preview holds black-box acceptance tests for milestone 2:
// metainfo parsing and validation, exercised through
// POST /api/v1/torrents/preview against fixtures whose expected values come
// from independent oracles (harness/fixtures/torrents/expected.json).
package preview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/oas"
)

func TestMain(m *testing.M) { os.Exit(app.Main(m)) }

type expFile struct {
	Idx             int     `json:"idx"`
	Path            *string `json:"path"`
	SizeBytes       int64   `json:"sizeBytes"`
	NotReservedFrom string  `json:"notReservedFrom"`
}

type expTracker struct {
	URL  string `json:"url"`
	Tier int    `json:"tier"`
}

type expected struct {
	Accept      bool         `json:"accept"`
	Rule        string       `json:"rule"`
	Why         string       `json:"why"`
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Private     bool         `json:"private"`
	PieceLength int64        `json:"pieceLength"`
	PieceCount  int64        `json:"pieceCount"`
	SizeBytes   int64        `json:"sizeBytes"`
	Files       []expFile    `json:"files"`
	Trackers    []expTracker `json:"trackers"`
}

type preview struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Private     bool   `json:"private"`
	SizeBytes   int64  `json:"sizeBytes"`
	PieceLength int64  `json:"pieceLength"`
	PieceCount  int64  `json:"pieceCount"`
	Files       []struct {
		Idx       int    `json:"idx"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
	} `json:"files"`
	Trackers []expTracker `json:"trackers"`
}

var (
	fixturesDir = filepath.Join(app.RepoRoot(), "harness", "fixtures", "torrents")
	spec        = mustSpec()
	fixtures    = mustFixtures()
)

func mustSpec() *oas.Spec {
	s, err := oas.LoadRepo(app.RepoRoot())
	if err != nil {
		panic(err)
	}
	return s
}

func mustFixtures() map[string]expected {
	data, err := os.ReadFile(filepath.Join(fixturesDir, "expected.json"))
	if err != nil {
		panic(err)
	}
	m := map[string]expected{}
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m
}

// select returns fixture names matching pred, sorted.
func selectFixtures(pred func(name string, e expected) bool) []string {
	var names []string
	for n, e := range fixtures {
		if pred(n, e) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func byRule(rule string) []string {
	return selectFixtures(func(_ string, e expected) bool { return !e.Accept && e.Rule == rule })
}

func multipartBody(t *testing.T, field, filename string, data []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

func postFile(t *testing.T, data []byte) app.Response {
	t.Helper()
	body, ct := multipartBody(t, "file", "upload.torrent", data)
	return app.Shared(t).Do(t, "POST", "/api/v1/torrents/preview", body, map[string]string{"Content-Type": ct})
}

func postFixture(t *testing.T, name string) app.Response {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return postFile(t, data)
}

func errorCode(t *testing.T, r app.Response) string {
	t.Helper()
	if err := spec.ValidateJSON("Error", r.Body); err != nil {
		t.Fatalf("status %d: body is not a valid error object: %v", r.Status, err)
	}
	var e struct{ Error struct{ Code string } }
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func accepted(t *testing.T, name string) preview {
	t.Helper()
	r := postFixture(t, name)
	if r.Status != 200 {
		t.Fatalf("%s: status %d, want 200; body: %.300s", name, r.Status, r.Body)
	}
	if err := spec.ValidateJSON("TorrentPreview", r.Body); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var p preview
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertRejected(t *testing.T, names []string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("no fixtures selected; expected.json is out of date")
	}
	for _, n := range names {
		t.Run(path.Base(n), func(t *testing.T) {
			r := postFixture(t, n)
			if r.Status != 400 {
				t.Fatalf("status %d, want 400 invalid_torrent (%s); body: %.300s", r.Status, fixtures[n].Why, r.Body)
			}
			if code := errorCode(t, r); code != "invalid_torrent" {
				t.Fatalf("error code %q, want invalid_torrent (%s)", code, fixtures[n].Why)
			}
		})
	}
}

func trackerSets(ts []expTracker) map[int][]string {
	m := map[int][]string{}
	for _, t := range ts {
		m[t.Tier] = append(m[t.Tier], t.URL)
	}
	for k := range m {
		sort.Strings(m[k])
	}
	return m
}

// compare checks a preview against the oracle. Within a tier, tracker order is
// not compared (the oracle shuffles tiers, BEP 12).
func compare(t *testing.T, name string, p preview, e expected) {
	t.Helper()
	check := func(field string, got, want any) {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %s = %v, want %v", name, field, got, want)
		}
	}
	check("id", p.ID, e.ID)
	check("name", p.Name, e.Name)
	check("private", p.Private, e.Private)
	check("pieceLength", p.PieceLength, e.PieceLength)
	check("pieceCount", p.PieceCount, e.PieceCount)
	check("sizeBytes", p.SizeBytes, e.SizeBytes)
	check("trackers", trackerSets(p.Trackers), trackerSets(e.Trackers))
	if len(p.Files) != len(e.Files) {
		t.Errorf("%s: %d files, want %d", name, len(p.Files), len(e.Files))
		return
	}
	for i, f := range p.Files {
		ef := e.Files[i]
		check(fmt.Sprintf("files[%d].idx", i), f.Idx, ef.Idx)
		check(fmt.Sprintf("files[%d].sizeBytes", i), f.SizeBytes, ef.SizeBytes)
		if ef.Path != nil {
			check(fmt.Sprintf("files[%d].path", i), f.Path, *ef.Path)
		}
	}
}

func TestPreviewMatchesOracle(t *testing.T) {
	names := selectFixtures(func(_ string, e expected) bool { return e.Accept })
	if len(names) < 10 {
		t.Fatalf("only %d accepted fixtures; expected.json is out of date", len(names))
	}
	for _, n := range names {
		t.Run(path.Base(n), func(t *testing.T) { compare(t, n, accepted(t, n), fixtures[n]) })
	}
}

func TestRejectsUnsafePaths(t *testing.T)    { assertRejected(t, byRule("META-02")) }
func TestRejectsDuplicatePaths(t *testing.T) { assertRejected(t, byRule("META-03")) }
func TestRejectsV2Only(t *testing.T)         { assertRejected(t, byRule("META-08")) }
func TestRejectsMalformed(t *testing.T)      { assertRejected(t, byRule("META-STRUCT")) }
func TestStrictInfoDict(t *testing.T)        { assertRejected(t, byRule("BENC-02")) }

var reservedRE = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)

func TestSanitizesWindowsNames(t *testing.T) {
	const name = "synth/windows_names.torrent"
	e := fixtures[name]
	p := accepted(t, name)
	compare(t, name, p, e)
	for i, ef := range e.Files {
		if ef.NotReservedFrom == "" || i >= len(p.Files) {
			continue
		}
		got := p.Files[i].Path
		base := path.Base(got)
		if got == ef.NotReservedFrom || reservedRE.MatchString(base) {
			t.Errorf("files[%d].path = %q: %q is still a Windows reserved name", i, got, base)
		}
		if path.Dir(got) != path.Dir(ef.NotReservedFrom) {
			t.Errorf("files[%d].path = %q: sanitizing must not change the directory", i, got)
		}
	}
	for _, f := range p.Files {
		if strings.ContainsAny(f.Path, `<>:"|?*`) || strings.HasSuffix(f.Path, ".") || strings.HasSuffix(f.Path, " ") {
			t.Errorf("path %q still contains a Windows-invalid character or trailing dot/space", f.Path)
		}
	}
}

func TestPrefersUTF8Fields(t *testing.T) {
	const name = "synth/utf8_names.torrent"
	p := accepted(t, name)
	compare(t, name, p, fixtures[name])
}

func TestValidatesPieces(t *testing.T) {
	assertRejected(t, byRule("META-06"))
	for _, n := range []string{"synth/non_power_of_two_piece.torrent", "synth/min_piece_length.torrent"} {
		t.Run(path.Base(n), func(t *testing.T) { compare(t, n, accepted(t, n), fixtures[n]) })
	}
}

func TestHybridV1View(t *testing.T) {
	for _, n := range []string{"libtorrent/v2_hybrid.torrent", "libtorrent/v2.torrent", "libtorrent/sample.torrent"} {
		t.Run(path.Base(n), func(t *testing.T) {
			p := accepted(t, n)
			compare(t, n, p, fixtures[n])
			for _, f := range p.Files {
				if strings.Contains(f.Path, "padding") || strings.Contains(f.Path, "/.pad/") {
					t.Errorf("pad file %q is visible", f.Path)
				}
			}
		})
	}
}

func TestInfoHashFromRawBytes(t *testing.T) {
	const name = "synth/unknown_info_keys.torrent"
	p := accepted(t, name)
	if p.ID != fixtures[name].ID {
		t.Fatalf("id = %s, want %s: the info-hash must be the SHA-1 of the original info bytes, including unknown keys", p.ID, fixtures[name].ID)
	}
}

func TestPreviewShape(t *testing.T) {
	p := accepted(t, "real/sintel.torrent") // validates against TorrentPreview
	for i, f := range p.Files {
		if f.Idx != i {
			t.Errorf("files[%d].idx = %d; idx must number files from 0 in order", i, f.Idx)
		}
	}
	inst := app.Shared(t)
	body, ct := multipartBody(t, "notfile", "x.torrent", []byte("d4:infodee"))
	if r := inst.Do(t, "POST", "/api/v1/torrents/preview", body, map[string]string{"Content-Type": ct}); r.Status != 400 || errorCode(t, r) != "invalid_request" {
		t.Errorf("multipart without a file part = %d %s, want 400 invalid_request", r.Status, r.Body)
	}
	if r := inst.Do(t, "POST", "/api/v1/torrents/preview", []byte(`{"file":"x"}`), map[string]string{"Content-Type": "application/json"}); r.Status != 400 || errorCode(t, r) != "invalid_request" {
		t.Errorf("JSON body = %d %s, want 400 invalid_request", r.Status, r.Body)
	}
	if r := inst.Do(t, "GET", "/api/v1/torrents/preview", nil, nil); r.Status != 405 && r.Status != 404 {
		t.Errorf("GET /api/v1/torrents/preview = %d, want 405 (or 404)", r.Status)
	}
}

func TestUploadLimit(t *testing.T) {
	const limit = 10 << 20
	over := bytes.Repeat([]byte("x"), limit+1)
	if r := postFile(t, over); r.Status != 413 || errorCode(t, r) != "payload_too_large" {
		t.Errorf("10 MiB + 1 byte upload = %d %.200s, want 413 payload_too_large", r.Status, r.Body)
	}
	under := bytes.Repeat([]byte("x"), limit-4096)
	if r := postFile(t, under); r.Status != 400 || errorCode(t, r) != "invalid_torrent" {
		t.Errorf("junk just under 10 MiB = %d %.200s, want 400 invalid_torrent", r.Status, r.Body)
	}
}
