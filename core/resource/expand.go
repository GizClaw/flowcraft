package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Scheme resolves one ${scheme:path} reference kind. Schemes are the
// extension point for new settings reference sources: implement Scheme,
// build a [ReferenceResolver], and enable it with [WithResolver] (or the
// deployment-level WithResolver on core/deploy.Builder).
//
// Resolve returns the materialized value. A plain string replaces the
// reference inline; any other JSON-serializable value (e.g. the
// {"store":..., "name":...} marker produced for lazy secret refs) is
// spliced into the settings tree in its place. The path is opaque to
// the parser and scheme-interpreted.
type Scheme interface {
	// Name is the scheme prefix inside ${scheme:path}.
	Name() string
	// Resolve materializes r. The standard escape (a backslash before
	// "${") is handled by the expansion wrapper before Resolve runs
	// unless the scheme opts in via [EscapedResolver].
	Resolve(ctx context.Context, r Reference) (any, error)
}

// EscapedResolver is optionally implemented by schemes that handle
// escaped references themselves (e.g. deferred namespaces that keep
// their backslash for a later phase). Schemes without it get the
// standard escape: the literal "${...}" text with the backslash
// dropped.
type EscapedResolver interface {
	ResolveEscaped(ctx context.Context, r Reference) (any, error)
}

// SchemeFunc adapts a function to [Scheme].
type SchemeFunc struct {
	SchemeName string
	Fn         func(context.Context, Reference) (any, error)
}

func (s SchemeFunc) Name() string { return s.SchemeName }
func (s SchemeFunc) Resolve(ctx context.Context, r Reference) (any, error) {
	return s.Fn(ctx, r)
}

// ReferenceResolver resolves ${scheme:path} references by scheme name.
// Unknown or disabled schemes are validation errors; see [Expand].
type ReferenceResolver struct {
	schemes map[string]Scheme
}

// NewResolver returns a resolver holding the given schemes.
func NewResolver(schemes ...Scheme) *ReferenceResolver {
	r := &ReferenceResolver{schemes: make(map[string]Scheme, len(schemes))}
	for _, s := range schemes {
		if s != nil && s.Name() != "" {
			r.schemes[s.Name()] = s
		}
	}
	return r
}

// Resolve dispatches one reference to its scheme.
func (r *ReferenceResolver) Resolve(ctx context.Context, ref Reference) (any, error) {
	if ref.Unterminated {
		if ref.Escaped {
			return nil, errdefs.Validationf(
				"resource settings expand: unterminated escaped reference in %q", ref.Raw)
		}
		return nil, errdefs.Validationf(
			"resource settings expand: unterminated reference in %q", ref.Raw)
	}
	if r == nil {
		return nil, errdefs.Validationf(
			"resource settings expand: reference scheme %q is not enabled", ref.Scheme)
	}
	s, ok := r.schemes[ref.Scheme]
	if !ok {
		message := fmt.Sprintf(
			"resource settings expand: reference scheme %q is not enabled", ref.Scheme)
		if dot := strings.IndexByte(ref.Scheme, '.'); dot > 0 {
			message += fmt.Sprintf(
				" (did you mean ${%s:...}?)", ref.Scheme[:dot])
		}
		return nil, errdefs.Validationf("%s", message)
	}
	if ref.Escaped {
		if es, ok := s.(EscapedResolver); ok {
			return es.ResolveEscaped(ctx, ref)
		}
		return ref.Literal(), nil
	}
	return s.Resolve(ctx, ref)
}

// WithScheme returns a resolver carrying every scheme of r plus s.
// s replaces a same-named scheme already present.
func (r *ReferenceResolver) WithScheme(s Scheme) *ReferenceResolver {
	merged := NewResolver()
	if r != nil {
		for name, existing := range r.schemes {
			merged.schemes[name] = existing
		}
	}
	if s != nil && s.Name() != "" {
		merged.schemes[s.Name()] = s
	}
	return merged
}

// Merge returns a resolver carrying every scheme of r overlaid by
// other's schemes (other wins on name collisions). Neither receiver is
// mutated.
func (r *ReferenceResolver) Merge(other *ReferenceResolver) *ReferenceResolver {
	merged := NewResolver()
	if r != nil {
		for name, existing := range r.schemes {
			merged.schemes[name] = existing
		}
	}
	if other != nil {
		for name, overlay := range other.schemes {
			merged.schemes[name] = overlay
		}
	}
	return merged
}

// EnvScheme resolves ${env:NAME} through lookup. A missing variable is
// an error.
func EnvScheme(lookup func(string) (string, bool)) Scheme {
	return SchemeFunc{
		SchemeName: "env",
		Fn: func(_ context.Context, r Reference) (any, error) {
			name := strings.TrimSpace(r.Path)
			value, ok := lookup(name)
			if !ok {
				return "", errdefs.Validationf(
					"resource settings expand: env %q is not set", name)
			}
			return value, nil
		},
	}
}

