package api

// FROZEN CONTRACT (ADR-0011, ADR-0015, ADR-0017) for the torrent routes.
// This file is owned by the architect; the integrator implements the
// handlers in other files.
//
// # Route table (M3)
//
// Go's ServeMux panics when a method-less "/api/v1/torrents/preview" and
// "GET /api/v1/torrents/{id}" are both registered (neither is more
// specific). The M3 table therefore is:
//
//	GET  /healthz                     existing
//	/healthz                          405, Allow: GET, HEAD
//	POST /api/v1/torrents/preview     existing
//	GET  /api/v1/torrents/preview     405, Allow: POST   (literal beats {id})
//	GET  /api/v1/torrents             list
//	POST /api/v1/torrents             add
//	/api/v1/torrents                  405, Allow: GET, POST
//	GET  /api/v1/torrents/{id}        get
//	/api/v1/torrents/{id}             405: Allow: POST when id == "preview",
//	                                  otherwise Allow: GET (M6 adds PATCH, DELETE)
//	POST /api/v1/torrents/{id}/pause         (M4, ADR-0016)
//	POST /api/v1/torrents/{id}/resume        (M4)
//	POST /api/v1/torrents/{id}/stop-seeding  (M4)
//	/api/v1/torrents/{id}/<action>    405, Allow: POST, for those three
//	/api/                             404 not_found
//	/                                 SPA
//
// GET also serves HEAD (ServeMux does this for GET patterns).
//
// # POST /api/v1/torrents (multipart/form-data)
//
//   - "file": the .torrent (max MaxTorrentFileSize, same upload handling and
//     errors as preview: 413 payload_too_large, 400 invalid_torrent for a
//     file metainfo.Parse rejects, 400 invalid_request when "file" is
//     missing). "magnet" is M5: a magnet without a file is 400
//     invalid_request.
//   - "downloadDir": optional; passed to TorrentService.Add as given.
//   - "startPaused": optional, "true" or "false" (strconv.ParseBool); other
//     values are 400 invalid_request.
//   - Errors from Add: session.ErrInvalidDownloadDir -> 400
//     invalid_download_dir; session.ErrAlreadyExists -> 409 with the
//     AlreadyExists body {"error":{"code":"already_exists",...},"id":<hex>};
//     anything else -> 500 internal.
//   - Success: 201 with the Torrent JSON.
//
// # GET /api/v1/torrents and GET /api/v1/torrents/{id}
//
//   - The list is a JSON array (never null) of Torrent in List order.
//   - {id} must be 40 lowercase hex characters (TorrentId schema); an id
//     that is malformed or unknown is 404 not_found.
//
// # Lifecycle actions (M4, §16.4 G18)
//
//   - 204 with no body on success (including the no-op cases).
//   - session.ErrNotFound (or a malformed id) -> 404 not_found;
//     session.ErrInvalidState -> 409 invalid_state;
//     session.ErrNotComplete -> 409 not_complete; others -> 500 internal.
//   - A request body is ignored.
//
// When Options.Torrents is nil the torrent routes return 500 internal.

import (
	"context"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/session"
)

// TorrentService is the part of session.Session the HTTP layer uses.
// session.Session satisfies it.
type TorrentService interface {
	// Add registers a torrent (see session.Session.Add).
	Add(ctx context.Context, m *metainfo.MetaInfo, opts session.AddOptions) (session.Status, error)
	// List returns every torrent in add order.
	List() []session.Status
	// Get returns one torrent.
	Get(id [20]byte) (session.Status, bool)
	// Pause, Resume and StopSeeding are the lifecycle actions (see
	// session.Session).
	Pause(id [20]byte) error
	// Resume resumes, resumes seeding or retries a torrent.
	Resume(id [20]byte) error
	// StopSeeding stops seeding a complete torrent.
	StopSeeding(id [20]byte) error
}

var _ TorrentService = session.Session(nil)

// Torrent is the JSON body of the Torrent schema in spec/openapi.yaml, as
// served at M3. Mapping from session.Status:
//
//   - ID is lowercase hex; State is the string value; HasMetadata is true.
//   - SizeBytes and CompletedBytes are always set (non-null) at M3.
//   - Ratio is UploadedTotal / max(DownloadedTotal, CompletedBytes), 0 when
//     the denominator is 0 (§16.4 A6).
//   - AddedAt and CompletedAt are RFC 3339 in UTC; CompletedAt is null when
//     zero. ErrorMessage is null unless the state is error.
//   - CheckingProgress is null unless checking (then 0..1). StopReason is
//     the session's StopReason in state stopped, null otherwise.
//     EtaSeconds, SwarmSeeders, SwarmLeechers, RatioLimit and
//     SeedTimeLimitMin are null until M6; SeedingSeconds, DownloadLimit and
//     UploadLimit are 0.
type Torrent struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	State            string   `json:"state"`
	StopReason       *string  `json:"stopReason"`
	Private          bool     `json:"private"`
	HasMetadata      bool     `json:"hasMetadata"`
	SizeBytes        *int64   `json:"sizeBytes"`
	CompletedBytes   *int64   `json:"completedBytes"`
	Progress         float64  `json:"progress"`
	CheckingProgress *float64 `json:"checkingProgress"`
	DownloadRate     int64    `json:"downloadRate"`
	UploadRate       int64    `json:"uploadRate"`
	UploadedTotal    int64    `json:"uploadedTotal"`
	DownloadedTotal  int64    `json:"downloadedTotal"`
	WastedTotal      int64    `json:"wastedTotal"`
	Ratio            float64  `json:"ratio"`
	PeersConnected   int      `json:"peersConnected"`
	SeedsConnected   int      `json:"seedsConnected"`
	SwarmSeeders     *int     `json:"swarmSeeders"`
	SwarmLeechers    *int     `json:"swarmLeechers"`
	EtaSeconds       *int64   `json:"etaSeconds"`
	DownloadDir      string   `json:"downloadDir"`
	AddedAt          string   `json:"addedAt"`
	CompletedAt      *string  `json:"completedAt"`
	SeedingSeconds   int64    `json:"seedingSeconds"`
	RatioLimit       *float64 `json:"ratioLimit"`
	SeedTimeLimitMin *int     `json:"seedTimeLimitMin"`
	DownloadLimit    int64    `json:"downloadLimit"`
	UploadLimit      int64    `json:"uploadLimit"`
	ErrorMessage     *string  `json:"errorMessage"`
}

// AlreadyExistsBody is the 409 body of POST /api/v1/torrents (the
// AlreadyExists schema).
type AlreadyExistsBody struct {
	// Error has code already_exists.
	Error ErrorDetail `json:"error"`
	// ID is the existing torrent's id.
	ID string `json:"id"`
}
