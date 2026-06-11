package memory

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DocumentTarget identifies one canonical document to check or rebuild.
type DocumentTarget struct {
	DatasetID  string
	DocumentID string
}

func normalizeDocumentTargets(scope Scope, targets []DocumentTarget) ([]DocumentTarget, error) {
	if targets == nil {
		return nil, nil
	}
	out := make([]DocumentTarget, 0, len(targets))
	for i, target := range targets {
		normalized := DocumentTarget{
			DatasetID:  strings.TrimSpace(target.DatasetID),
			DocumentID: strings.TrimSpace(target.DocumentID),
		}
		if normalized.DatasetID == "" {
			normalized.DatasetID = scope.DatasetID
		}
		if normalized.DocumentID == "" {
			return nil, errdefs.Validationf("memory: document target %d document_id is required", i)
		}
		if normalized.DatasetID == "" {
			return nil, errdefs.Validationf("memory: document target %q dataset_id is required", normalized.DocumentID)
		}
		out = append(out, normalized)
	}
	return out, nil
}

func cloneDocumentTargets(in []DocumentTarget) []DocumentTarget {
	if in == nil {
		return nil
	}
	out := make([]DocumentTarget, len(in))
	copy(out, in)
	return out
}

func documentTargetLabel(target DocumentTarget) string {
	return fmt.Sprintf("%s/%s", target.DatasetID, target.DocumentID)
}

func capabilitySelected(capabilities []Capability, capability Capability) bool {
	for _, candidate := range capabilities {
		if Capability(strings.TrimSpace(string(candidate))) == capability {
			return true
		}
	}
	return false
}
