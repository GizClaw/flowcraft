package read

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// AllSourcesFailed wraps per-source failures when every activated source
// returned an error and produced zero candidates. When all underlying
// errors are NotAvailable the aggregate maps to NotAvailable; otherwise
// Internal so HTTP shims do not treat a total recall outage as 400.
func AllSourcesFailed(sourceErrs []error) error {
	if len(sourceErrs) == 0 {
		return nil
	}
	joined := errors.Join(sourceErrs...)
	base := fmt.Errorf("recall.Recall: all sources failed: %w", joined)
	allNotAvailable := true
	for _, err := range sourceErrs {
		if !errdefs.IsNotAvailable(err) {
			allNotAvailable = false
			break
		}
	}
	if allNotAvailable {
		return errdefs.NotAvailable(base)
	}
	return errdefs.Internal(base)
}
