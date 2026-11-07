package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const index = "<!doctype html><html><title>torrentd</title></html>"

var spa = fstest.MapFS{
	"index.html":    {Data: []byte(index)},
	"assets/app.js": {Data: []byte("console.log(1)")},
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(dev bool) *Server {
	return New(Options{
		AllowedHosts: []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"},
		Dev:          dev,
		SPA:          spa,
		Logger:       quiet,
	})
}

type resp struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) resp {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader("{}"))
	r.Host = "127.0.0.1:8080"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return resp{w.Code, w.Header(), w.Body.String()}
}

func errCode(t *testing.T, r resp) string {
	t.Helper()
	var b ErrorBody
	dec := json.NewDecoder(strings.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("status %d: not an Error body: %v: %q", r.status, err, r.body)
	}
	if b.Error.Message == "" {
		t.Errorf("empty error message")
	}
	if !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("error Content-Type %q", r.header.Get("Content-Type"))
	}
	return b.Error.Code
}

func TestHealthz(t *testing.T) {
	h := newServer(false).Handler()
	r := do(t, h, "GET", "/healthz", nil)
	if r.status != 200 || r.body != "ok" {
		t.Fatalf("got %d %q", r.status, r.body)
	}
	r = do(t, h, "HEAD", "/healthz", nil)
	if r.status != 200 {
		t.Fatalf("HEAD got %d", r.status)
	}
	r = do(t, h, "POST", "/healthz", nil)
	if r.status != 405 || errCode(t, r) != CodeInvalidRequest || r.header.Get("Allow") == "" {
		t.Fatalf("POST got %d %v", r.status, r.header)
	}
}

func TestSPA(t *testing.T) {
	h := newServer(false).Handler()
	for _, p := range []string{"/", "/index.html", "/settings", "/torrents/abc", "/a/b/c", "/assets"} {
		r := do(t, h, "GET", p, nil)
		if r.status != 200 || r.body != index || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %q %q", p, r.status, r.header.Get("Content-Type"), r.body)
		}
	}
	if r := do(t, h, "GET", "/assets/app.js", nil); r.status != 200 || r.body != "console.log(1)" {
		t.Errorf("asset: %d %q", r.status, r.body)
	}
	if r := do(t, h, "HEAD", "/settings", nil); r.status != 200 || r.body != "" {
		t.Errorf("HEAD: %d %q", r.status, r.body)
	}
	if r := do(t, h, "DELETE", "/settings", nil); r.status != 405 || errCode(t, r) != CodeInvalidRequest {
		t.Errorf("DELETE: %d", r.status)
	}
}

func TestAPINotFound(t *testing.T) {
	h := newServer(false).Handler()
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range []string{"/api/nope", "/api/v1/torrents/x/unknown"} {
			r := do(t, h, m, p, nil)
			if r.status != 404 || errCode(t, r) != CodeNotFound {
				t.Errorf("%s %s = %d", m, p, r.status)
			}
		}
	}
}

func TestNoSPA(t *testing.T) {
	h := New(Options{AllowedHosts: []string{"127.0.0.1:8080"}, SPA: fstest.MapFS{}, Logger: quiet}).Handler()
	if r := do(t, h, "GET", "/", nil); r.status != 404 || errCode(t, r) != CodeNotFound {
		t.Errorf("SPA without index: %d", r.status)
	}
	h = New(Options{AllowedHosts: []string{"127.0.0.1:8080"}}).Handler()
	if r := do(t, h, "GET", "/", nil); r.status != 404 {
		t.Errorf("nil SPA: %d", r.status)
	}
}

func TestHostCheck(t *testing.T) {
	h := newServer(false).Handler()
	for _, host := range []string{"localhost:8080", "LOCALHOST:8080", "127.0.0.1:8080", "[::1]:8080"} {
		if r := do(t, h, "GET", "/healthz", map[string]string{"Host": host}); r.status != 200 {
			t.Errorf("Host %s: %d", host, r.status)
		}
	}
	for _, host := range []string{"evil.example:8080", "evil.example", "localhost:1", "localhost", "[::1]", "::1", "127.0.0.1.evil.example:8080", ""} {
		for _, p := range []string{"/healthz", "/", "/api/v1/torrents"} {
			r := do(t, h, "GET", p, map[string]string{"Host": host})
			if r.status != 403 || errCode(t, r) != CodeForbiddenHost {
				t.Errorf("Host %q %s: %d", host, p, r.status)
			}
		}
	}
}

func TestHostDefaultPort(t *testing.T) {
	h := New(Options{AllowedHosts: []string{"localhost:80", "[::1]:80"}, SPA: spa, Logger: quiet}).Handler()
	for _, host := range []string{"localhost", "[::1]"} {
		if r := do(t, h, "GET", "/healthz", map[string]string{"Host": host}); r.status != 200 {
			t.Errorf("Host %s on port 80: %d", host, r.status)
		}
	}
}

