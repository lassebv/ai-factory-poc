package session

import (
	"errors"
	"fmt"
	"math"

	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
)

func floatBits(f float64) uint64 { return math.Float64bits(f) }
func floatFrom(u uint64) float64 { return math.Float64frombits(u) }

// haveSet returns the verified pieces (kept in sync with the picker so
// that lookups do not copy).
func (t *torrent) haveSet() *piece.Bitfield {
	if t.have == nil {
		t.have = piece.NewBitfield(t.layout.NumPieces)
	}
	return t.have
}

// markHave records a verified piece in the picker and the local set.
func (t *torrent) markHave(index int) {
	t.picker.PieceVerified(index)
	t.haveSet().Set(index)
	t.haveGen++
}

// haveBytes returns the verified bitfield in wire format (cached; the
// returned slice is never modified, so snapshots may share it).
func (t *torrent) haveBytes() []byte {
	if t.picker == nil {
		return nil
	}
	if t.haveBF == nil || t.haveN != t.haveGen {
		t.haveBF = t.haveSet().Bytes()
		t.haveN = t.haveGen
	}
	return t.haveBF
}

// written handles a finished block write.
func (t *torrent) written(ev evWritten) {
	t.writes[ev.index]--
	if t.writes[ev.index] <= 0 {
		delete(t.writes, ev.index)
	}
	if ev.err != nil {
		if t.state != StateError && !errors.Is(ev.err, storage.ErrClosed) {
			t.fail(fmt.Errorf("write piece %d: %w", ev.index, ev.err))
		}
		return
	}
	t.maybeVerify(ev.index)
}

// maybeVerify hashes a complete piece once all of its writes are done.
func (t *torrent) maybeVerify(index int) {
	if !t.toVerify[index] || t.writes[index] > 0 || t.state == StateError || t.store == nil {
		return
	}
	delete(t.toVerify, index)
	st := t.store
	err := st.VerifyPieceAsync(index, func(ok bool, err error) {
		if err == nil && ok {
			err = st.MarkVerified(index)
		}
		t.send(evVerified{index, ok, err})
	})
	if err != nil {
		t.fail(fmt.Errorf("verify piece %d: %w", index, err))
	}
}

func (t *torrent) verified(ev evVerified) {
	if t.state == StateError || t.picker == nil {
		return
	}
	if ev.err != nil {
		if !errors.Is(ev.err, storage.ErrClosed) {
			t.fail(fmt.Errorf("verify piece %d: %w", ev.index, ev.err))
		}
		return
	}
	if !ev.ok {
		t.s.log.Warn("piece failed the hash check", "torrent", t.m.HexHash(), "piece", ev.index)
		t.wasted.Add(t.layout.PieceSize(ev.index))
		for _, ip := range t.picker.PieceFailed(ev.index) {
			if t.s.strike(ip) {
				t.dropIP(ip)
				t.s.broadcastBan(t, ip)
			}
		}
		t.fillAll()
		return
	}
	t.markHave(ev.index)
	t.completed.Add(t.store.WantedBytes(ev.index))
	for _, p := range t.peers {
		if !p.bf.Has(ev.index) {
			_ = p.conn.Send(peer.Message{ID: peer.MsgHave, Index: uint32(ev.index)})
		}
		t.updateInterest(p)
	}
	if t.picker.Done() && t.state == StateDownloading {
		t.finish()
	}
}
