package openai

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	oai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

const defaultModel = "text-embedding-3-small"

func init() {
	embedding.RegisterProvider("openai", func(model string, config map[string]any) (embedding.Embedder, error) {
		apiKey, _ := config["api_key"].(string)
		e := New(apiKey, model)
		if e == nil {
			return nil, embedding.ErrMissingCredentials
		}
		return e, nil
	})
}

// Embedder implements embedding.Embedder using the OpenAI embeddings API.
type Embedder struct {
	client *oai.Client
	model  string
}

var _ embedding.Embedder = (*Embedder)(nil)

// New returns an Embedder backed by OpenAI. Returns nil if apiKey is empty.
// Extra opts are passed directly to the openai-go client constructor
// (used by the Azure variant to inject endpoint/auth options).
func New(apiKey, model string, opts ...option.RequestOption) *Embedder {
	if apiKey == "" && len(opts) == 0 {
		return nil
	}
	if model == "" {
		model = defaultModel
	}
	all := make([]option.RequestOption, 0, 1+len(opts))
	if apiKey != "" {
		all = append(all, option.WithAPIKey(apiKey))
	}
	all = append(all, opts...)
	c := oai.NewClient(all...)
	return &Embedder{client: &c, model: model}
}

func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.client.Embeddings.New(ctx, oai.EmbeddingNewParams{
		Input: oai.EmbeddingNewParamsInputUnion{OfString: oai.String(text)},
		Model: oai.EmbeddingModel(e.model),
	})
	if err != nil {
		return nil, errdefs.ClassifyProviderError("openai", err)
	}
	if len(resp.Data) == 0 {
		return nil, errdefs.NotAvailablef("openai embedding: empty response for model %s", e.model)
	}
	return toFloat32(resp.Data[0].Embedding), nil
}

func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	strs := make([]string, len(texts))
	copy(strs, texts)

	resp, err := e.client.Embeddings.New(ctx, oai.EmbeddingNewParams{
		Input: oai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: strs},
		Model: oai.EmbeddingModel(e.model),
	})
	if err != nil {
		return nil, errdefs.ClassifyProviderError("openai", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, errdefs.NotAvailablef("openai embeddings: expected %d results, got %d", len(texts), len(resp.Data))
	}
	result := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		result[i] = toFloat32(d.Embedding)
	}
	return result, nil
}

func toFloat32(in []float64) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = float32(v)
	}
	return out
}
