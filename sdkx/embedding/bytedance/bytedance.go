package bytedance

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

const defaultModel = "doubao-embedding-vision-251215"

func init() {
	embedding.RegisterProvider("bytedance", func(modelName string, config map[string]any) (embedding.Embedder, error) {
		apiKey, _ := config["api_key"].(string)
		baseURL, _ := config["base_url"].(string)
		region, _ := config["region"].(string)
		return New(apiKey, modelName, baseURL, region)
	})
}

// Embedder implements embedding.Embedder using the Volcengine ArkRuntime SDK.
// It automatically selects the standard or multimodal embedding endpoint
// based on the model name.
type Embedder struct {
	client     *arkruntime.Client
	model      string
	multimodal bool
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
	return &Embedder{
		client:     client,
		model:      modelName,
		multimodal: strings.Contains(modelName, "vision"),
	}, nil
}

func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.multimodal {
		return e.embedMultimodal(ctx, text)
	}
	return e.embedStandard(ctx, text)
}

func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.multimodal {
		return e.embedBatchMultimodal(ctx, texts)
	}
	return e.embedBatchStandard(ctx, texts)
}

// --- standard text embedding endpoint ---

func (e *Embedder) embedStandard(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.client.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: []string{text},
		Model: e.model,
	})
	if err != nil {
		return nil, errdefs.ClassifyProviderError("bytedance", err)
	}
	if len(resp.Data) == 0 {
		return nil, errdefs.NotAvailablef("bytedance embedding: empty response for model %s", e.model)
	}
	return resp.Data[0].Embedding, nil
}

func (e *Embedder) embedBatchStandard(ctx context.Context, texts []string) ([][]float32, error) {
	input := make([]string, len(texts))
	copy(input, texts)

	resp, err := e.client.CreateEmbeddings(ctx, model.EmbeddingRequestStrings{
		Input: input,
		Model: e.model,
	})
	if err != nil {
		return nil, errdefs.ClassifyProviderError("bytedance", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, errdefs.NotAvailablef("bytedance embedding: expected %d results, got %d", len(texts), len(resp.Data))
	}
	result := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		result[i] = d.Embedding
	}
	return result, nil
}

// --- multimodal embedding endpoint (for vision models) ---

func (e *Embedder) embedMultimodal(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.client.CreateMultiModalEmbeddings(ctx, model.MultiModalEmbeddingRequest{
		Model: e.model,
		Input: []model.MultimodalEmbeddingInput{
			{Type: model.MultiModalEmbeddingInputTypeText, Text: &text},
		},
	})
	if err != nil {
		return nil, errdefs.ClassifyProviderError("bytedance", err)
	}
	if len(resp.Data.Embedding) == 0 {
		return nil, errdefs.NotAvailablef("bytedance embedding: empty response for model %s", e.model)
	}
	return resp.Data.Embedding, nil
}

// embedBatchMultimodal calls the multimodal endpoint once per text, since
// that endpoint merges all inputs into a single embedding vector.
func (e *Embedder) embedBatchMultimodal(ctx context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := e.embedMultimodal(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("bytedance embedding batch[%d]: %w", i, err)
		}
		result[i] = vec
	}
	return result, nil
}
