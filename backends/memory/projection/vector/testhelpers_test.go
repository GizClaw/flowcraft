package vector

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/core/inference"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// newTestWorkspace returns an isolated local workspace for one test.
func newTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// shardOf names the shard a conversation's chat records live in. The fixtures
// below are chat records, so their shard is a conversation's.
func shardOf(conversationID string) string {
	shard, err := component.ConversationShard(conversationID)
	if err != nil {
		panic(err)
	}
	return shard
}

// datasetShardOf names the shard one dataset's documents live in.
func datasetShardOf(datasetID string) string {
	shard, err := component.DatasetShard(datasetID)
	if err != nil {
		panic(err)
	}
	return shard
}

// addressOf names one shard of one scope, as the store addresses it.
func addressOf(scope corememory.Scope, shard string) projectionstore.Address {
	return projectionstore.Address{Scope: scope, Shard: shard}
}

// newTestRuntime builds an inference assembly from provider definitions
// through the same resource factory deployments use.
func newTestRuntime(t *testing.T, providers []inference.ProviderDefinition) *inference.Assembly {
	t.Helper()
	deps := make(map[string]any, len(providers))
	for index, provider := range providers {
		deps[fmt.Sprintf("provider.%d", index)] = provider
	}
	value, err := inference.Factory{}.New(context.Background(), resource.NewTestInput(deps))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