// BaseScheme resolves ${base} and ${base:rel} rooted at dir (paths
// relative to the deployment document). An empty dir is an error: a
// deployment with no document directory cannot resolve base refs.
func BaseScheme(dir string) Scheme {
	return SchemeFunc{
		SchemeName: "base",
		Fn: func(_ context.Context, r Reference) (any, error) {
			if dir == "" {
				return "", errdefs.Validationf(
					"resource settings expand: base reference requires a base directory")
			}
			if r.Path == "" {
				return dir, nil
			}
			return filepath.Join(dir, r.Path), nil
		},
	}
}

// HomeScheme resolves "~", "~/...", ${home}, and ${home:rel} rooted at
// the current user's home directory.
func HomeScheme() Scheme {
	return SchemeFunc{
		SchemeName: "home",
		Fn: func(_ context.Context, r Reference) (any, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", errdefs.Validationf(
					"resource settings expand: home: %v", err)
			}
			if r.Path == "" {
				return home, nil
			}
			return filepath.Join(home, r.Path), nil
		},
	}
}

// expandConfig carries the enabled reference schemes.
type expandConfig struct {
	resolver *ReferenceResolver
}

func (c *expandConfig) add(schemes ...Scheme) {
	if c.resolver == nil {
		c.resolver = NewResolver()
	}
	for _, s := range schemes {
		if s != nil && s.Name() != "" {
			c.resolver.schemes[s.Name()] = s
		}
	}
}

func (c *expandConfig) hasScheme(name string) bool {
	return c != nil && c.resolver != nil && c.resolver.schemes[name] != nil
}

type configResolver struct{ cfg *expandConfig }

func (r configResolver) Resolve(ctx context.Context, ref Reference) (any, error) {
	return r.cfg.resolver.Resolve(ctx, ref)
}

// ExpandOption configures scalar settings expansion.
type ExpandOption func(*expandConfig)

// WithResolver enables every scheme carried by r for expansion.
func WithResolver(r *ReferenceResolver) ExpandOption {
	return func(c *expandConfig) {
		if r != nil {
			for _, s := range r.schemes {
				c.add(s)
			}
		}
	}
}

// ExpandEnv enables ${env:NAME} references, resolved with
// os.LookupEnv. A missing variable is an expansion error.
func ExpandEnv() ExpandOption {
	return WithResolver(NewResolver(EnvScheme(os.LookupEnv)))
}

// WithEnv enables ${env:NAME} references resolved through lookup.
func WithEnv(lookup func(string) (string, bool)) ExpandOption {
	return WithResolver(NewResolver(EnvScheme(lookup)))
}

// ExpandBase enables ${base} and ${base:rel} references rooted at
// baseDir, for paths relative to the deployment document.
func ExpandBase(baseDir string) ExpandOption {
	return WithResolver(NewResolver(BaseScheme(baseDir)))
}

// ExpandHome enables "~" / "~/..." and ${home} / ${home:rel}
// references rooted at the current user's home directory.
func ExpandHome() ExpandOption {
	return WithResolver(NewResolver(HomeScheme()))
}

// Expand walks raw settings JSON and expands scalar string references
// everywhere strings can appear (map values, array items). Without
// options the input is returned unchanged. Expansion is strict: a
// reference whose scheme is not enabled, an unknown scheme, a missing
// env variable, a base reference without a base directory, a nested or
// malformed reference, or an unterminated "${" is an error. A literal
// "${" can be written as "\${"; the backslash is consumed and the
// reference is emitted verbatim.
func Expand(ctx context.Context, raw []byte, opts ...ExpandOption) (json.RawMessage, error) {
	if len(opts) == 0 {
		if len(raw) == 0 {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(raw), nil
	}
	var cfg expandConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errdefs.Validationf(
			"resource settings expand: %v", err)
	}
	expanded, err := expandValue(ctx, value, &cfg)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(expanded)
	if err != nil {
		return nil, errdefs.Validationf(
			"resource settings expand: encode: %v", err)
	}
	return out, nil
}

// expandString is the string-level entry for the config walk: it keeps
// the "~" / "~/..." home shorthand, then delegates to [ExpandRefs].
func expandString(ctx context.Context, s string, cfg *expandConfig) (any, error) {
	if cfg.hasScheme("home") && (s == "~" || strings.HasPrefix(s, "~/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errdefs.Validationf(
				"resource settings expand: home: %v", err)
		}
		return filepath.Join(home, strings.TrimPrefix(s, "~")), nil
	}
	return ExpandRefs(ctx, s, configResolver{cfg: cfg})
}

// expandValue walks a decoded JSON value, expanding strings.
func expandValue(ctx context.Context, value any, cfg *expandConfig) (any, error) {
	switch v := value.(type) {
	case string:
		return expandString(ctx, v, cfg)
	case map[string]any:
		for key, item := range v {
			expanded, err := expandValue(ctx, item, cfg)
			if err != nil {
				return nil, err
			}
			v[key] = expanded
		}
		return v, nil
	case []any:
		for i, item := range v {
			expanded, err := expandValue(ctx, item, cfg)
			if err != nil {
				return nil, err
			}
			v[i] = expanded
		}
		return v, nil
	default:
		return value, nil
	}
}
