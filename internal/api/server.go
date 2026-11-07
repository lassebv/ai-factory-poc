// Package api is torrentd's HTTP layer (§6 HTTP, §8, §16.7): the REST
// handlers, the Host/Origin middleware, --dev CORS, the error shape and the
// embedded SPA with its client-side-routing fallback.
package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// Options configures a Server.
type Options struct {
	// AllowedHosts is the Host-header allowlist ("host:port", see
	// AllowedHosts). Same-origin Origins are derived from it.
	AllowedHosts []string
	// Dev disables the embedded SPA and enables CORS for DevOrigins.
	Dev bool
	// SPA is the built web UI with index.html at its root. It is ignored
	// with Dev. A nil SPA serves 404 for non-API paths.
	SPA fs.FS
	// Logger receives request errors and panics. Nil means slog.Default().
	Logger *slog.Logger
	// Torrents backs the /api/v1/torrents routes (see contract.go).
	Torrents TorrentService
}

// Server is the torrentd HTTP server.
type Server struct {
	opts       Options
	log        *slog.Logger
	hosts      map[string]bool
	origins    map[string]bool
	devOrigins map[string]bool
	index      []byte
	handler    http.Handler
	srv        *http.Server
}

// New builds a Server from opts.
func New(opts Options) *Server {
	s := &Server{
		opts:       opts,
		log:        opts.Logger,
		hosts:      map[string]bool{},
		origins:    map[string]bool{},
		devOrigins: map[string]bool{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, h := range opts.AllowedHosts {
		h = strings.ToLower(h)
		s.hosts[h] = true
		s.origins["http://"+h] = true
	}
	if opts.Dev {
		for _, o := range DevOrigins {
			s.devOrigins[o] = true
			s.origins[o] = true
		}
	}
	if !opts.Dev && opts.SPA != nil {
		if b, err := fs.ReadFile(opts.SPA, "index.html"); err == nil {
			s.index = b
		} else {
			s.log.Warn("embedded web UI has no index.html", "err", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /api/v1/torrents/preview", s.previewTorrent)
	mux.HandleFunc("/api/v1/torrents/preview", methodNotAllowed("POST"))
	mux.HandleFunc("/api/", notFound)
	mux.HandleFunc("/", s.static)

	var h http.Handler = mux
	h = s.originCheck(h)
	if opts.Dev {
		h = s.cors(h)
	}
	h = s.hostCheck(h)
	h = s.recoverer(h)
	s.handler = h
	return s
}

// Handler returns the root handler: Host check, optional CORS, Origin check,
// then routing.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve serves HTTP on l until Shutdown is called. It returns nil after a
// graceful shutdown.
func (s *Server) Serve(l net.Listener) error {
	s.srv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	if err := s.srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server, waiting for active requests until
// ctx is done.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok"))
}

func notFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		WriteError(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "method "+r.Method+" is not allowed here")
	}
}

// static serves the embedded SPA. Existing files are served as they are;
// every other path gets index.html so client-side routes work.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		notFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed("GET, HEAD")(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(s.opts.SPA, name); err == nil && !st.IsDir() {
			http.ServeFileFS(w, r, s.opts.SPA, name)
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(s.index)
}

// recoverer turns a handler panic into a 500 internal error.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in HTTP handler", "method", r.Method, "path", r.URL.Path, "panic", v)
				WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
