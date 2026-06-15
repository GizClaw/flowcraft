package azure

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/llm/openai"
	"github.com/openai/openai-go/azure"
	"github.com/openai/openai-go/option"
)

func init() {
	llm.RegisterProvider("azure", func(model string, config map[string]any) (llm.LLM, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		apiVersion, _ := config["api_version"].(string)
		return New(model, apiKey, baseURL, apiVersion)
	})
}

const defaultAPIVersion = "2025-04-01-preview"

// New creates an Azure OpenAI Chat Completions LLM instance.
func New(model, apiKey, baseURL, apiVersion string) (*openai.ChatLLM, error) {
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	inner, err := openai.NewChat(model, "", "", // apiKey/baseURL handled by azure options
		azureClientOptions(apiKey, baseURL, apiVersion)...,
	)
	if err != nil {
		return nil, err
	}
	// Tag the OTel/metrics provider as "azure" so dashboards split out
	// Azure-routed traffic (different region, capacity, billing) from
	// the direct openai.com endpoint. See sdkx/llm/openai/openai.go ▸
	// WithProviderName.
	return inner.WithProviderName("azure"), nil
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
