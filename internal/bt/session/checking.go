package session

import (
	"context"
	"fmt"

	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
)

// activate brings a torrent towards downloading or seeding: it opens (and,
// if needed, checks) storage first.
func (t *torrent) activate() {
	t.state = StateChecking
	t.errMsg = ""
	switch {
	case t.store == nil:
		if !t.opening {
			t.open()
		}
	case t.needCheck || !t.ready:
		t.startCheck()
	default:
		t.start()
	}
}

// open validates resume data and opens storage on the disk pool.
func (t *torrent) open() {
	t.opening = true
	gen, info, dir := t.gen, t.m.Info, t.dir
	var have *piece.Bitfield
	var fp string
	restored := t.restored != nil
	if restored {
		have, fp = t.picker.Have(), t.fprint
	}
	clean := t.s.clean
	t.goJob(t.s.disk, func() {
		ev := evOpened{gen: gen}
		files := statFiles(info, dir)
		switch {
		case restored && have.Count() > 0:
			if files.allMissing() {
				ev.missing = true
				t.send(ev)
				return
			}
			ev.recheck = !files.matches(fp, clean)
		case !restored:
			ev.recheck = files.anyData()
		}
		ev.st, ev.err = storage.Open(dir, info, storage.Options{})
		if ev.err == nil && restored && !ev.recheck {
			ev.trusted = true
			for i := 0; i < have.Len(); i++ {
				if have.Has(i) {
					if err := ev.st.MarkVerified(i); err != nil {
						ev.err = err
						break
					}
				}
			}
		}
		if !t.send(ev) && ev.st != nil {
			_ = ev.st.Close()
		}
	})
}

// opened handles the result of open.
func (t *torrent) opened(ev evOpened) {
	t.opening = false
	if ev.missing {
		t.failMsg(FilesMissingMessage)
		return
	}
	if ev.err != nil {
		if ev.st != nil {
			_ = ev.st.Close()
		}
		t.fail(fmt.Errorf("open storage: %w", ev.err))
		return
	}
	t.store = ev.st
	t.restored = nil
	if ev.recheck {
		t.needCheck = true
		if t.picker.HaveCount() > 0 {
			t.resetPicker()
		}
	} else {
		t.ready = true
	}
	if ev.gen != t.gen || t.desired != DesiredActive {
		return // paused or stopped meanwhile
	}
	t.activate()
}

// resetPicker forgets every verified piece (before a full recheck). No
// peers are registered while checking.
func (t *torrent) resetPicker() {
	t.picker = t.newPicker()
	t.completed.Store(0)
}

// startCheck verifies every piece not yet verified, on the hash pool.
func (t *torrent) startCheck() {
	t.ready = false
	st, gen := t.store, t.gen
	t.checking = 0
	have := t.picker.Have()
	for i := 0; i < t.layout.NumPieces; i++ {
		if have.Has(i) {
			continue
		}
		t.checking++
		t.goJob(t.s.hash, func() {
			if t.ctx.Err() != nil {
				t.send(evChecked{gen: gen, index: i, err: context.Canceled})
				return
			}
			ok, err := st.VerifyPiece(i)
			if err == nil && ok {
				err = st.MarkVerified(i)
			}
			t.send(evChecked{gen: gen, index: i, ok: ok, err: err})
		})
	}
	if t.checking == 0 {
		t.checkDone()
	}
}

func (t *torrent) checked(ev evChecked) {
	if ev.gen != t.gen || t.state != StateChecking {
		return
	}
	t.checking--
	if ev.err != nil {
		t.fail(fmt.Errorf("check piece %d: %w", ev.index, ev.err))
		return
	}
	if ev.ok {
		t.picker.PieceVerified(ev.index)
		t.completed.Add(t.store.WantedBytes(ev.index))
	}
	if t.checking == 0 {
		t.checkDone()
	}
}

func (t *torrent) checkDone() {
	t.needCheck = false
	t.ready = true
	t.start()
}
