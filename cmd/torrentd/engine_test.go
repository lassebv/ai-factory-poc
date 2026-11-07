package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"torrentd/internal/config"
	"torrentd/internal/store"
)

type fakeSettings map[string]string

func (f fakeSettings) Setting(_ context.Context, key string) (string, error) {
	if v, ok := f[key]; ok {
		return v, nil
	}
	if f["fail"] != "" {
		return "", errors.New("db broken")
	}
	return "", store.ErrNotFound
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPeerPort(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		flag int
		set  fakeSettings
		want int
	}{
		{7000, fakeSettings{keyListenPort: "6000"}, 7000},
		{0, nil, 0},
		{config.PeerPortUnset, fakeSettings{keyListenPort: "6000"}, 6000},
		{config.PeerPortUnset, fakeSettings{keyListenPort: `"6001"`}, 6001},
		{config.PeerPortUnset, fakeSettings{keyListenPort: "99999"}, defaultListenPort},
		{config.PeerPortUnset, fakeSettings{}, defaultListenPort},
		{config.PeerPortUnset, fakeSettings{"fail": "x"}, defaultListenPort},
	} {
		if got := peerPort(ctx, config.Config{PeerPort: tc.flag}, tc.set, quietLog); got != tc.want {
			t.Errorf("peerPort(%d, %v) = %d, want %d", tc.flag, tc.set, got, tc.want)
		}
	}
}

func TestDefaultDownloadDir(t *testing.T) {
	ctx := context.Background()
	if got := defaultDownloadDir(ctx, fakeSettings{keyDefaultDownloadDir: `"/srv/dl"`}, quietLog); got != "/srv/dl" {
		t.Errorf("setting: %q", got)
	}
	for _, s := range []fakeSettings{{keyDefaultDownloadDir: `"relative"`}, {}, {"fail": "x"}} {
		if got := defaultDownloadDir(ctx, s, quietLog); filepath.Base(got) != "Downloads" {
			t.Errorf("%v: %q, want ~/Downloads", s, got)
		}
	}
}

func TestStartEngine(t *testing.T) {
	sess, err := startEngine(context.Background(), config.Config{PeerPort: 0}, fakeSettings{}, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	if sess.PeerPort() == 0 {
		t.Error("no peer port bound")
	}
}
