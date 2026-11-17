package config

import (
	"context"
	"io/fs"
	"os"
	"sort"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Source is where one layer's bytes come from.
//
// It is the only reason a layer needs to know anything about the world outside
// this package: a file on disk, a byte slice the binary carries, a filesystem
// (which is what makes //go:embed work), or a function for a document that has
// to be fetched, decrypted or generated.
type Source interface {
	// Body returns the layer's bytes. It is called once per load; a view that
	// wants the bytes twice caches them itself.
	Body(ctx context.Context) ([]byte, error)
}

// File reads a layer from a path on this host's filesystem.
func File(path string) Source { return fileSource{path: path} }

type fileSource struct{ path string }

func (s fileSource) Body(context.Context) ([]byte, error) { return os.ReadFile(s.path) }

// Bytes serves a layer from memory: an embedded constant, a fixture, a document
// a caller assembled.
func Bytes(body []byte) Source { return bytesSource(body) }

type bytesSource []byte

func (s bytesSource) Body(context.Context) ([]byte, error) { return []byte(s), nil }

// FS reads a layer from any io/fs filesystem. embed.FS is the common case
// (`config.FS(assets, "base.yaml")`), os.DirFS and fs.Sub cover the others.
func FS(fsys fs.FS, name string) Source { return fsSource{fsys: fsys, name: name} }

type fsSource struct {
	fsys fs.FS
	name string
}

func (s fsSource) Body(context.Context) ([]byte, error) { return fs.ReadFile(s.fsys, s.name) }

// Func serves a layer from a function, for a body that has to be fetched,
// decrypted, templated or generated at load time. The context is the load's, so
// a source that talks to a network or a vault can be cancelled.
func Func(load func(ctx context.Context) ([]byte, error)) Source { return funcSource(load) }

type funcSource func(ctx context.Context) ([]byte, error)

func (s funcSource) Body(ctx context.Context) ([]byte, error) {
	if s == nil {
		return nil, errdefs.Validationf("source: nil function")
	}
	return s(ctx)
}

// Layer is one configuration layer. Layers merge in ascending priority, so a
// higher priority layer overrides a lower one.
//
// Priority is explicit rather than implied by order because the order a host
// receives layers in is not always the order it wants them applied: a profile
// picked on the command line belongs above a file listed in a directory scan,
// whatever the scan produced. Duplicate priorities are rejected instead of
// being broken by a tie-break nobody can predict.
type Layer struct {
	// Priority orders the layer; higher wins.
	Priority int
	// Name identifies the layer in errors and provenance. It is not a path:
	// an embedded or generated layer has no path, and one written to a
	// temporary file should still be reported under the name its author knows.
	Name string
	// Source is where the bytes come from.
	Source Source
}

// Ref identifies the layer that supplied a merged value.
type Ref struct {
	Priority int
	Name     string
}

// body reads the layer, naming the layer in every error: "read failed" without
// the layer that failed sends the reader to the wrong file.
func (l Layer) body(ctx context.Context, label string) ([]byte, error) {
	if l.Source == nil {
		return nil, errdefs.Validationf("%s: layer %q has no source", label, l.Name)
	}
	body, err := l.Source.Body(ctx)
	if err != nil {
		return nil, errdefs.Validationf("%s: layer %q: %w", label, l.Name, err)
	}
	return body, nil
}

// orderedLayers sorts the stack by priority and refuses a duplicate.
func orderedLayers(layers []Layer, label string) ([]Layer, error) {
	ordered := append([]Layer(nil), layers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority < ordered[j].Priority
	})
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Priority == ordered[i-1].Priority {
			return nil, errdefs.Validationf(
				"%s: layers %q and %q share priority %d",
				label, ordered[i-1].Name, ordered[i].Name, ordered[i].Priority)
		}
	}
	return ordered, nil
}
