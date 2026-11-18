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

// File is the file source: a document read from a path on this host's
// filesystem.
//
// Settings are {path}. A relative path is relative to the process's working
// directory, the rule the graph document already follows.
func File() deploy.SourceKind {
	return deploy.SourceKind{Name: "file", Open: openFile}
}

// fileSettings is the file source's written configuration.
type fileSettings struct {
	Path string `json:"path"`
}

func openFile(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
	decoded, err := decode.Decode[fileSettings](settings)
	if err != nil {
		return deploy.Source{}, errdefs.Validationf("file: settings: %v", err)
	}
	if strings.TrimSpace(decoded.Path) == "" {
		return deploy.Source{}, errdefs.Validationf("file: settings.path is required")
	}
	return deploy.Source{Name: decoded.Path, Body: config.File(decoded.Path)}, nil
}
