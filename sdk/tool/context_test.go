package tool

import (
	"context"
	"testing"
)

func TestCatalogContext_RoundTrip(t *testing.T) {
	reg := NewRegistry()
	ctx := WithCatalogOnContext(context.Background(), reg)
	got, ok := CatalogFromContext(ctx)
	if !ok {
		t.Fatal("catalog missing from context")
	}
	if got != Catalog(reg) {
		t.Errorf("catalog = %T, want the registry", got)
	}
}

func TestCatalogContext_AbsentAndNil(t *testing.T) {
	if _, ok := CatalogFromContext(context.Background()); ok {
		t.Error("empty context reported a catalog")
	}
	ctx := WithCatalogOnContext(context.Background(), nil)
	if _, ok := CatalogFromContext(ctx); ok {
		t.Error("typed nil catalog reported as present")
	}
}
