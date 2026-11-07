package config

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(kv ...string) Env {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(nil, envOf("HOME", "/home/u"), "linux", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: DefaultAddr, DataDir: filepath.Join("/home/u", ".local", "share", "torrentd"), PeerPort: PeerPortUnset, LogLevel: slog.LevelInfo}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestEnvBeatsDefault(t *testing.T) {
	env := envOf("HOME", "/h", "TORRENTD_ADDR", "127.0.0.1:9", "TORRENTD_DATA_DIR", "/env",
		"TORRENTD_PEER_PORT", "1234", "TORRENTD_LOG_LEVEL", "DEBUG")
	cfg, err := Parse(nil, env, "linux", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9" || cfg.DataDir != "/env" || cfg.PeerPort != 1234 || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("got %+v", cfg)
	}
}

func TestFlagBeatsEnv(t *testing.T) {
	env := envOf("TORRENTD_ADDR", "127.0.0.1:9", "TORRENTD_DATA_DIR", "/env", "TORRENTD_PEER_PORT", "1234", "TORRENTD_LOG_LEVEL", "error")
	args := []string{"--addr", "127.0.0.1:10", "--data-dir", "/flag", "--peer-port", "0", "--log-level", "warn", "--open", "--dev"}
	cfg, err := Parse(args, env, "linux", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: "127.0.0.1:10", DataDir: "/flag", PeerPort: 0, Open: true, LogLevel: slog.LevelWarn, Dev: true}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestErrors(t *testing.T) {
	env := envOf("HOME", "/h")
	for name, args := range map[string][]string{
		"unknown flag":   {"--nope"},
		"positional":     {"extra"},
		"bad peer port":  {"--peer-port", "x"},
		"peer port high": {"--peer-port", "70000"},
		"peer port neg":  {"--peer-port", "-2"},
		"bad level":      {"--log-level", "loud"},
		"empty addr":     {"--addr", ""},
	} {
		if _, err := Parse(args, env, "linux", io.Discard); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Parse(nil, envOf("TORRENTD_PEER_PORT", "abc", "HOME", "/h"), "linux", io.Discard); err == nil {
		t.Error("bad TORRENTD_PEER_PORT accepted")
	}
	if _, err := Parse(nil, envOf(), "linux", io.Discard); err == nil {
		t.Error("missing HOME accepted")
	}
	if _, err := Parse([]string{"-h"}, env, "linux", io.Discard); !errors.Is(err, ErrHelp) {
		t.Errorf("-h: err %v, want ErrHelp", err)
	}
}

func TestDefaultDataDir(t *testing.T) {
	cases := []struct {
		goos string
		env  Env
		want string
	}{
		{"linux", envOf("HOME", "/h", "XDG_DATA_HOME", "/xdg"), filepath.Join("/xdg", "torrentd")},
		{"linux", envOf("HOME", "/h", "XDG_DATA_HOME", ""), filepath.Join("/h", ".local", "share", "torrentd")},
		{"linux", envOf("HOME", "/h", "XDG_DATA_HOME", "relative"), filepath.Join("/h", ".local", "share", "torrentd")},
		{"freebsd", envOf("HOME", "/h"), filepath.Join("/h", ".local", "share", "torrentd")},
		{"darwin", envOf("HOME", "/Users/u"), filepath.Join("/Users/u", "Library", "Application Support", "torrentd")},
		{"windows", envOf("APPDATA", `C:\Users\u\AppData\Roaming`), `C:\Users\u\AppData\Roaming\torrentd`},
	}
	for _, c := range cases {
		got, err := DefaultDataDir(c.goos, c.env)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.goos, got, err, c.want)
		}
	}
	for _, goos := range []string{"windows", "darwin", "linux"} {
		if _, err := DefaultDataDir(goos, envOf()); err == nil {
			t.Errorf("%s with empty env: no error", goos)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "Info": slog.LevelInfo, "warn": slog.LevelWarn, "warning": slog.LevelWarn, "ERROR": slog.LevelError} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := ParseLevel(""); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("empty level: %v", err)
	}
}
