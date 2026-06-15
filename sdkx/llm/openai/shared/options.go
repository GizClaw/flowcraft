package shared

import (
	"github.com/GizClaw/flowcraft/sdk/llm"

	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

const (
	DefaultModel        = "gpt-5"
	DefaultProviderName = "openai"
)

func NewClient(apiKey, baseURL string, extraOpts ...option.RequestOption) *oai.Client {
	var clientOpts []option.RequestOption
	if apiKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(baseURL))
	}
	clientOpts = append(clientOpts, extraOpts...)
	client := oai.NewClient(clientOpts...)
	return &client
}

// ExtraRequestOpts converts GenerateOptions.Extra into per-request
// option.WithJSONSet calls, allowing provider-specific body fields through the
// standard Extra mechanism.
func ExtraRequestOpts(opts *llm.GenerateOptions) []option.RequestOption {
	if len(opts.Extra) == 0 {
		return nil
	}
	out := make([]option.RequestOption, 0, len(opts.Extra))
	for k, v := range opts.Extra {
		out = append(out, option.WithJSONSet(k, v))
	}
	return out
}
