package session

import (
	"context"
	"encoding/json"
	"strings"

	"torrentd/internal/bt/storage"
	"torrentd/internal/store"
)

// persistSnap is what the flusher needs from a torrent: the record as the
// actor last captured it and the storage to sync before writing it.
type persistSnap struct {
	rec store.TorrentRecord
	st  storage.Storage
	// statOK is false while the files must not be fingerprinted (the saved
	// fingerprint is kept, e.g. before or during checking).
	statOK bool
}

// requestFlush asks the flusher for a flush soon (state changes).
func (s *session) requestFlush() {
	select {
	case s.flushReq <- struct{}{}:
	default:
	}
}

// flushLoop flushes every FlushInterval and on request until Close.
func (s *session) flushLoop() {
	defer close(s.flushDone)
	tk := s.clk.NewTicker(FlushInterval)
	defer tk.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tk.C():
		case <-s.flushReq:
		}
		s.flush(s.ctx)
	}
}

// flush captures, syncs, fingerprints and saves every torrent (ADR-0019).
func (s *session) flush(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	ts := append([]*torrent(nil), s.order...)
	s.mu.Unlock()
	recs := make([]store.TorrentRecord, 0, len(ts))
	for _, t := range ts {
		snap := t.persist.Load()
		if snap == nil {
			continue
		}
		rec := snap.rec
		if snap.st != nil && snap.statOK {
			if err := snap.st.Sync(); err != nil {
				s.log.Debug("sync before flush failed; keeping the old record", "torrent", t.m.HexHash(), "err", err)
				continue
			}
			if fp, err := storage.Stat(t.dir, t.m.Info); err == nil {
				if b, err := json.Marshal(fp); err == nil {
					rec.ResumeFingerprint = b
				}
			}
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return
	}
	if err := s.store.SaveTorrents(ctx, recs); err != nil {
		s.log.Error("persisting torrents failed; retrying at the next flush", "err", err)
	}
}

// fileRecords lists the non-pad files of t.
func (t *torrent) fileRecords() []store.FileRecord {
	var out []store.FileRecord
	for _, f := range t.m.Info.Files {
		if !f.Pad {
			out = append(out, store.FileRecord{Path: strings.Join(f.Path, "/"), SizeBytes: f.Length})
		}
	}
	return out
}

func (t *torrent) trackerRecords() []store.TrackerRecord {
	var out []store.TrackerRecord
	for _, tr := range t.m.Trackers {
		out = append(out, store.TrackerRecord{URL: tr.URL, Tier: tr.Tier})
	}
	return out
}

// capture builds the persistence snapshot from actor state.
func (t *torrent) capture() *persistSnap {
	rec := store.TorrentRecord{
		InfoHash:          t.m.InfoHash,
		Name:              t.m.Info.Name,
		RawInfo:           t.m.RawInfo,
		Trackers:          t.trackers,
		Private:           t.m.Info.Private,
		SizeBytes:         t.size,
		PieceLength:       t.m.Info.PieceLength,
		Files:             t.files,
		DownloadDir:       t.dir,
		DesiredState:      t.desired,
		StopReason:        string(t.stopReason),
		LimitOverride:     t.limitOverride,
		Bitfield:          t.savedBF,
		ResumeFingerprint: t.savedFP,
		UploadedTotal:     t.uploaded.Load(),
		DownloadedTotal:   t.downloaded.Load(),
		WastedTotal:       t.wasted.Load(),
		SeedingSeconds:    t.seedingSecs,
		QueuePosition:     t.queuePos,
		AddedAt:           t.addedAt,
		CompletedAt:       t.completedAt,
	}
	if t.state == StateError {
		rec.LastError = t.errMsg
	}
	checked := t.picker != nil && t.state != StateChecking
	if checked {
		rec.Bitfield = t.haveBytes()
	}
	return &persistSnap{rec: rec, st: t.store, statOK: checked}
}
