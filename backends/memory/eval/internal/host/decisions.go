package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/driver/openai"
)

// Model names one model a live provider is declared to serve. The kind is what
// the assembly routes by: a model asked for an operation it was not declared
// with fails at the call, which is the difference a reading depends on.
type Model struct {
	Name string
	Kind string
}

// OpenAIProvider builds an inference assembly over one OpenAI provider and the
// models it is declared to serve. It is the wiring a live lane needs when it
// measures one surface rather than a deployment: eval/deploy.yaml names the
// providers a harness run uses, and a lane that wants a decision model (or a
// model beside the ones the document declares) would otherwise have to rewrite
// that document to say so.
//
// The provider's own endpoint is the default: a proxy that reorders or filters
// answers would be measured as the model giving them.
func OpenAIProvider(ctx context.Context, baseURL, apiKey string, models []Model) (*inference.Assembly, error) {
	if ctx == nil {
		return nil, fmt.Errorf("host: context is required")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("host: an api key is required")
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("host: at least one model is required")
	}
	declared := make([]map[string]any, len(models))
	for index, model := range models {
		if model.Name == "" || model.Kind == "" {
			return nil, fmt.Errorf("host: model %d needs a name and a kind", index)
		}
		// Every model here reads text; a generate model also has to say what
		// it writes, because the provider refuses one that writes nothing.
		capabilities := map[string]any{"inputs": []string{"text"}}
		if model.Kind == "generate" {
			capabilities["outputs"] = []string{"text"}
		}
		declared[index] = map[string]any{
			"name":         model.Name,
			"kind":         model.Kind,
			"capabilities": capabilities,
		}
	}
	settings, err := json.Marshal(map[string]any{
		"id": "openai",
		"spec": map[string]any{
			"endpoint": map[string]any{"base_url": baseURL},
			"models":   declared,
		},
		"profiles": []map[string]any{
			{"secrets": map[string]any{"api_key": apiKey}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("host: encode the provider: %w", err)
	}
	definition, err := openai.Factory().New(ctx, resource.NewInput(json.RawMessage(settings), nil, nil, nil))
	if err != nil {
		return nil, fmt.Errorf("host: build the provider: %w", err)
	}
	assembly, err := inference.Factory{}.New(ctx, resource.NewInput(nil,
		map[string]any{"provider.openai": definition}, nil, nil))
	if err != nil {
		return nil, fmt.Errorf("host: build the inference assembly: %w", err)
	}
	return assembly, nil
}
