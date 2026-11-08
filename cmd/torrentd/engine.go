package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"torrentd/internal/bt/session"
	"torrentd/internal/clock"
	"torrentd/internal/config"
	"torrentd/internal/store"
)

// Setting keys and defaults used by the engine (§7 default settings).
const (
	keyListenPort         = "listen_port"
	keyDefaultDownloadDir = "default_download_dir"
	defaultListenPort     = 42069
)

// settings is the part of the store the engine reads at startup.
type settings interface {
	Setting(ctx context.Context, key string) (string, error)
}

// peerPort resolves the peer port: --peer-port wins; otherwise the
// listen_port setting, otherwise its default.
func peerPort(ctx context.Context, cfg config.Config, st settings, log *slog.Logger) int {
	if cfg.PeerPort != config.PeerPortUnset {
		return cfg.PeerPort
	}
	raw, err := st.Setting(ctx, keyListenPort)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Warn("read listen_port setting", "err", err)
		}
		return defaultListenPort
	}
	var p int
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p < 0 || p > 65535 {
		// Accept a string-encoded number as well.
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 65535 {
				return n
			}
		}
		log.Warn("invalid listen_port setting; using the default", "value", raw, "default", defaultListenPort)
		return defaultListenPort
	}
	return p
}

// defaultDownloadDir resolves the default download dir: the
// default_download_dir setting, otherwise ~/Downloads.
func defaultDownloadDir(ctx context.Context, st settings, log *slog.Logger) string {
	if raw, err := st.Setting(ctx, keyDefaultDownloadDir); err == nil {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil && filepath.IsAbs(s) {
			return s
		}
		log.Warn("invalid default_download_dir setting; using ~/Downloads", "value", raw)
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Warn("read default_download_dir setting", "err", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads")
}

// startEngine creates the session and restores the saved torrents from db
// (§6.1 steps 3 and 4); db may be nil (no persistence).
func startEngine(ctx context.Context, cfg config.Config, st settings, db session.Store, previousClean bool, log *slog.Logger) (session.Session, error) {
	sess, err := session.New(session.Config{
		PeerPort:              peerPort(ctx, cfg, st, log),
		DefaultDownloadDir:    defaultDownloadDir(ctx, st, log),
		Clock:                 clock.Real(),
		Logger:                log,
		Store:                 db,
		PreviousShutdownClean: previousClean,
	})
	if err != nil {
		return nil, err
	}
	if err := sess.Load(ctx); err != nil {
		_ = sess.Close(context.Background())
		return nil, err
	}
	if err := sess.ListenErr(); err != nil {
		log.Warn("peer port unavailable; incoming connections are disabled", "err", err)
	} else {
		log.Info("peer port bound", "port", sess.PeerPort())
	}
	return sess, nil
}
