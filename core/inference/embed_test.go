package inference_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func embedRequest(docs ...string) inference.EmbedRequest {
	input := make([]message.Content, 0, len(docs))
	for _, doc := range docs {
		input = append(input, message.NewTextContent(doc))
	}
	return inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "p", Name: "embed"}},
		Input: input,
	}
}

// A provider that ignores Dimensions returns a well-formed answer of the wrong
// shape; the contract refuses it rather than letting the caller store vectors it
// cannot query.
func TestEmbedResponseValidateFor(t *testing.T) {
	request := embedRequest("a", "b")

	ok := inference.EmbedResponse{Vectors: [][]float32{{1, 2}, {3, 4}}}
	if err := ok.ValidateFor(request); err != nil {
		t.Fatalf("valid answer rejected: %v", err)
	}

	cases := map[string]inference.EmbedResponse{
		"count":        {Vectors: [][]float32{{1, 2}}},
		"empty vector": {Vectors: [][]float32{{}, {1, 2}}},
		"width":        {Vectors: [][]float32{{1, 2}, {3}}},
	}
	for name, response := range cases {
		if err := response.ValidateFor(request); err == nil {
			t.Errorf("%s: ValidateFor = nil, want a rejection", name)
		}
	}

	sized := request
	sized.Dimensions = 3
	if err := ok.ValidateFor(sized); err == nil {
		t.Error("requested width ignored, want a rejection")
	}
}

func TestEmbedRequestValidate(t *testing.T) {
	if err := (inference.EmbedRequest{}).Validate(); err == nil {
		t.Error("empty request accepted")
	}
	request := embedRequest("a")
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	negative := request
	negative.Dimensions = -1
	if err := negative.Validate(); err == nil {
		t.Error("negative dimensions accepted")
	}
}

func TestEmbedRequestActiveFields(t *testing.T) {
	fields := embedRequest("a").ActiveFields()
	if len(fields) != 2 || fields[0] != inference.FieldModel || fields[1] != inference.FieldInput {
		t.Fatalf("ActiveFields = %v, want [model input]", fields)
	}
	request := embedRequest("a")
	request.Dimensions = 64
	found := false
	for _, field := range request.ActiveFields() {
		if field == inference.FieldDimensions {
			found = true
		}
	}
	if !found {
		t.Errorf("ActiveFields misses dimensions: %v", request.ActiveFields())
	}
}
