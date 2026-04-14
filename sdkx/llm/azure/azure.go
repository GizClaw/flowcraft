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

// New creates an Azure OpenAI LLM instance.
func New(model, apiKey, baseURL, apiVersion string) (*openai.LLM, error) {
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	return openai.New(model, "", "", // apiKey/baseURL handled by azure options
		azureClientOptions(apiKey, baseURL, apiVersion)...,
	)
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
