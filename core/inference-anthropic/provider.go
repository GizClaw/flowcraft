package anthropic

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/internal/cache"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

type driver struct {
	settings Settings
	client   client
	// schemas memoizes decoded tool schemas for this deployment: per instance, not
	// global, so one process serving several providers cannot evict across them.
	schemas *cache.LRU[anthropicgo.ToolInputSchemaParam]
}

// New builds a provider for one deployment.
func New(settings Settings) (inference.Provider, error) {
	normalized := settings.normalized()
	if err := normalized.Validate(); err != nil {
		return inference.Provider{}, err
	}
	driver := &driver{settings: normalized, client: newClient(normalized), schemas: cache.New[anthropicgo.ToolInputSchemaParam](normalized.SchemaCacheEntries)}
	unary, err := protocol.BindGenerate(
		ProviderID, driver.compile, driver.call, driver.decode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("anthropic: %w", err)
	}
	stream, err := protocol.BindGenerateStream(
		ProviderID, driver.compile, driver.open, driver.newStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("anthropic: %w", err)
	}

	provider := inference.Provider{
		Generator: protocol.BindGenerator(unary, stream),
	}
	if len(normalized.Models) > 0 {
		provider.Catalog = newCatalog(normalized.Models)
	}
	if err := provider.Validate(); err != nil {
		return inference.Provider{}, fmt.Errorf("anthropic: %w", err)
	}
	return provider, nil
}
