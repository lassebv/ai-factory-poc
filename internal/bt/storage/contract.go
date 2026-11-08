// Package storage maps a torrent's pieces onto files in the download dir,
// preallocates them as sparse .part files, verifies pieces with SHA-1,
// renames each file when all of its pieces are verified, and provides the
// process-wide disk and hash worker pools and the fast-resume planner (§4.1
// Storage and Resume, §4.5, §16.1 C2, §16.2 G21, §16.3 C3, G8, D9).
//
// FROZEN CONTRACT (ADR-0011, ADR-0013, ADR-0017, ADR-0019). This file is
// owned by the architect. The implementer provides, in their own files
// (replacing stub_m4.go):
//
//	func open(dir string, info *metainfo.Info, opts Options) (Storage, error)
//	func newPool(workers int) Pool
//	func stat(dir string, info *metainfo.Info) (Fingerprint, error)
//	func planResume(in ResumeInput) ResumePlan
//
// # On-disk layout
//
//   - Every non-pad file i lives at filepath.Join(dir, info.Files[i].Path...)
//     (for multi-file torrents Path already starts with the torrent name).
//   - While a file has unverified pieces it is named <path>.part (PartSuffix).
//     As soon as every piece overlapping it is verified (MarkVerified), it is
//     renamed to <path>. A zero-length file has no pieces and is created
//     directly under its final name by Open.
//   - BEP 47 pad files (File.Pad) are never created, opened or renamed.
//     Reads of pad ranges yield zeros; writes to pad ranges are discarded.
//   - Nothing is ever created outside dir (STOR-07). Open and Stat reject
//     any file whose path is not filepath.IsLocal relative to dir.
//   - Boundary-piece .parts files (G21, skipped files) are M6 scope; M4 has
//     no file priorities, so every non-pad file is wanted.
//
// # Concurrency and resources (ADR-0017)
//
//   - A Storage is safe for concurrent use by any number of goroutines.
//   - The §4.5 worker pools are process-wide: the session creates one
//     Pools and passes it to every Open. WriteBlock, ReadBlock, Sync and
//     MarkVerified's renames run on Pools.Disk; VerifyPiece and
//     VerifyPieceAsync run on Pools.Hash (the hash worker reads the piece
//     itself, not through Disk, so the pools can never deadlock on each
//     other). The blocking methods block their caller until the work ran.
//     So at most Disk.Workers() disk operations and Hash.Workers() hashes
//     run at once in the whole process, with exactly that many worker
//     goroutines, and no goroutine per block anywhere.
//   - VerifyPiece streams the piece through SHA-1 with a buffer of at most
//     MaxHashBuffer bytes whatever the piece size (MEM-01: 8 MiB pieces).
//   - At most Options.MaxOpenFiles *os.File handles are open at any moment
//     (an LRU cache, STOR-06), including during Open's preallocation.
//   - After Close returns, every file handle is closed and other methods
//     return ErrClosed. Close waits for this Storage's running operations;
//     operations queued but not yet started fail with ErrClosed.
//
// # Durability (C3, ADR-0002)
//
// Sync makes every block whose WriteBlock returned before Sync was called
// durable (fsync), including files whose handle the LRU closed in between
// (a dirty handle is fsynced before it is evicted, or remembered and
// reopened). The session's flush protocol relies on this: capture the
// bitfield, Sync, then persist the bitfield.
package storage

import (
	"context"
	"errors"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/piece"
)

// PartSuffix is appended to the name of a file that is not yet complete.
const PartSuffix = ".part"

// Defaults for Options (D9).
const (
	// DefaultMaxOpenFiles is the file-handle LRU size.
	DefaultMaxOpenFiles = 128
	// DefaultDiskWorkers is the number of concurrent disk operations.
	DefaultDiskWorkers = 4
)

// MaxHashBuffer bounds the read buffer of one piece verification.
const MaxHashBuffer = 256 << 10

// Errors returned by Storage.
var (
	// ErrClosed is returned by every method after Close.
	ErrClosed = errors.New("storage: closed")
	// ErrOutOfRange is returned for a piece index, offset or length outside
	// the torrent's layout.
	ErrOutOfRange = errors.New("storage: out of range")
	// ErrUnsafePath is returned by Open when a file path would escape dir.
	ErrUnsafePath = errors.New("storage: unsafe file path")
)

