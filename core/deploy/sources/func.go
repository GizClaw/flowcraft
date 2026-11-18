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

// Func is the source for everything else: a document fetched, decrypted,
// templated or generated. The host supplies the function, the document supplies
// the reference it is called with.
//
//	kinds := deploy.Kinds{}.WithSources(sources.Func("vault", vault.Read))
//
//	source: {kind: vault, settings: {name: openai/luna}}
//
// It is the reason core ships no HTTP, vault or database source. Each of those is
// a policy — TLS, retries, a size limit, credentials — and five lines of the
// host's policy here beats a platform feature that decides them for every
// deployment. The load function receives the document's context, so a fetch can be
// cancelled with the load.
func Func(name string, load func(ctx context.Context, ref string) ([]byte, error)) deploy.SourceKind {
	return deploy.SourceKind{
		Name: name,
		Open: func(_ context.Context, settings json.RawMessage) (deploy.Source, error) {
			if load == nil {
				return deploy.Source{}, errdefs.Validationf(
					"%s: source was registered without a loader", name)
			}
			decoded, err := decode.Decode[funcSettings](settings)
			if err != nil {
				return deploy.Source{}, errdefs.Validationf("%s: settings: %v", name, err)
			}
			if strings.TrimSpace(decoded.Name) == "" {
				return deploy.Source{}, errdefs.Validationf("%s: settings.name is required", name)
			}
			// The reference is bound here; the context comes from the load, so a
			// fetch is cancelled with the document that asked for it.
			ref := decoded.Name
			return deploy.Source{
				Name: name + ":" + ref,
				Body: config.Func(func(ctx context.Context) ([]byte, error) {
					return load(ctx, ref)
				}),
			}, nil
		},
	}
}

// funcSettings is a function source's written configuration.
type funcSettings struct {
	// Name is the reference the host's loader is called with.
	Name string `json:"name"`
}
