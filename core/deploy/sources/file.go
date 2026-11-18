package sources

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// File is the file layer source: a document read from a path on this host's
// filesystem.
//
// Settings are {path}. A relative path is relative to the process's working
// directory, the rule the graph document already follows.
func File() deploy.LayerSourceKind {
	return deploy.LayerSourceKind{Name: "file", Open: openFile}
}

// fileSettings is the file source's written configuration.
type fileSettings struct {
	Path string `json:"path"`
}

func openFile(_ context.Context, settings json.RawMessage) (deploy.LayerSource, error) {
	decoded, err := decode.Decode[fileSettings](settings)
	if err != nil {
		return deploy.LayerSource{}, errdefs.Validationf("file: settings: %v", err)
	}
	if strings.TrimSpace(decoded.Path) == "" {
		return deploy.LayerSource{}, errdefs.Validationf("file: settings.path is required")
	}
	return deploy.LayerSource{Name: decoded.Path, Body: config.File(decoded.Path)}, nil
}
