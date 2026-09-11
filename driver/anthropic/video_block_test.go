package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// declaredVideoEntry builds a one-model declared catalog, optionally opening
// the endpoint's video extension. The model always declares video input, so
// the two gates can be toggled independently.
func declaredVideoEntry(t *testing.T, wireVideo bool) catalogEntry {
	t.Helper()
	raw := `{"catalog":"declared","models":[{"name":"m",` +
		`"capabilities":{"inputs":["text","video"],"outputs":["text"]}}]}`
	if wireVideo {
		raw = `{"wire":{"video_input":true},"catalog":"declared",` +
			`"models":[{"name":"m",` +
			`"capabilities":{"inputs":["text","video"],"outputs":["text"]}}]}`
	}
	spec, err := decodeSpec(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	return models["m"]
}

// assertRejected finds the video decision and checks the reason names the
// gate that closed; the ledger, not the error string, is the contract.
func assertRejected(t *testing.T, report inference.CompileReport, want string) {
	t.Helper()
	for _, decision := range report.Decisions {
		if decision.Field != inference.FieldGenerateInputVideo {
			continue
		}
		if decision.Disposition != inference.Rejected ||
			!strings.Contains(decision.Reason, want) {
			t.Fatalf("decision = %+v, want a rejection naming %q", decision, want)
		}
		return
	}
	t.Fatalf("no video decision in %+v", report.Decisions)
}

func videoRequest(t *testing.T) inference.GenerateRequest {
	t.Helper()
	source, err := media.NewVideoBytes([]byte("clip-bytes"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		message.VideoPart{Source: source},
	)
	return request
}

// TestVideoBlockNeedsBothGates locks the double gate: the endpoint must open
// spec.wire.video_input and the model must declare video input, because the
// block is a compatible-endpoint extension rather than part of the Messages
// schema Anthropic itself serves.
func TestVideoBlockNeedsBothGates(t *testing.T) {
	request := videoRequest(t)

	if _, err := compileGenerate("m", declaredVideoEntry(t, true))(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	); err != nil {
		t.Fatalf("both gates open must compile: %v", err)
	}

	compiled, err := compileGenerate("m", declaredVideoEntry(t, false))(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("closed endpoint gate must reject the video part")
	}
	assertRejected(t, compiled.Report, "wire.video_input")
}

// TestVideoBlockNeedsModelCapability covers the other half: an endpoint that
// accepts video blocks still must not send one for a model that never
// declared video input.
func TestVideoBlockNeedsModelCapability(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"wire":{"video_input":true},"catalog":"declared",`+
			`"models":[{"name":"m",`+
			`"capabilities":{"inputs":["text"],"outputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	compiled, err := compileGenerate("m", models["m"])(
		context.Background(),
		conformanceModel("m"),
		videoRequest(t),
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("undeclared video capability must reject the part")
	}
	assertRejected(t, compiled.Report, "model does not accept video input")
}
