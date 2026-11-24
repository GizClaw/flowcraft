package sources_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
)

// TestEverySourceFeedsTheLoader is the package's whole contract: a layer is bytes with a
// name, and the four constructors only differ in where the bytes come from.
func TestEverySourceFeedsTheLoader(t *testing.T) {
	loader := deploy.NewLoader(deploy.NewImpls())

	// A minimal but complete process document: observability is required, so a bare version is
	// not a document the loader accepts — that rule is the loader's, not this package's.
	document := []byte("version: 1\nobservability: { trace: none, bodies: false }\n")
	fsys := fstest.MapFS{"base.yaml": &fstest.MapFile{Data: document}}
	for name, layer := range map[string]config.Layer{
		"bytes": sources.Bytes("base.yaml", document),
		"fs":    sources.FS(fsys, "base.yaml"),
		"func": sources.Func("base.yaml", func(context.Context) ([]byte, error) {
			return document, nil
		}),
	} {
		if _, err := loader.Process(context.Background(), layer); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestFuncSurfacesItsError keeps the seam honest: a host loader that fails must fail the
// deployment, not produce an empty document.
func TestFuncSurfacesItsError(t *testing.T) {
	boom := errors.New("vault unreachable")
	layer := sources.Func("runtime.yaml", func(context.Context) ([]byte, error) { return nil, boom })

	loader := deploy.NewLoader(deploy.NewImpls())
	_, err := loader.Process(context.Background(), layer)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the loader's own error", err)
	}
}

// TestOverRaisesPriority is how an operator's overlay beats the default a binary carries.
func TestOverRaisesPriority(t *testing.T) {
	layer := sources.Over(sources.Bytes("prod.yaml", []byte("version: 1\n")), 10)
	if layer.Priority != 10 {
		t.Fatalf("priority = %d, want 10", layer.Priority)
	}
}
