package store

// Placeholder bodies so the tree builds before the M4 implementation exists.
// The implementer replaces this file (see contract.go).

import (
	"context"
	"errors"
	"time"
)

var errNotImplemented = errors.New("store: not implemented (M4)")

// LoadTorrents returns every persisted torrent (see contract.go).
func (s *Store) LoadTorrents(ctx context.Context) ([]TorrentRecord, error) {
	return nil, errNotImplemented
}

// SaveTorrents upserts recs in one transaction (see contract.go).
func (s *Store) SaveTorrents(ctx context.Context, recs []TorrentRecord) error {
	return errNotImplemented
}

// LoadBans returns the unexpired bans (see contract.go).
func (s *Store) LoadBans(ctx context.Context, now time.Time) ([]Ban, error) {
	return nil, errNotImplemented
}

// SaveBan upserts a ban (see contract.go).
func (s *Store) SaveBan(ctx context.Context, b Ban) error { return errNotImplemented }
