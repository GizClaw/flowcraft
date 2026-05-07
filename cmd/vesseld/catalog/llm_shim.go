package catalog

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// fixedClientResolver is the trivial one-entry LLMResolver shim
// used to bridge places that expect an llm.LLMResolver (e.g.
// vessel.LLMReachableProbe) to the daemon's per-profile client
// map. Resolve always returns the wrapped client regardless of
// the model string the caller passes.
//
// This is fine for the probe — it only needs *some* client to ping
// — but the shim is intentionally not exported: anyone building
// their own factory should pull from Deps.LLMClients directly.
type fixedClientResolver struct {
	client llm.LLM
}

func (f *fixedClientResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) {
	return f.client, nil
}

// InvalidateCache is a no-op: the wrapped client is reused for
// the life of the daemon, with the underlying provider client
// owning its own pool refresh logic.
func (f *fixedClientResolver) InvalidateCache(_ ...llm.InvalidateOption) {}