// Options tunes a Storage. Zero values select the defaults.
type Options struct {
	// MaxOpenFiles bounds open file handles (0 = DefaultMaxOpenFiles).
	MaxOpenFiles int
	// Pools are the shared worker pools. nil means private pools of
	// DiskWorkers and HashWorkers workers, owned and closed by this Storage
	// (tests and tools only; the session always passes shared Pools).
	Pools *Pools
	// DiskWorkers sizes private pools only (0 = DefaultDiskWorkers).
	DiskWorkers int
	// HashWorkers sizes private pools only (0 = DefaultHashWorkers()).
	HashWorkers int
}

// Pool is a fixed set of worker goroutines with a FIFO queue. All methods
// are safe for concurrent use. A fn run by a Pool must not call Run on the
// same Pool (it could deadlock); fns run by Storage never do.
type Pool interface {
	// Run queues fn and waits until it has run. It returns ctx.Err() if
	// ctx is done before fn started (fn then never runs) and ErrClosed if
	// the Pool is closed before fn started.
	Run(ctx context.Context, fn func()) error
	// Submit queues fn and returns at once; fn runs later on a worker. It
	// returns ErrClosed after Close (fn never runs). The queue is not
	// bounded by the Pool: callers bound what they submit (the session
	// submits at most one hash per pending piece).
	Submit(fn func()) error
	// Workers returns the number of worker goroutines.
	Workers() int
	// Close stops accepting work, drops queued fns that have not started,
	// waits for running fns and for every worker goroutine to exit. It is
	// idempotent.
	Close()
}

// Pools are the process-wide worker pools (§4.5).
type Pools struct {
	// Disk runs block writes and reads, syncs and renames.
	Disk Pool
	// Hash runs piece verification.
	Hash Pool
}

// NewPool starts a Pool of workers goroutines (workers < 1 means 1).
func NewPool(workers int) Pool { return newPool(workers) }

// NewPools returns Pools with the given sizes; 0 selects
// DefaultDiskWorkers and DefaultHashWorkers().
func NewPools(diskWorkers, hashWorkers int) *Pools {
	if diskWorkers <= 0 {
		diskWorkers = DefaultDiskWorkers
	}
	if hashWorkers <= 0 {
		hashWorkers = DefaultHashWorkers()
	}
	return &Pools{Disk: NewPool(diskWorkers), Hash: NewPool(hashWorkers)}
}

// Close closes both pools.
func (p *Pools) Close() {
	p.Disk.Close()
	p.Hash.Close()
}

// Storage is one torrent's data on disk. Piece indexes and offsets refer to
// the torrent's flat byte view (all files in order, pad files included).
type Storage interface {
	// WriteBlock writes data at offset begin within piece index. The range
	// may span several files and pad files. It returns ErrOutOfRange if
	// begin+len(data) exceeds the piece size.
	WriteBlock(index int, begin int, data []byte) error
	// ReadBlock fills buf from offset begin within piece index (zeros for
	// pad ranges). It returns ErrOutOfRange like WriteBlock.
	ReadBlock(index int, begin int, buf []byte) error
	// VerifyPiece reads piece index from disk (pad ranges as zeros) and
	// reports whether its SHA-1 equals the hash in the info dict. A false
	// result with a nil error is a hash mismatch. It does not change any
	// state; call MarkVerified for a piece that passed.
	VerifyPiece(index int) (bool, error)
	// VerifyPieceAsync queues VerifyPiece(index) on the hash pool and
	// returns at once; done receives the result on the hash worker. If it
	// returns nil, done is called exactly once (with ErrClosed if the
	// Storage was closed before the hash ran), unless the Pools were
	// closed first. It never blocks, so the torrent actor may call it.
	VerifyPieceAsync(index int, done func(ok bool, err error)) error
	// Sync makes every completed WriteBlock durable (see Durability).
	Sync() error
	// MarkVerified records piece index as verified and renames every file
	// whose overlapping pieces are now all verified from <path>.part to
	// <path>. It is idempotent. Data of a renamed file stays readable.
	MarkVerified(index int) error
	// PieceSize returns the size of piece index in the flat view
	// (including pad bytes), or 0 if out of range.
	PieceSize(index int) int64
	// WantedBytes returns the number of bytes of piece index that belong
	// to non-pad files: what the piece adds to completedBytes. 0 if out of
	// range.
	WantedBytes(index int) int64
	// Close flushes and closes all file handles. It is idempotent.
	Close() error
}

