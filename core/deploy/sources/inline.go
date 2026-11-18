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

// Inline is the source a document carries: the bytes are the settings themselves.
//
// It exists for deployments that are generated, and for tests, where writing a
// file to describe one layer is worse than the layer. A document written for a
// person should use [File]: YAML inside a YAML string is exactly the shape the
// rest of this package avoids, and the one thing an embedded tree ([FS]) does
// better.
//
// The body is the *decoded* document's text, so placeholders in the deploying
// document are already expanded once when it arrives; a body that wants a literal
// "${...}" — an unexpanded reference for the layer the host is generating — must
// escape it as if it were being written one level deeper.
func Inline() deploy.SourceKind {
	return deploy.SourceKind{Name: "inline", Open: openInline}
}

// inlineSettings is the inline source's written configuration.
type inlineSettings struct {
	// Name is what errors call this layer. Empty means "inline", which is
	// unambiguous for one layer and not for two — the loader refuses a stack in
	// which two sources report the same name.
	Name string `json:"name"`
	// Body is the layer itself.
	Body string `json:"body"`
}

func openInline(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
	decoded, err := decode.Decode[inlineSettings](settings)
	if err != nil {
		return deploy.Source{}, errdefs.Validationf("inline: settings: %v", err)
	}
	// An empty body is a layer the merge would skip, so a typo — "bod" is caught
	// by the strict decode, an empty value is not — would look like a deployment
	// that configured nothing.
	if strings.TrimSpace(decoded.Body) == "" {
		return deploy.Source{}, errdefs.Validationf("inline: settings.body is required")
	}
	name := strings.TrimSpace(decoded.Name)
	if name == "" {
		name = "inline"
	}
	return deploy.Source{Name: name, Body: config.Bytes([]byte(decoded.Body))}, nil
}
