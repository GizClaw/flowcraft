package api

import (
	"encoding/json"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// errorBody is the JSON shape every non-streaming error response
// uses. Keeping it stable across endpoints lets clients parse
// errors with one decoder regardless of which route they hit.
type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

// writeError translates a Go error into HTTP status + JSON body.
// The `reason` field carries the errdefs category name so clients
// can branch without string matching the human message.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	reason := "Internal"
	switch {
	case errdefs.IsNotFound(err):
		status = http.StatusNotFound
		reason = "NotFound"
	case errdefs.IsValidation(err):
		status = http.StatusBadRequest
		reason = "Validation"
	case errdefs.IsConflict(err):
		status = http.StatusConflict
		reason = "Conflict"
	case errdefs.IsNotAvailable(err):
		status = http.StatusServiceUnavailable
		reason = "NotAvailable"
	case errdefs.IsAborted(err):
		status = http.StatusGone
		reason = "Aborted"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: err.Error(), Reason: reason})
}

// writeJSON is the success-path counterpart to writeError. status
// is usually 200; callers wanting 201 / 202 pass that explicitly.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