// Open prepares the files of info under dir and returns their Storage.
//
// dir must be absolute; it is created with its parents if missing. For each
// non-pad file, Open creates the parent directories and, unless <path> or
// <path>.part already exists, creates <path>.part sparsely preallocated to
// the file's length (truncate, no data written). Zero-length files are
// created as <path>. Open never deletes or shortens existing data; an
// existing <path> is used in place, an existing shorter <path>.part is
// extended. Open treats every piece as unverified; the session calls
// MarkVerified for the pieces it trusts or has verified.
func Open(dir string, info *metainfo.Info, opts Options) (Storage, error) {
	return open(dir, info, opts)
}

// DefaultHashWorkers is max(1, GOMAXPROCS/2).
func DefaultHashWorkers() int { return defaultHashWorkers() }

// FileStat is the resume fingerprint entry of one non-pad file (§4.1 fast
// resume). It is JSON-encoded into torrents.resume_fingerprint.
type FileStat struct {
	// Path is the file path relative to dir, "/"-separated (no .part).
	Path string `json:"path"`
	// Exists is true if <path> or <path>.part exists as a regular file.
	Exists bool `json:"exists"`
	// Size is the size of the existing file, 0 if it does not exist.
	Size int64 `json:"size"`
	// ModTime is the mtime in Unix nanoseconds, 0 if it does not exist.
	ModTime int64 `json:"mtime"`
}

// Fingerprint is one FileStat per non-pad file, in info-dict order.
type Fingerprint []FileStat

// Stat returns the current Fingerprint of info under dir without creating,
// opening for write or modifying anything. When both <path> and
// <path>.part exist, <path> wins. Errors other than "does not exist" (e.g.
// permission denied) are returned.
func Stat(dir string, info *metainfo.Info) (Fingerprint, error) { return stat(dir, info) }

// ResumeInput is what the session knows about a torrent at startup or add.
type ResumeInput struct {
	// Info is the torrent's info dict.
	Info *metainfo.Info
	// Saved is the persisted bitfield; nil if none (a fresh add, or never
	// flushed).
	Saved *piece.Bitfield
	// SavedFingerprint is the persisted fingerprint; nil if none.
	SavedFingerprint Fingerprint
	// Current is Stat's result now.
	Current Fingerprint
	// Clean reports whether the previous shutdown was clean (C3).
	Clean bool
}

// ResumePlan says what to trust, what to hash and whether to fail.
type ResumePlan struct {
	// FilesMissing is the G8 case: Saved has at least one piece and every
	// non-pad file of non-zero length is missing. The torrent goes to
	// error "Files missing" and Open must not be called (it would
	// recreate the files).
	FilesMissing bool
	// Trusted is the pieces to mark verified without hashing.
	Trusted *piece.Bitfield
	// Recheck is the pieces to hash, ascending; each that passes is
	// verified, the rest are downloaded.
	Recheck []int
}

// PlanResume decides fast resume (§4.1, C3, G8, ADR-0019). A file is
// *suspect* if it is missing, or if:
//
//   - Clean: its Size or ModTime differs from SavedFingerprint (every file
//     is suspect when SavedFingerprint is nil or has another length);
//   - not Clean: its Size differs from SavedFingerprint (from the file's
//     length when SavedFingerprint is nil). mtimes are ignored (ADR-0002).
//
// With Saved nil there is nothing to trust: Recheck is every piece that
// overlaps at least one existing file of non-zero size (a torrent added
// over existing data is verified, SEED-01), and nothing if no file exists.
// Otherwise Trusted is the Saved pieces overlapping no suspect file, and
// Recheck is the Saved pieces that overlap a suspect file and no missing
// file (pieces overlapping a missing file are simply downloaded again).
// Pieces not in Saved are never trusted. Pad files are ignored. PlanResume
// is pure: no I/O.
func PlanResume(in ResumeInput) ResumePlan { return planResume(in) }
