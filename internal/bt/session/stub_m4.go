package session

// M4 placeholders so the tree builds; the integrator replaces this file.

import (
	"context"
	"errors"
)

var errM4 = errors.New("session: not implemented (M4)")

// Pause is not implemented yet (M4).
func (s *session) Pause(context.Context, [20]byte) error { return errM4 }

// Resume is not implemented yet (M4).
func (s *session) Resume(context.Context, [20]byte) error { return errM4 }

// StopSeeding is not implemented yet (M4).
func (s *session) StopSeeding(context.Context, [20]byte) error { return errM4 }
