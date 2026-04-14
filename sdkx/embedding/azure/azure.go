package azure

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	embeddingOpenAI "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	"github.com/openai/openai-go/azure"
	"github.com/openai/openai-go/option"
)

func init() {
	embedding.RegisterProvider("azure", func(model string, config map[string]any) (embedding.Embedder, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		apiVersion, _ := config["api_version"].(string)
		return New(model, apiKey, baseURL, apiVersion)
	})
}

const defaultAPIVersion = "2025-04-01-preview"

// New creates an Azure OpenAI Embedder.
func New(model, apiKey, baseURL, apiVersion string) (*embeddingOpenAI.Embedder, error) {
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	e := embeddingOpenAI.New("", model, azureClientOptions(apiKey, baseURL, apiVersion)...)
	if e == nil {
		return nil, embedding.ErrMissingCredentials
	}
	return e, nil
}

func azureClientOptions(apiKey, endpoint, apiVersion string) []option.RequestOption {
	var opts []option.RequestOption
	if endpoint != "" {
		opts = append(opts, azure.WithEndpoint(endpoint, apiVersion))
	}
	if apiKey != "" {
		opts = append(opts, azure.WithAPIKey(apiKey))
	}
	return opts
}
