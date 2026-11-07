package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tinyTorrent = "d8:announce8:http://t4:infod6:lengthi20000e4:name5:a.bin12:piece lengthi20000e6:pieces20:" +
	"aaaaaaaaaaaaaaaaaaaaee"

func postPreview(t *testing.T, parts map[string][]byte, order []string) resp {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, name := range order {
		p, err := w.CreateFormFile(name, name+".torrent")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = p.Write(parts[name])
	}
	_ = w.Close()
	return postRaw(t, buf.Bytes(), w.FormDataContentType())
}

func postRaw(t *testing.T, body []byte, ct string) resp {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/torrents/preview", bytes.NewReader(body))
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	newServer(false).Handler().ServeHTTP(rec, r)
	return resp{rec.Code, rec.Header(), rec.Body.String()}
}

func TestPreviewOK(t *testing.T) {
	r := postPreview(t, map[string][]byte{"other": []byte("x"), "file": []byte(tinyTorrent)}, []string{"other", "file"})
	if r.status != http.StatusOK {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	var p TorrentPreview
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "a.bin" || p.PieceLength != 20000 || p.PieceCount != 1 || p.SizeBytes != 20000 ||
		len(p.Files) != 1 || p.Files[0].Path != "a.bin" || len(p.Trackers) != 1 || p.Trackers[0].URL != "http://t" ||
		len(p.ID) != 40 {
		t.Fatalf("%+v", p)
	}
}

func TestPreviewErrors(t *testing.T) {
	cases := []struct {
		name   string
		r      resp
		status int
		code   string
	}{
		{"junk", postPreview(t, map[string][]byte{"file": []byte("junk")}, []string{"file"}), 400, CodeInvalidTorrent},
		{"no file", postPreview(t, map[string][]byte{"x": []byte("junk")}, []string{"x"}), 400, CodeInvalidRequest},
		{"json", postRaw(t, []byte(`{}`), "application/json"), 400, CodeInvalidRequest},
		{"no boundary", postRaw(t, []byte(`{}`), "multipart/form-data"), 400, CodeInvalidRequest},
		{"broken multipart", postRaw(t, []byte("--b\r\nbroken"), "multipart/form-data; boundary=b"), 400, CodeInvalidRequest},
		{"too large", postPreview(t, map[string][]byte{"file": bytes.Repeat([]byte("x"), MaxTorrentFileSize+1)}, []string{"file"}), 413, CodePayloadTooLarge},
		{"body too large", postPreview(t, map[string][]byte{"pad": bytes.Repeat([]byte("x"), MaxTorrentFileSize+multipartOverhead)}, []string{"pad"}), 413, CodePayloadTooLarge},
	}
	for _, c := range cases {
		if c.r.status != c.status || errCode(t, c.r) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, c.r.status, c.r.body, c.status, c.code)
		}
	}
	r := do(t, newServer(false).Handler(), "GET", "/api/v1/torrents/preview", nil)
	if r.status != http.StatusMethodNotAllowed || !strings.Contains(r.header.Get("Allow"), "POST") {
		t.Errorf("GET: %d %v", r.status, r.header)
	}
}
