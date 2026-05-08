package knowledge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkknowledge "github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	tool "github.com/GizClaw/flowcraft/sdkx/tool/knowledge"
)

func newLocalService(t *testing.T) *sdkknowledge.Service {
	t.Helper()
	return factory.NewLocal(workspace.NewMemWorkspace())
}

func TestSearchServiceTool_BasicSearch(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "go.md", "Go is a compiled programming language"); err != nil {
		t.Fatalf("put: %v", err)
	}
	x := tool.NewSearchServiceTool(svc)
	if got := x.Definition().Name; got != "knowledge_search" {
		t.Fatalf("tool name = %q, want knowledge_search", got)
	}

	out, err := x.Execute(ctx, `{"query":"Go programming"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var hits []sdkknowledge.Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("unmarshal hits: %v (raw=%s)", err, out)
	}
}

func TestSearchServiceTool_NilService(t *testing.T) {
	x := tool.NewSearchServiceTool(nil)
	_, err := x.Execute(context.Background(), `{"query":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("nil service: want NotAvailable, got %v", err)
	}
}

func TestSearchServiceTool_InvalidScope(t *testing.T) {
	svc := newLocalService(t)
	x := tool.NewSearchServiceTool(svc)
	_, err := x.Execute(context.Background(), `{"query":"q","scope":"weird"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid scope") {
		t.Fatalf("invalid scope: want validation error, got %v", err)
	}
}

func TestPutServiceTool_RoundTrip(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	x := tool.NewPutServiceTool(svc)
	if got := x.Definition().Name; got != "knowledge_put" {
		t.Fatalf("tool name = %q, want knowledge_put", got)
	}

	out, err := x.Execute(ctx, `{"name":"a.md","content":"alpha"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp struct {
		Status    string `json:"status"`
		DatasetID string `json:"dataset_id"`
		Name      string `json:"name"`
		Version   uint64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, out)
	}
	if resp.Status != "ok" || resp.DatasetID != "default" || resp.Name != "a.md" {
		t.Errorf("response shape: %+v", resp)
	}
	if resp.Version == 0 {
		t.Errorf("version should be > 0 after put, got 0")
	}
}

func TestPutServiceTool_NilService(t *testing.T) {
	x := tool.NewPutServiceTool(nil)
	_, err := x.Execute(context.Background(), `{"name":"x","content":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("nil service: want NotAvailable, got %v", err)
	}
}
