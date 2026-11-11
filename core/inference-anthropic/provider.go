package anthropic

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

type driver struct {
	settings Settings
	client   client
}

// New builds a provider for one deployment.
func New(settings Settings) (inference.Provider, error) {
	normalized := settings.normalized()
	if err := normalized.Validate(); err != nil {
		return inference.Provider{}, err
	}
	driver := &driver{settings: normalized, client: newClient(normalized)}
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
