package choke

// Placeholders so the tree builds; the implementer replaces this file.

import (
	"math/rand/v2"

	"torrentd/internal/clock"
)

func newChoker(*rand.Rand) Choker { panic("choke: NewChoker not implemented (M4)") }

func newMeter(clock.Clock) Meter { panic("choke: NewMeter not implemented (M4)") }
