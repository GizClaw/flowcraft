package resource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Scheme resolves one ${scheme:ref} reference kind. Schemes are the
// extension point for new settings reference sources: implement Scheme,
// build a [ReferenceResolver], and enable it with [WithResolver] (or the
// deployment-level WithResolver on core/deploy.Builder).
type Scheme interface {
	// Name is the scheme prefix inside ${scheme:ref}.
	Name() string
	// Resolve materializes ref. ref is the text after "scheme:" with
	// surrounding whitespace trimmed. An unresolvable ref is an error.
	Resolve(ctx context.Context, ref string) (string, error)
}

// SchemeFunc adapts a function to [Scheme].
type SchemeFunc struct {
	SchemeName string
	Fn         func(context.Context, string) (string, error)
}

func (s SchemeFunc) Name() string { return s.SchemeName }
func (s SchemeFunc) Resolve(ctx context.Context, ref string) (string, error) {
	return s.Fn(ctx, ref)
}

// ReferenceResolver resolves ${scheme:ref} references by scheme name.
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

// Resolve materializes one reference. scheme must be registered; a
// missing scheme is a validation error naming the scheme.
func (r *ReferenceResolver) Resolve(ctx context.Context, scheme, ref string) (string, error) {
	if r == nil {
		return "", errdefs.Validationf(
			"resource settings expand: reference scheme %q is not enabled", scheme)
	}
	s, ok := r.schemes[scheme]
	if !ok {
		return "", errdefs.Validationf(
			"resource settings expand: reference scheme %q is not enabled", scheme)
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

// EnvScheme resolves ${env:NAME} through lookup. A missing variable is
// an error.
func EnvScheme(lookup func(string) (string, bool)) Scheme {
	return SchemeFunc{
		SchemeName: "env",
		Fn: func(_ context.Context, ref string) (string, error) {
			name := strings.TrimSpace(ref)
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
		Fn: func(_ context.Context, ref string) (string, error) {
			if dir == "" {
				return "", errdefs.Validationf(
					"resource settings expand: base reference requires a base directory")
			}
			if ref == "" {
				return dir, nil
			}
			return filepath.Join(dir, ref), nil
		},
	}
}

// HomeScheme resolves "~", "~/...", ${home}, and ${home:rel} rooted at
// the current user's home directory.
func HomeScheme() Scheme {
	return SchemeFunc{
		SchemeName: "home",
		Fn: func(_ context.Context, ref string) (string, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", errdefs.Validationf(
					"resource settings expand: home: %v", err)
			}
			if ref == "" {
				return home, nil
			}
			return filepath.Join(home, ref), nil
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

func (c *expandConfig) resolve(ctx context.Context, scheme, ref string) (string, error) {
	if c == nil || c.resolver == nil {
		return "", errdefs.Validationf(
			"resource settings expand: reference scheme %q is not enabled", scheme)
	}
	return c.resolver.Resolve(ctx, scheme, ref)
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
// env variable, a base reference without a base directory, or a
// malformed "${" is an error. A literal "${" can be written as "\${";
// the backslash is consumed and the reference is emitted verbatim.
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

func expandString(ctx context.Context, s string, cfg *expandConfig) (string, error) {
	if cfg.hasScheme("home") && (s == "~" || strings.HasPrefix(s, "~/")) {
		return cfg.resolve(ctx, "home", strings.TrimPrefix(s, "~"))
	}
	if !strings.Contains(s, "${") {
		return s, nil
	}

	var builder strings.Builder
	rest := s
	for {
		refStart := strings.Index(rest, "${")
		escStart := strings.Index(rest, `\${`)
		// An escaped reference that starts at or before the next real
		// reference wins: "\${" emits a literal "${" and the scan
		// continues after it.
		if escStart >= 0 && (refStart < 0 || escStart < refStart) {
			builder.WriteString(rest[:escStart])
			builder.WriteString("${")
			rest = rest[escStart+3:]
			continue
		}
		if refStart < 0 {
			builder.WriteString(rest)
			break
		}
		relativeEnd := strings.Index(rest[refStart:], "}")
		if relativeEnd < 0 {
			return "", errdefs.Validationf(
				"resource settings expand: unterminated reference in %q", s)
		}
		end := refStart + relativeEnd
		builder.WriteString(rest[:refStart])
		replacement, err := expandExpr(ctx, rest[refStart+2:end], cfg)
		if err != nil {
			return "", err
		}
		builder.WriteString(replacement)
		rest = rest[end+1:]
	}
	return builder.String(), nil
}

func expandExpr(ctx context.Context, expr string, cfg *expandConfig) (string, error) {
	scheme, ref, _ := strings.Cut(expr, ":")
	scheme = strings.TrimSpace(scheme)
	if scheme == "" {
		return "", errdefs.Validationf(
			"resource settings expand: unknown reference ${%s}", expr)
	}
	return cfg.resolve(ctx, scheme, strings.TrimSpace(ref))
}
