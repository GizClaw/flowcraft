package sources

import (
	"context"
	"io/fs"
	"strings"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// FS is the layer source a host reads from an io/fs filesystem: an //go:embed
// tree, an os.DirFS, an fs.Sub of either.
//
// The kind name is the host's, because one process may carry several trees; the
// document names the member:
//
//	//go:embed assets
//	var assets embed.FS
//
//	kinds := deploy.Kinds{}.WithLayerSources(sources.FS("assets", assets))
//
//	source: {kind: assets, settings: {name: runtime/base.yaml}}
//
// The filesystem cannot be written in a document — it is a Go value — which is
// why this is a factory and [File] is a registration.
func FS(name string, fsys fs.FS) deploy.LayerSourceKind {
	return deploy.LayerSourceKind{
		Name: name,
		Open: func(_ context.Context, settings json.RawMessage) (deploy.LayerSource, error) {
			if fsys == nil {
				return deploy.LayerSource{}, errdefs.Validationf(
					"%s: source was registered without a filesystem", name)
			}
			decoded, err := decode.Decode[fsSettings](settings)
			if err != nil {
				return deploy.LayerSource{}, errdefs.Validationf("%s: settings: %v", name, err)
			}
			if strings.TrimSpace(decoded.Name) == "" {
				return deploy.LayerSource{}, errdefs.Validationf("%s: settings.name is required", name)
			}
			return deploy.LayerSource{
				Name: name + ":" + decoded.Name,
				Body: config.FS(fsys, decoded.Name),
			}, nil
		},
	}
}

// fsSettings is a filesystem source's written configuration.
type fsSettings struct {
	// Name is the member inside the host's filesystem.
	Name string `json:"name"`
}
