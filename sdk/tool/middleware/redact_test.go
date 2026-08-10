package middleware

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func TestRedact_RedactsResultKeepsArguments(t *testing.T) {
	var sawArgs json.RawMessage
	reg := catalogWith(tool.FuncTool(message.Definition{Name: "secret"},
		func(_ context.Context, args string) (string, error) {
			sawArgs = json.RawMessage(args)
			return "token=abc123 and keep=ok", nil
		}))
	exec := tool.NewExecutor(reg, Redact(RedactRule{
		Pattern: regexp.MustCompile(`abc123`),
	}))

	res := exec.Execute(context.Background(), message.Call{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if strings.Contains(res.Content, "abc123") {
		t.Errorf("result still contains secret: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED]") {
		t.Errorf("result has no redaction marker: %q", res.Content)
	}
	if got := string(sawArgs); !strings.Contains(got, "abc123") {
		t.Errorf("tool should receive original arguments for execution, got %s", got)
	}
}

func TestRedact_CustomReplacement(t *testing.T) {
	reg := catalogWith(tool.FuncTool(message.Definition{Name: "card"},
		func(_ context.Context, _ string) (string, error) {
			return "card 4111-1111-1111-1111 ok", nil
		}))
	exec := tool.NewExecutor(reg, Redact(RedactRule{
		Pattern:     regexp.MustCompile(`\d{4}-\d{4}-\d{4}-\d{4}`),
		Replacement: "<card>",
	}))
	res := exec.Execute(context.Background(), call("card"))
	if strings.Contains(res.Content, "4111") {
		t.Errorf("content still contains card number: %q", res.Content)
	}
	if !strings.Contains(res.Content, "<card>") {
		t.Errorf("content has no custom replacement: %q", res.Content)
	}
}

func TestRedact_WithAuditRedactedRecord(t *testing.T) {
	var mu sync.Mutex
	var recorded []AuditRecord
	sink := AuditSinkFunc(func(_ context.Context, rec AuditRecord) {
		mu.Lock()
		recorded = append(recorded, rec)
		mu.Unlock()
	})
	reg := catalogWith(tool.FuncTool(message.Definition{Name: "secret"},
		func(_ context.Context, args string) (string, error) {
			return "token=abc123", nil
		}))
	exec := tool.NewExecutor(reg,
		Redact(RedactRule{Pattern: regexp.MustCompile(`abc123`)}),
		AuditRedacted(sink, RedactRule{Pattern: regexp.MustCompile(`abc123`)}),
	)

	call := message.Call{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	}
	if res := exec.Execute(context.Background(), call); res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("audit records = %d, want 1", len(recorded))
	}
	rec := recorded[0]
	if strings.Contains(string(rec.Call.Arguments), "abc123") {
		t.Errorf("audit call arguments are not redacted: %s", rec.Call.Arguments)
	}
	if strings.Contains(rec.Result.Content, "abc123") {
		t.Errorf("audit result content is not redacted: %q", rec.Result.Content)
	}
}

func TestAuditRedacted_ModelSeesOriginalAuditRedacted(t *testing.T) {
	var recorded []AuditRecord
	sink := AuditSinkFunc(func(_ context.Context, rec AuditRecord) {
		recorded = append(recorded, rec)
	})
	reg := catalogWith(tool.FuncTool(message.Definition{Name: "secret"},
		func(_ context.Context, _ string) (string, error) {
			return "token=abc123", nil
		}))
	exec := tool.NewExecutor(reg,
		AuditRedacted(sink, RedactRule{
			Pattern: regexp.MustCompile(`abc123`),
		}),
	)

	res := exec.Execute(context.Background(), message.Call{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	})
	if !strings.Contains(res.Content, "abc123") {
		t.Errorf("model-facing content was redacted, want original: %q", res.Content)
	}
	if len(recorded) != 1 {
		t.Fatalf("audit records = %d, want 1", len(recorded))
	}
	if strings.Contains(recorded[0].Result.Content, "abc123") {
		t.Errorf("audit content is not redacted: %q", recorded[0].Result.Content)
	}
	if strings.Contains(string(recorded[0].Call.Arguments), "abc123") {
		t.Errorf("audit arguments are not redacted: %s", recorded[0].Call.Arguments)
	}
}

func TestRedact_NilPatternPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil pattern")
		}
	}()
	Redact(RedactRule{Replacement: "x"})
}
