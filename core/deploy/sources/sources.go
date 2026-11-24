// Package sources is where a deployment document's bytes come from.
//
// It is a small package on purpose. Four constructors and nothing else: the loader takes
// layers, and a layer is bytes with a name. Everything a host might want beyond these —
// HTTP, a vault, a row in a table, a document generated at startup — is [Func] with five
// lines of the host's own policy. TLS, retries, size limits and credentials are decisions a
// platform should not make on a deployment's behalf.
//
// # Why the functions take a name
//
// A layer's name is what an error and a provenance entry report, and it is not the same
// thing as its address. An embedded layer has a path inside the FS that means nothing
// outside it; a generated one has no address at all. Naming the layer separately, as the
// author knows it, is what makes "runtime.yaml, line 12" readable for a document that was
// never a file.
package sources

import (
	"context"
	"io/fs"

	"github.com/GizClaw/flowcraft/core/config"
)

// File reads a layer from a path on this host's filesystem.
func File(path string) config.Layer {
	return config.Layer{Priority: 0, Name: path, Source: config.File(path)}
}

// Bytes serves a layer from memory: an embedded constant, a test fixture, a document a host
// assembled from its own store.
func Bytes(name string, data []byte) config.Layer {
	return config.Layer{Priority: 0, Name: name, Source: config.Bytes(data)}
}

// FS reads a layer from any io/fs filesystem. embed.FS is the common case — the default
// configuration a binary carries — and os.DirFS and fs.Sub cover the others.
func FS(fsys fs.FS, name string) config.Layer {
	return config.Layer{Priority: 0, Name: name, Source: config.FS(fsys, name)}
}

// Func reads a layer through a function, for a document that has to be fetched, decrypted or
// generated. It is the seam a host uses for anything the three above cannot express.
func Func(name string, load func(ctx context.Context) ([]byte, error)) config.Layer {
	return config.Layer{Priority: 0, Name: name, Source: config.Func(load)}
}

// Over sets the layer's priority: higher wins, which is what makes an overlay written by an
// operator take precedence over the default the binary carries.
func Over(layer config.Layer, priority int) config.Layer {
	layer.Priority = priority
	return layer
}
