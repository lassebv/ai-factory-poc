// Package config parses torrentd's command-line flags and environment
// variables (§10). Flags take precedence over environment variables, which
// take precedence over the built-in defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultAddr is the HTTP listen address used when neither --addr nor
// TORRENTD_ADDR is set.
const DefaultAddr = "127.0.0.1:8080"

// PeerPortUnset means that no peer port was given, so the listen_port
// setting from the database applies.
const PeerPortUnset = -1

// Config is the parsed process configuration.
type Config struct {
	// Addr is the HTTP listen address (host:port).
	Addr string
	// DataDir is the directory holding torrentd.db.
	DataDir string
	// PeerPort is the TCP+UDP port for peers and DHT. PeerPortUnset means
	// "use the listen_port setting"; 0 means "pick any free port".
	PeerPort int
	// Open opens the web UI in a browser after startup.
	Open bool
	// LogLevel is the minimum level that is logged.
	LogLevel slog.Level
	// Dev disables the embedded SPA and enables CORS for the Vite dev server.
	Dev bool
}

// Env looks up an environment variable; it returns "" when it is unset.
type Env func(key string) string

// ErrHelp is returned by Parse when -h or --help was requested.
var ErrHelp = flag.ErrHelp

// Parse parses args (without the program name) using env for the
// environment-variable fallbacks and goos for the OS-specific default data
// directory. Usage and error messages are written to out.
func Parse(args []string, env Env, goos string, out io.Writer) (Config, error) {
	cfg := Config{Addr: DefaultAddr, PeerPort: PeerPortUnset, LogLevel: slog.LevelInfo}

	if v := env("TORRENTD_ADDR"); v != "" {
		cfg.Addr = v
	}
	cfg.DataDir = env("TORRENTD_DATA_DIR")
	peerPort := env("TORRENTD_PEER_PORT")
	logLevel := "info"
	if v := env("TORRENTD_LOG_LEVEL"); v != "" {
		logLevel = v
	}

	fs := flag.NewFlagSet("torrentd", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address (env TORRENTD_ADDR)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory holding torrentd.db (env TORRENTD_DATA_DIR; default: OS data dir)")
	fs.StringVar(&peerPort, "peer-port", peerPort, "TCP+UDP port for peers and DHT; 0 picks a free port (env TORRENTD_PEER_PORT; default: from settings)")
	fs.BoolVar(&cfg.Open, "open", false, "open the web UI in a browser on startup")
	fs.StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn or error (env TORRENTD_LOG_LEVEL)")
	fs.BoolVar(&cfg.Dev, "dev", false, "don't serve the embedded SPA; allow CORS from the Vite dev server")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	if cfg.Addr == "" {
		return Config{}, errors.New("--addr must not be empty")
	}
	if peerPort != "" {
		p, err := strconv.Atoi(peerPort)
		if err != nil || p < 0 || p > 65535 {
			return Config{}, fmt.Errorf("invalid peer port %q: want 0-65535", peerPort)
		}
		cfg.PeerPort = p
	}
	lvl, err := ParseLevel(logLevel)
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = lvl

	if cfg.DataDir == "" {
		dir, err := DefaultDataDir(goos, env)
		if err != nil {
			return Config{}, err
		}
		cfg.DataDir = dir
	}
	return cfg, nil
}

// ParseLevel parses a §10 log level name (case-insensitive).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q: want debug, info, warn or error", s)
}

// DefaultDataDir returns the OS default data directory (§7 and the pinned
// CLI contract): $XDG_DATA_HOME/torrentd (or $HOME/.local/share/torrentd)
// on Linux and other Unix systems, $HOME/Library/Application Support/torrentd
// on macOS and %APPDATA%\torrentd on Windows.
func DefaultDataDir(goos string, env Env) (string, error) {
	switch goos {
	case "windows":
		if a := env("APPDATA"); a != "" {
			return a + `\torrentd`, nil
		}
		return "", errors.New("cannot determine the default data dir: APPDATA is not set (use --data-dir)")
	case "darwin", "ios":
		home := env("HOME")
		if home == "" {
			return "", errors.New("cannot determine the default data dir: HOME is not set (use --data-dir)")
		}
		return filepath.Join(home, "Library", "Application Support", "torrentd"), nil
	default:
		if x := env("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "torrentd"), nil
		}
		home := env("HOME")
		if home == "" {
			return "", errors.New("cannot determine the default data dir: HOME is not set (use --data-dir)")
		}
		return filepath.Join(home, ".local", "share", "torrentd"), nil
	}
}
