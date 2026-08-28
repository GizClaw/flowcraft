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
//
// Resolve returns the materialized value. A plain string replaces the
// reference inline; any other JSON-serializable value (e.g. the
// {"store":..., "name":...} marker produced for lazy secret refs) is
// spliced into the settings tree in its place.
type Scheme interface {
	// Name is the scheme prefix inside ${scheme:ref}.
	Name() string
	// Resolve materializes ref. ref is the text after "scheme:" with
	// surrounding whitespace trimmed. An unresolvable ref is an error.
	Resolve(ctx context.Context, ref string) (any, error)
}

// SchemeFunc adapts a function to [Scheme].
type SchemeFunc struct {
	SchemeName string
	Fn         func(context.Context, string) (any, error)
}

func (s SchemeFunc) Name() string { return s.SchemeName }
func (s SchemeFunc) Resolve(ctx context.Context, ref string) (any, error) {
	return s.Fn(ctx, ref)
}

// ReferenceResolver resolves ${scheme:ref} references by scheme name.
// Unknown or disabled schemes are validation errors; see [Expand].
type ReferenceResolver struct {
	schemes  map[string]Scheme
	deferred []string
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
func (r *ReferenceResolver) Resolve(ctx context.Context, scheme, ref string) (any, error) {
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

// WithDeferred returns a resolver that additionally passes references
// under the given prefixes through expansion verbatim (both the plain
// ${prefix...} form and the \${prefix...} escaped form keep their
// backslash). Deferred namespaces belong to a downstream resolver that
// runs later — e.g. the agent board's ${board.*} references, which are
// resolved against agent.Board at execution time.
func (r *ReferenceResolver) WithDeferred(prefixes ...string) *ReferenceResolver {
	merged := NewResolver()
	if r != nil {
		for name, existing := range r.schemes {
			merged.schemes[name] = existing
		}
		merged.deferred = append(merged.deferred, r.deferred...)
	}
	merged.deferred = append(merged.deferred, prefixes...)
	return merged
}

// isDeferred reports whether expr (the text inside ${...}) belongs to a
// deferred namespace.
func (r *ReferenceResolver) isDeferred(expr string) bool {
	if r == nil {
		return false
	}
	for _, prefix := range r.deferred {
		if expr == prefix || strings.HasPrefix(expr, prefix+".") {
			return true
		}
	}
	return false
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
		merged.deferred = append(merged.deferred, r.deferred...)
	}
	if other != nil {
		for name, overlay := range other.schemes {
			merged.schemes[name] = overlay
		}
		merged.deferred = append(merged.deferred, other.deferred...)
	}
	return merged
}

// EnvScheme resolves ${env:NAME} through lookup. A missing variable is
// an error.
func EnvScheme(lookup func(string) (string, bool)) Scheme {
	return SchemeFunc{
		SchemeName: "env",
		Fn: func(_ context.Context, ref string) (any, error) {
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
		Fn: func(_ context.Context, ref string) (any, error) {
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
		Fn: func(_ context.Context, ref string) (any, error) {
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

func (c *expandConfig) addDeferred(prefixes ...string) {
	if c.resolver == nil {
		c.resolver = NewResolver()
	}
	c.resolver.deferred = append(c.resolver.deferred, prefixes...)
}

func (c *expandConfig) hasScheme(name string) bool {
	return c != nil && c.resolver != nil && c.resolver.schemes[name] != nil
}

func (c *expandConfig) isDeferred(expr string) bool {
	return c != nil && c.resolver != nil && c.resolver.isDeferred(expr)
}

func (c *expandConfig) resolve(ctx context.Context, scheme, ref string) (any, error) {
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
			c.addDeferred(r.deferred...)
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

func expandString(ctx context.Context, s string, cfg *expandConfig) (any, error) {
	if cfg.hasScheme("home") && (s == "~" || strings.HasPrefix(s, "~/")) {
		return cfg.resolve(ctx, "home", strings.TrimPrefix(s, "~"))
	}
	if !strings.Contains(s, "${") {
		return s, nil
	}
	// A string that is exactly one reference splices the resolved
	// value as-is, so schemes may return structured (non-string)
	// values such as lazy secret refs.
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") &&
		strings.Count(s, "${") == 1 && strings.Count(s, "}") == 1 {
		expr := s[2 : len(s)-1]
		if cfg.isDeferred(expr) {
			return s, nil
		}
		return expandExpr(ctx, expr, cfg)
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
			// Strictness mirrors real references: an escaped "${" must
			// still be a well-formed reference shape with a closing
			// brace.
			closeRel := strings.Index(rest[escStart+1:], "}")
			if closeRel < 0 {
				return "", errdefs.Validationf(
					"resource settings expand: unterminated escaped reference in %q", s)
			}
			escEnd := escStart + 1 + closeRel
			escExpr := rest[escStart+3 : escEnd]
			if cfg.isDeferred(escExpr) {
				// Deferred namespaces keep the whole escaped form
				// (backslash included) for the downstream resolver.
				builder.WriteString(rest[:escEnd+1])
				rest = rest[escEnd+1:]
				continue
			}
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
		expr := rest[refStart+2 : end]
		builder.WriteString(rest[:refStart])
		if cfg.isDeferred(expr) {
			builder.WriteString(rest[refStart : end+1])
			rest = rest[end+1:]
			continue
		}
		replacement, err := expandExpr(ctx, expr, cfg)
		if err != nil {
			return "", err
		}
		replacementString, ok := replacement.(string)
		if !ok {
			return "", errdefs.Validationf(
				"resource settings expand: reference ${%s} cannot be embedded in text",
				rest[refStart+2:end])
		}
		builder.WriteString(replacementString)
		rest = rest[end+1:]
	}
	return builder.String(), nil
}

func expandExpr(ctx context.Context, expr string, cfg *expandConfig) (any, error) {
	scheme, ref, _ := strings.Cut(expr, ":")
	scheme = strings.TrimSpace(scheme)
	if scheme == "" {
		return "", errdefs.Validationf(
			"resource settings expand: unknown reference ${%s}", expr)
	}
	return cfg.resolve(ctx, scheme, strings.TrimSpace(ref))
}
