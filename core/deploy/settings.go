package deploy

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// decodeSettings reads one entry's settings block with the same discipline as the document
// around it: unknown fields are refused, so a misspelt setting is an error at load time
// rather than a silently ignored key that changes behaviour.
func decodeSettings(raw json.RawMessage, into any) error {
	if err := decode.DecodeInto(raw, into); err != nil {
		return errdefs.Validationf("deploy: settings: %v", err)
	}
	return nil
}

// sortStrings sorts in place, so the call sites do not each import sort.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
