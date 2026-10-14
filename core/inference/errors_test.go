package inference

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestProviderResponseUnusableClassification pins the split between a
// completed response whose payload the request contract rejects (attempt
// void at caller granularity) and wire-level corruption on the decode and
// assembly paths.
func TestProviderResponseUnusableClassification(t *testing.T) {
	request := responseValidationRequest(Intent{Text: &TextIntent{}})
	response := responseValidationResponse(
		message.ReasoningPart{Text: "thinking", Signature: "sig"})
	rejected := newResponseValidationError(
		OperationGenerate, "generate", response.ValidateFor(request))
	if rejected.Kind != ProviderResponseUnusable {
		t.Fatalf("Kind = %q, want %q", rejected.Kind, ProviderResponseUnusable)
	}
	if rejected.Detail != "generate.validation.no_text" {
		t.Fatalf("Detail = %q, want generate.validation.no_text", rejected.Detail)
	}
	if !errdefs.IsNotAvailable(rejected) {
		t.Fatal("a completed-but-unusable response must classify as not available (attempt void)")
	}
	if errdefs.IsInternal(rejected) {
		t.Fatal("a completed-but-unusable response must not classify as internal corruption")
	}
}

// TestCauseMessageExposesRedactedCausesOnly covers the marker boundary: the
// flowcraft-generated reasons come through, driver and transport causes do
// not.
func TestCauseMessageExposesRedactedCausesOnly(t *testing.T) {
	t.Run("terminal stream validation", func(t *testing.T) {
		stream := detailTestStream([]GenerateStreamEvent{
			{PartIndex: 0, Delta: ReasoningDelta{Text: "thinking", Signature: "sig"}},
			{FinishReason: FinishCompleted},
		}, textStreamRequest(t))
		streamErr := nextStreamError(t, stream)
		if got := CauseMessage(streamErr); got != "completed generate response contains no requested text" {
			t.Fatalf("CauseMessage = %q", got)
		}
	})
	t.Run("unary validation", func(t *testing.T) {
		request := responseValidationRequest(Intent{Text: &TextIntent{}})
		rejected := newResponseValidationError(
			OperationGenerate, "generate", responseValidationResponse().ValidateFor(request))
		want := `generate response content is required for finish reason "completed"`
		if got := CauseMessage(rejected); got != want {
			t.Fatalf("CauseMessage = %q, want %q", got, want)
		}
	})
	t.Run("undefined tool", func(t *testing.T) {
		request := responseValidationRequest(Intent{Text: &TextIntent{
			Tools: []message.ToolDefinition{message.DefineSchema("known", "a known tool").Build()},
		}})
		response := responseValidationResponse(responseValidationToolCall("ghost"))
		response.FinishReason = FinishToolCalls
		rejected := newResponseValidationError(
			OperationGenerate, "generate", response.ValidateFor(request))
		want := `generate tool call 0 names undefined tool "ghost"`
		if got := CauseMessage(rejected); got != want {
			t.Fatalf("CauseMessage = %q, want %q", got, want)
		}
	})
	t.Run("overlong tool name is bounded", func(t *testing.T) {
		name := strings.Repeat("S3CRET", 20)
		request := responseValidationRequest(Intent{Text: &TextIntent{
			Tools: []message.ToolDefinition{message.DefineSchema("known", "a known tool").Build()},
		}})
		response := responseValidationResponse(responseValidationToolCall(name))
		response.FinishReason = FinishToolCalls
		rejected := newResponseValidationError(
			OperationGenerate, "generate", response.ValidateFor(request))

		// tokenLimit runes of the name survive, marked as truncated; the rest
		// of the model-supplied text never reaches the log.
		bounded := strings.Repeat("S3CRET", 10) + "S3CR"
		want := "generate tool call 0 names undefined tool " +
			strconv.Quote(bounded+"...")
		got := CauseMessage(rejected)
		if got != want {
			t.Fatalf("CauseMessage = %q, want %q", got, want)
		}
		if strings.Contains(got, name) {
			t.Fatalf("CauseMessage = %q, want the provider name bounded", got)
		}
	})
	t.Run("tool call conflict", func(t *testing.T) {
		err := NewError(InvalidProviderResponse, OperationGenerate, "",
			&toolCallConflictError{err: fmt.Errorf("tool call changed id")})
		if got := CauseMessage(err); got != "tool call changed id" {
			t.Fatalf("CauseMessage = %q", got)
		}
	})
	t.Run("decode failures stay unexposed", func(t *testing.T) {
		err := newStreamError("stream.decode", errors.New("decode boom"))
		if got := CauseMessage(err); got != "" {
			t.Fatalf("CauseMessage(decode) = %q, want empty", got)
		}
	})
	t.Run("no inference error", func(t *testing.T) {
		if got := CauseMessage(errors.New("plain")); got != "" {
			t.Fatalf("CauseMessage(plain) = %q, want empty", got)
		}
		if got := CauseMessage(nil); got != "" {
			t.Fatalf("CauseMessage(nil) = %q, want empty", got)
		}
	})
}

// TestQuoteTokenBoundsProviderText covers the rendering rule that lets
// provider-supplied tool names reach error.cause: quoted, valid UTF-8, and
// capped, however hostile the name is.
func TestQuoteTokenBoundsProviderText(t *testing.T) {
	cases := map[string]struct {
		value string
		want  string
	}{
		"short name unchanged": {value: "ghost", want: `"ghost"`},
		"control characters escaped": {
			value: "a\nb",
			want:  `"a\nb"`,
		},
		"overlong name truncated": {
			value: strings.Repeat("x", tokenLimit+10),
			want:  `"` + strings.Repeat("x", tokenLimit) + `..."`,
		},
		"rune aware truncation keeps valid UTF-8": {
			value: strings.Repeat("\u00e9", tokenLimit+1),
			want:  `"` + strings.Repeat("\u00e9", tokenLimit) + `..."`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := quoteToken(tc.value); got != tc.want {
				t.Fatalf("quoteToken = %q, want %q", got, tc.want)
			}
		})
	}
}
