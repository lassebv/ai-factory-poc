package api

import (
	"encoding/json"
	"net/http"
)

// Error codes from the ErrorCode schema in spec/openapi.yaml (§16.7 G12).
const (
	CodeNotFound           = "not_found"
	CodeInvalidRequest     = "invalid_request"
	CodeInvalidState       = "invalid_state"
	CodeNotComplete        = "not_complete"
	CodeNoMetadata         = "no_metadata"
	CodeAlreadyExists      = "already_exists"
	CodeInvalidTorrent     = "invalid_torrent"
	CodeInvalidMagnet      = "invalid_magnet"
	CodeInvalidDownloadDir = "invalid_download_dir"
	CodeTargetExists       = "target_exists"
	CodePortInUse          = "port_in_use"
	CodePayloadTooLarge    = "payload_too_large"
	CodeForbiddenOrigin    = "forbidden_origin"
	CodeForbiddenHost      = "forbidden_host"
	CodeInternal           = "internal"
)

// ErrorBody is the JSON body of every error response (the Error schema).
type ErrorBody struct {
	// Error holds the machine-readable code and the human-readable message.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner object of ErrorBody.
type ErrorDetail struct {
	// Code is one of the Code* constants.
	Code string `json:"code"`
	// Message is shown inline in the UI; it is never empty.
	Message string `json:"message"`
}

// WriteError writes an error response with the given status, code and
// message, using the Error schema.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	if message == "" {
		message = http.StatusText(status)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Del("Content-Length")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}
