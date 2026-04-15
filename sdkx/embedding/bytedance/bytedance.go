package bytedance

import (
	"context"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/embedding"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

const defaultModel = "doubao-embedding-large"

func init() {
	embedding.RegisterProvider("bytedance", func(modelName string, config map[string]any) (embedding.Embedder, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		region, _ := config["region"].(string)
		return New(apiKey, modelName, baseURL, region)
	})
}

// Embedder implements embedding.Embedder using the Volcengine ArkRuntime SDK.
type Embedder struct {
	client *arkruntime.Client
	model  string
}

var _ embedding.Embedder = (*Embedder)(nil)

// New creates a ByteDance embedding instance.
// Returns an error if apiKey is empty.
func New(apiKey, modelName, baseURL, region string) (*Embedder, error) {
	if apiKey == "" {
		return nil, embedding.ErrMissingCredentials
	}
	if modelName == "" {
		modelName = defaultModel
	}

	var opts []arkruntime.ConfigOption
	if region != "" {
		opts = append(opts, arkruntime.WithRegion(region))
	}
	if baseURL != "" {
		opts = append(opts, arkruntime.WithBaseUrl(baseURL))
	}
	opts = append(opts, arkruntime.WithHTTPClient(http.DefaultClient))

	client := arkruntime.NewClientWithApiKey(apiKey, opts...)
	return &Embedder{client: client, model: modelName}, nil
}

func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.client.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: []string{text},
		Model: e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("bytedance embedding: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("bytedance embedding: empty response for model %s", e.model)
	}
	return resp.Data[0].Embedding, nil
}

func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	input := make([]string, len(texts))
	copy(input, texts)

	resp, err := e.client.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: input,
		Model: e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("bytedance embedding: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("bytedance embedding: expected %d results, got %d", len(texts), len(resp.Data))
	}
	result := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		result[i] = d.Embedding
	}
	return result, nil
}
