// Package storage maps a torrent's pieces onto files in the download dir,
// preallocates them as sparse .part files, verifies pieces with SHA-1 and
// renames each file when all of its pieces are verified (§4.1 Storage, §4.5,
// §16.1 C2, §16.2 G21, D9).
//
// FROZEN CONTRACT (ADR-0011, ADR-0013, ADR-0017). This file is owned by the architect.
// The implementer provides, in their own files (replacing stub.go):
//
//	func open(dir string, info *metainfo.Info, opts Options) (Storage, error)
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
//   - Nothing is ever created outside dir (STOR-07). Open rejects any file
//     whose path is not filepath.IsLocal relative to dir.
//   - Boundary-piece .parts files (G21, skipped files) are M6 scope; M3 has
//     no file priorities, so every non-pad file is wanted.
//
// # Concurrency and resources
//
//   - A Storage is safe for concurrent use by any number of goroutines.
//   - It realizes the §4.5 worker pools as bounded concurrency: at most
//     Options.DiskWorkers ReadBlock/WriteBlock/rename operations and at most
//     Options.HashWorkers VerifyPiece calls run at once; excess callers block
//     until a slot is free. Callers that must not block (the torrent actor)
//     call these methods from their own goroutines.
//   - At most Options.MaxOpenFiles *os.File handles are open at any moment
//     (an LRU cache, STOR-06), including during Open's preallocation.
//   - Storage starts no goroutines that outlive a call. After Close returns,
//     every file handle is closed and other methods return ErrClosed.
package storage

import (
	"errors"

	"torrentd/internal/bt/metainfo"
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
	// DiskWorkers bounds concurrent disk I/O (0 = DefaultDiskWorkers).
	DiskWorkers int
	// HashWorkers bounds concurrent VerifyPiece calls
	// (0 = max(1, GOMAXPROCS/2)).
	HashWorkers int
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
	// Sync fsyncs every file written since the last Sync (§16.3 C3: piece
	// data is fsynced before a bitfield containing it is persisted). Added
	// at M4 (ADR-0017).
	Sync() error
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
// extended. Open treats every piece as unverified; a caller restoring
// resume data calls MarkVerified for each trusted piece.
func Open(dir string, info *metainfo.Info, opts Options) (Storage, error) {
	return open(dir, info, opts)
}
