package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// DevOrigins are the Vite dev-server origins allowed with --dev (§10, G17).
var DevOrigins = []string{"http://localhost:5173", "http://127.0.0.1:5173"}

// AllowedHosts returns the Host-header allowlist for a server configured
// with addr (the --addr value) and actually listening on listenAddr:
// localhost, 127.0.0.1, [::1] and the host of addr, each with the listening
// port (§16.7 G17). Entries are lowercase "host:port".
func AllowedHosts(addr string, listenAddr net.Addr) []string {
	port := ""
	if tcp, ok := listenAddr.(*net.TCPAddr); ok {
		port = strconv.Itoa(tcp.Port)
	} else if listenAddr != nil {
		_, port, _ = net.SplitHostPort(listenAddr.String())
	}
	cfgHost, cfgPort, err := net.SplitHostPort(addr)
	if err != nil {
		cfgHost = ""
	}
	if port == "" {
		port = cfgPort
	}
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if cfgHost != "" {
		hosts = append(hosts, cfgHost)
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		hp := strings.ToLower(net.JoinHostPort(h, port))
		if !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	return out
}

// normalizeHost lowercases a Host header value and adds the default HTTP
// port when it has none.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if _, _, err := net.SplitHostPort(h); err != nil {
		if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
			return h + ":80"
		}
		if strings.Contains(h, ":") { // malformed
			return h
		}
		return h + ":80"
	}
	return h
}

// isStateChanging reports whether the method can change server state and is
// therefore subject to the Origin check.
func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// hostCheck rejects requests whose Host header is not on the allowlist.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[normalizeHost(r.Host)] {
			WriteError(w, http.StatusForbidden, CodeForbiddenHost, "Host header "+quote(r.Host)+" is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors answers CORS preflights and adds CORS headers for the dev origins.
// It is only installed with --dev.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		h := w.Header()
		h.Add("Vary", "Origin")
		if origin == "" || !s.devOrigins[strings.ToLower(origin)] {
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				h.Set("Access-Control-Allow-Headers", req)
			} else {
				h.Set("Access-Control-Allow-Headers", "Content-Type")
			}
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originCheck rejects state-changing requests with a foreign Origin header.
// A missing Origin is allowed (§16.7 G17).
func (s *Server) originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStateChanging(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" && !s.origins[strings.ToLower(origin)] {
				WriteError(w, http.StatusForbidden, CodeForbiddenOrigin, "Origin "+quote(origin)+" is not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func quote(s string) string {
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return "\"" + strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s) + "\""
}
