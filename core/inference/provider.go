package inference

import "fmt"

// Provider is one vendor's capabilities: the seams it serves plus the model
// line-up it declared.
//
// It is a struct, not an interface that grows a method per capability. Adding a
// seam is one field, and a nil field is the honest answer to "can this provider
// do it" — no stub returning UnsupportedOperation, no wrapper type per
// combination of supported seams.
//
// Generator is embedded, so a host that only wants turns keeps working:
// Provider satisfies Generator.
type Provider struct {
	Generator
	// Embedder is nil when the provider does not serve embeddings.
	Embedder Embedder
	// Decider is nil when the provider does not serve judgments.
	Decider Decider
	// Catalog is nil when the deployment declared no models.
	Catalog Catalog
}

// Validate checks that what the line-up declares is actually served.
//
// It is the one place that can answer "declared but uncallable": a model that
// declares an embedding width while no Embedder is bound, or decision support
// while no Decider is bound, would be a capability a caller can discover and
// never use. Failing at construction turns that into a deployment error.
func (p Provider) Validate() error {
	if p.Generator == nil {
		return fmt.Errorf("inference: provider has no generator")
	}
	if p.Catalog == nil {
		return nil
	}
	for _, descriptor := range p.Catalog.Models() {
		capabilities := descriptor.Capabilities
		if capabilities.EmbeddingDimensions > 0 && p.Embedder == nil {
			return fmt.Errorf(
				"inference: model %q declares an embedding width but the provider serves no embeddings",
				descriptor.ID.Name)
		}
		if capabilities.Decision && p.Decider == nil {
			return fmt.Errorf(
				"inference: model %q declares decision support but the provider serves no decisions",
				descriptor.ID.Name)
		}
	}
	return nil
}
