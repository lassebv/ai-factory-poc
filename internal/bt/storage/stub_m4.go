package storage

// M4 placeholders so the tree builds; the implementer replaces this file
// and reworks storage.go to the M4 contract (ADR-0017, ADR-0019).

import (
	"context"
	"errors"
	"runtime"

	"torrentd/internal/bt/metainfo"
)

func newPool(int) Pool { return stubPool{} }

type stubPool struct{}

func (stubPool) Run(context.Context, func()) error { return ErrClosed }
func (stubPool) Submit(func()) error               { return ErrClosed }
func (stubPool) Workers() int                      { return 0 }
func (stubPool) Close()                            {}

var errM4 = errors.New("storage: not implemented (M4)")

func stat(string, *metainfo.Info) (Fingerprint, error) { return nil, errM4 }

func planResume(ResumeInput) ResumePlan { panic("storage: PlanResume not implemented (M4)") }

func defaultHashWorkers() int { return max(1, runtime.GOMAXPROCS(0)/2) }

// VerifyPieceAsync is not implemented yet (M4).
func (s *storage) VerifyPieceAsync(int, func(bool, error)) error { return ErrClosed }

// Sync is not implemented yet (M4).
func (s *storage) Sync() error { return errM4 }