func TestOriginCheck(t *testing.T) {
	h := newServer(false).Handler()
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, o := range []string{"http://evil.example", "null", "http://localhost:5173", "http://127.0.0.1:1", "https://127.0.0.1:8080"} {
			r := do(t, h, m, "/api/v1/x", map[string]string{"Origin": o})
			if r.status != 403 || errCode(t, r) != CodeForbiddenOrigin {
				t.Errorf("%s Origin %s: %d", m, o, r.status)
			}
		}
		for _, o := range []string{"", "http://127.0.0.1:8080", "http://LOCALHOST:8080", "http://[::1]:8080"} {
			if r := do(t, h, m, "/api/v1/x", map[string]string{"Origin": o}); r.status != 404 {
				t.Errorf("%s Origin %q: %d, want 404", m, o, r.status)
			}
		}
	}
	if r := do(t, h, "GET", "/healthz", map[string]string{"Origin": "http://evil.example"}); r.status != 200 {
		t.Errorf("GET with foreign Origin: %d", r.status)
	}
}

func TestDevMode(t *testing.T) {
	h := newServer(true).Handler()
	pre := do(t, h, "OPTIONS", "/api/v1/torrents", map[string]string{
		"Origin": "http://localhost:5173", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	})
	if pre.status != 204 || pre.header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		pre.header.Get("Access-Control-Allow-Headers") != "content-type" || pre.header.Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("preflight: %d %v", pre.status, pre.header)
	}
	pre = do(t, h, "OPTIONS", "/api/v1/torrents", map[string]string{"Origin": "http://127.0.0.1:5173", "Access-Control-Request-Method": "GET"})
	if pre.status != 204 || pre.header.Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("preflight without headers: %d %v", pre.status, pre.header)
	}
	r := do(t, h, "POST", "/api/v1/x", map[string]string{"Origin": "http://localhost:5173"})
	if r.status != 404 || r.header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("dev POST: %d %v", r.status, r.header)
	}
	r = do(t, h, "POST", "/api/v1/x", map[string]string{"Origin": "http://evil.example"})
	if r.status != 403 || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("dev foreign POST: %d %v", r.status, r.header)
	}
	if r := do(t, h, "GET", "/", nil); r.status != 404 || errCode(t, r) != CodeNotFound {
		t.Errorf("dev GET /: %d", r.status)
	}
	if r := do(t, h, "GET", "/healthz", nil); r.status != 200 {
		t.Errorf("dev healthz: %d", r.status)
	}
	if r := newServer(false).Handler(); do(t, r, "OPTIONS", "/api/v1/torrents", map[string]string{
		"Origin": "http://localhost:5173", "Access-Control-Request-Method": "POST",
	}).header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS headers without --dev")
	}
}

func TestAllowedHosts(t *testing.T) {
	l := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4321}
	got := strings.Join(AllowedHosts("127.0.0.1:0", l), ",")
	if got != "localhost:4321,127.0.0.1:4321,[::1]:4321" {
		t.Errorf("got %s", got)
	}
	got = strings.Join(AllowedHosts("MyHost:8080", &net.TCPAddr{Port: 8080}), ",")
	if got != "localhost:8080,127.0.0.1:8080,[::1]:8080,myhost:8080" {
		t.Errorf("got %s", got)
	}
	got = strings.Join(AllowedHosts("[::]:9", &net.UnixAddr{Name: "x"}), ",")
	if got != "localhost:9,127.0.0.1:9,[::1]:9,[::]:9" {
		t.Errorf("got %s", got)
	}
	got = strings.Join(AllowedHosts(":7", nil), ",")
	if got != "localhost:7,127.0.0.1:7,[::1]:7" {
		t.Errorf("got %s", got)
	}
}

func TestRecoverer(t *testing.T) {
	s := newServer(false)
	h := s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	if r := do(t, h, "GET", "/", nil); r.status != 500 || errCode(t, r) != CodeInternal {
		t.Errorf("panic: %d", r.status)
	}
	h = s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("ErrAbortHandler was swallowed")
		}
	}()
	do(t, h, "GET", "/", nil)
}

func TestWriteErrorDefaultMessage(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, 404, CodeNotFound, "")
	if !strings.Contains(w.Body.String(), `"message":"Not Found"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

func TestQuote(t *testing.T) {
	if q := quote("a\nb"); q != `"a?b"` {
		t.Errorf("got %s", q)
	}
	if q := quote(strings.Repeat("x", 200)); len(q) > 110 {
		t.Errorf("not truncated: %d", len(q))
	}
}

func TestServeAndShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{AllowedHosts: AllowedHosts("127.0.0.1:0", l.Addr()), SPA: spa, Logger: quiet})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Serve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get("http://" + l.Addr().String() + "/healthz"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve after Shutdown: %v", err)
	}
	// Serving on a closed listener is an error.
	if err := New(Options{}).Serve(l); err == nil {
		t.Error("Serve on a closed listener: no error")
	}
}
