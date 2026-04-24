package recall_test

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

type scriptedExtractor struct {
	facts       [][]recall.ExtractedFact
	err         error
	calls       int
	gotExisting [][]string
}

func (e *scriptedExtractor) Extract(_ context.Context, _ recall.Scope, _ []llm.Message, opts ...recall.ExtractOption) ([]recall.ExtractedFact, error) {
	var o recall.ExtractOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	e.gotExisting = append(e.gotExisting, append([]string(nil), o.ExistingFacts...))
	if e.err != nil {
		return nil, e.err
	}
	i := e.calls
	e.calls++
	if i >= len(e.facts) {
		return nil, nil
	}
	return append([]recall.ExtractedFact(nil), e.facts[i]...), nil
}

type mapEmbedder struct {
	vectors map[string][]float32
}

var _ embedding.Embedder = (*mapEmbedder)(nil)

func (e *mapEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vectors[text]; ok {
		return append([]float32(nil), v...), nil
	}
	return []float32{0, 0}, nil
}

func (e *mapEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, err := e.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

type failingStage struct {
	name string
	err  error
}

func (s failingStage) Name() string { return s.name }
func (s failingStage) Run(context.Context, *pipeline.State) error {
	return s.err
}
