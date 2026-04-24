package knowledge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

func TestSearchServiceTool_AllScopeDefault(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "go.md", "Go is a compiled programming language"); err != nil {
		t.Fatalf("put: %v", err)
	}
	tool := knowledge.NewSearchServiceTool(svc)
	if got := tool.Definition().Name; got != "knowledge_search" {
		t.Fatalf("tool name = %q, want knowledge_search", got)
	}

	out, err := tool.Execute(ctx, `{"query":"Go programming"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var hits []knowledge.Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected hits, got %s", out)
	}
}

func TestSearchServiceTool_NilService(t *testing.T) {
	tool := knowledge.NewSearchServiceTool(nil)
	if _, err := tool.Execute(context.Background(), `{"query":"x"}`); err == nil {
		t.Fatal("expected error for nil service")
	}
}

func TestSearchServiceTool_SingleScope(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "docs", "go.md", "Go alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}
	tool := knowledge.NewSearchServiceTool(svc)
	out, err := tool.Execute(ctx, `{"query":"alpha","scope":"single","dataset_id":"docs"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out == "[]" {
		t.Fatal("expected hits for single scope, got []")
	}
}

func TestSearchServiceTool_InvalidScope(t *testing.T) {
	svc := newLocalService(t)
	tool := knowledge.NewSearchServiceTool(svc)
	if _, err := tool.Execute(context.Background(), `{"query":"x","scope":"bogus"}`); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPutServiceTool_DefaultDatasetAndVersion(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	tool := knowledge.NewPutServiceTool(svc)
	if got := tool.Definition().Name; got != "knowledge_put" {
		t.Fatalf("tool name = %q, want knowledge_put", got)
	}
	out, err := tool.Execute(ctx, `{"name":"a.md","content":"alpha"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, `"status":"ok"`) {
		t.Fatalf("response missing status: %s", out)
	}
	if !strings.Contains(out, `"version":1`) {
		t.Fatalf("response missing version 1: %s", out)
	}

	out2, err := tool.Execute(ctx, `{"name":"a.md","content":"beta"}`)
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if !strings.Contains(out2, `"version":2`) {
		t.Fatalf("response missing version 2: %s", out2)
	}
}

func TestPutServiceTool_NilService(t *testing.T) {
	tool := knowledge.NewPutServiceTool(nil)
	if _, err := tool.Execute(context.Background(), `{"name":"a.md","content":"x"}`); err == nil {
		t.Fatal("expected error for nil service")
	}
}

func TestPutServiceTool_CustomDataset(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	tool := knowledge.NewPutServiceTool(svc)
	if _, err := tool.Execute(ctx, `{"dataset_id":"recipes","name":"r.md","content":"x"}`); err != nil {
		t.Fatalf("execute: %v", err)
	}
	doc, err := svc.GetDocument(ctx, "recipes", "r.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if doc == nil {
		t.Fatal("expected document in custom dataset")
	}
}
