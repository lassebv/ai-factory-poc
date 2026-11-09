package choke

import (
	"math/rand/v2"
	"slices"
	"time"

	"torrentd/internal/bt/piece"
)

// choker implements Choker.
type choker struct {
	rnd       *rand.Rand
	regular   []piece.PeerKey
	opt       piece.PeerKey
	hasOpt    bool
	optChosen time.Time
}

func newChoker(rnd *rand.Rand) Choker {
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(1, 1))
	}
	return &choker{rnd: rnd}
}

// Optimistic returns the current optimistic peer.
func (c *choker) Optimistic() (piece.PeerKey, bool) { return c.opt, c.hasOpt }

func snubbed(now time.Time, p Peer, seeding bool) bool {
	if seeding || !p.AmInterested {
		return false
	}
	last := p.LastBlock
	if last.IsZero() {
		last = p.ConnectedAt
	}
	return now.Sub(last) >= SnubTimeout
}

func rate(p Peer, seeding bool) int64 {
	if seeding {
		return p.UploadRate
	}
	return p.DownloadRate
}

// ranked returns the interested peers ordered for regular slots:
// non-snubbed before snubbed, then by rate, then current holders first,
// then randomly.
func (c *choker) ranked(now time.Time, peers []Peer, seeding bool) []Peer {
	var in []Peer
	for _, p := range peers {
		if p.Interested {
			in = append(in, p)
		}
	}
	c.rnd.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	held := map[piece.PeerKey]bool{}
	for _, k := range c.regular {
		held[k] = true
	}
	slices.SortStableFunc(in, func(a, b Peer) int {
		sa, sb := snubbed(now, a, seeding), snubbed(now, b, seeding)
		if sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		ra, rb := rate(a, seeding), rate(b, seeding)
		if ra != rb {
			if ra > rb {
				return -1
			}
			return 1
		}
		if held[a.Key] != held[b.Key] {
			if held[a.Key] {
				return -1
			}
			return 1
		}
		return 0
	})
	return in
}

// pickOptimistic chooses a random peer of rest, preferring one other than
// avoid.
func (c *choker) pickOptimistic(now time.Time, rest []Peer, avoid piece.PeerKey, hasAvoid bool) {
	c.hasOpt = false
	var cands []piece.PeerKey
	for _, p := range rest {
		if !hasAvoid || p.Key != avoid {
			cands = append(cands, p.Key)
		}
	}
	if len(cands) == 0 && hasAvoid {
		for _, p := range rest {
			if p.Key == avoid {
				cands = append(cands, avoid)
			}
		}
	}
	if len(cands) == 0 {
		return
	}
	c.opt, c.hasOpt, c.optChosen = cands[c.rnd.IntN(len(cands))], true, now
}

// Rechoke re-evaluates every slot.
func (c *choker) Rechoke(now time.Time, peers []Peer, seeding bool) []piece.PeerKey {
	in := c.ranked(now, peers, seeding)
	keepOpt := false
	if c.hasOpt && now.Sub(c.optChosen) < OptimisticInterval {
		for _, p := range in {
			if p.Key == c.opt {
				keepOpt = true
			}
		}
	}
	prevOpt, hadOpt := c.opt, c.hasOpt
	var regular []piece.PeerKey
	var rest []Peer
	for _, p := range in {
		switch {
		case keepOpt && p.Key == c.opt:
		case len(regular) < RegularSlots && !snubbed(now, p, seeding):
			regular = append(regular, p.Key)
		default:
			rest = append(rest, p)
		}
	}
	if !keepOpt {
		c.pickOptimistic(now, rest, prevOpt, hadOpt)
	}
	for _, p := range rest {
		if len(regular) >= RegularSlots {
			break
		}
		if !c.hasOpt || p.Key != c.opt {
			regular = append(regular, p.Key)
		}
	}
	return c.finish(regular)
}

// Fill keeps the current slots and fills free ones.
func (c *choker) Fill(now time.Time, peers []Peer, seeding bool) []piece.PeerKey {
	in := c.ranked(now, peers, seeding)
	eligible := map[piece.PeerKey]bool{}
	for _, p := range in {
		eligible[p.Key] = true
	}
	if c.hasOpt && !eligible[c.opt] {
		c.hasOpt = false
	}
	var regular []piece.PeerKey
	taken := map[piece.PeerKey]bool{}
	for _, k := range c.regular {
		if eligible[k] && !taken[k] && !(c.hasOpt && k == c.opt) {
			regular = append(regular, k)
			taken[k] = true
		}
	}
	if c.hasOpt {
		taken[c.opt] = true
	}
	var rest []Peer
	for _, p := range in {
		if taken[p.Key] {
			continue
		}
		if len(regular) < RegularSlots {
			regular = append(regular, p.Key)
			taken[p.Key] = true
			continue
		}
		rest = append(rest, p)
	}
	if !c.hasOpt {
		c.pickOptimistic(now, rest, 0, false)
	}
	return c.finish(regular)
}

func (c *choker) finish(regular []piece.PeerKey) []piece.PeerKey {
	c.regular = regular
	out := slices.Clone(regular)
	if c.hasOpt {
		out = append(out, c.opt)
	}
	return slices.Clone(out)
}
