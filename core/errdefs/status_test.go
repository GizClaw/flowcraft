package errdefs

import (
	"errors"
	"testing"
)

// TestClassifyStatusMapping pins the status → classification table every
// provider transport maps onto, with the cause chain preserved.
func TestClassifyStatusMapping(t *testing.T) {
	cause := errors.New("provider said no")
	for _, test := range []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"400 validation", 400, IsValidation},
		{"404 validation", 404, IsValidation},
		{"422 validation", 422, IsValidation},
		{"401 unauthorized", 401, IsUnauthorized},
		{"403 forbidden", 403, IsForbidden},
		{"409 conflict", 409, IsConflict},
		{"429 rate limit", 429, IsRateLimit},
		{"408 timeout", 408, IsTimeout},
		{"504 timeout", 504, IsTimeout},
		{"500 not available", 500, IsNotAvailable},
		{"503 not available", 503, IsNotAvailable},
		{"301 not available", 301, IsNotAvailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ClassifyStatus(test.status, cause)
			if !test.check(err) {
				t.Fatalf("ClassifyStatus(%d) = %v", test.status, err)
			}
			// The provider's own error stays in the chain so callers can
			// still inspect the typed SDK error.
			if !errors.Is(err, cause) {
				t.Fatalf("ClassifyStatus(%d) did not preserve the cause", test.status)
			}
		})
	}
}
