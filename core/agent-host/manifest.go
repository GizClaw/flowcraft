package host

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/goccy/go-json"
)

// manifestHash renders a manifest component as a stable hash.
func manifestHash(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// manifestModels is the model catalog the deployment declared.
func manifestModels(rt *runtime.Runtime) string {
	if rt == nil {
		return ""
	}
	return manifestHash(rt.Config().Inference.Models)
}

// manifestTools is the tool schema the deployment wired.
func manifestTools(rt *runtime.Runtime) string {
	if rt == nil {
		return ""
	}
	return manifestHash(rt.Tools().Definitions())
}
