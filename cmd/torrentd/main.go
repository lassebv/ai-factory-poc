// Command torrentd is a single-binary BitTorrent client with a web UI.
//
// Milestone 1 wires up the skeleton: configuration (§10), the data directory,
// SQLite with migrations, and the HTTP server with the embedded SPA. It
// shuts down gracefully on SIGINT or SIGTERM (§6.2).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"torrentd"
	"torrentd/internal/api"
	"torrentd/internal/clock"
	"torrentd/internal/config"
	"torrentd/internal/store"
)

// httpShutdownTimeout bounds the graceful HTTP shutdown (§6.2 step 1).
const httpShutdownTimeout = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole program; it returns the process exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Parse(args, getenv, runtime.GOOS, stderr)
	if errors.Is(err, config.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "torrentd:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	if err := serve(ctx, cfg, log); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

// serve runs startup (§6.1 steps 1, 2 and 5), blocks until ctx is cancelled,
// then shuts down (§6.2).
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// §6.1 step 1: data directory.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// §6.1 step 2: SQLite and migrations.
	dbPath := store.Path(cfg.DataDir)
	st, err := store.Open(ctx, dbPath, clock.Real())
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("close database", "err", err)
		}
	}()
	previousClean, err := st.BeginSession(ctx)
	if err != nil {
		return err
	}
	log.Info("database ready", "path", dbPath, "previous_shutdown_clean", previousClean)

	// §6.1 steps 3 and 4 (session, torrents) arrive with the engine.
	if cfg.PeerPort != config.PeerPortUnset {
		log.Debug("peer port configured; the engine is not implemented yet", "peer_port", cfg.PeerPort)
	}

	// §6.1 step 5: HTTP server.
	l, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	if !isLoopback(cfg.Addr) {
		log.Warn("HTTP server is listening on a non-loopback address; the web UI has no authentication",
			"addr", cfg.Addr, "listen", l.Addr().String())
	}
	srv := api.New(api.Options{
		AllowedHosts: api.AllowedHosts(cfg.Addr, l.Addr()),
		Dev:          cfg.Dev,
		SPA:          torrentd.WebFS(),
		Logger:       log,
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()
	url := "http://" + browserHost(cfg.Addr, l.Addr())
	log.Info("torrentd started", "url", url, "data_dir", cfg.DataDir, "dev", cfg.Dev)
	if cfg.Open {
		if err := openBrowser(url); err != nil {
			log.Warn("could not open the browser", "url", url, "err", err)
		}
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		return fmt.Errorf("HTTP server: %w", err)
	}

	// §6.2 step 1: graceful HTTP shutdown.
	shCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Warn("HTTP shutdown did not finish in time", "err", err)
	}
	<-serveErr

	// §6.2 steps 2-4 (announces, flushes, peers) arrive with the engine. The
	// clean-shutdown marker is the last write before closing the DB (C3).
	if err := st.MarkCleanShutdown(context.Background()); err != nil {
		log.Error("write clean-shutdown marker", "err", err)
	}
	log.Info("stopped")
	return nil
}

// isLoopback reports whether addr's host is a loopback address or
// "localhost". An empty host binds every interface and is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// browserHost is the host:port to open in a browser: the configured host,
// or 127.0.0.1 for wildcard binds, with the actual port.
func browserHost(addr string, listen net.Addr) string {
	host, _, _ := net.SplitHostPort(addr)
	_, port, _ := net.SplitHostPort(listen.String())
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// openBrowser opens url in the default browser (--open).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
