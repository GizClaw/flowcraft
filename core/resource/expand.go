package resource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// expandConfig carries the enabled reference kinds and their sources.
type expandConfig struct {
	env     func(string) (string, bool)
	baseDir string
	useEnv  bool
	useBase bool
	useHome bool
}

// ExpandOption configures scalar settings expansion.
type ExpandOption func(*expandConfig)

// ExpandEnv enables ${env:NAME} references, resolved with
// os.LookupEnv. A missing variable is an expansion error.
func ExpandEnv() ExpandOption {
	return func(c *expandConfig) {
		c.useEnv = true
		c.env = os.LookupEnv
	}
}

// WithEnv enables ${env:NAME} references resolved through lookup.
func WithEnv(lookup func(string) (string, bool)) ExpandOption {
	return func(c *expandConfig) {
		c.useEnv = true
		c.env = lookup
	}
}

// ExpandBase enables ${base} and ${base:rel} references rooted at
// baseDir, for paths relative to the deployment document.
func ExpandBase(baseDir string) ExpandOption {
	return func(c *expandConfig) {
		c.useBase = true
		c.baseDir = baseDir
	}
}

// ExpandHome enables "~" / "~/..." and ${home} / ${home:rel}
// references rooted at the current user's home directory.
func ExpandHome() ExpandOption {
	return func(c *expandConfig) { c.useHome = true }
}

// Expand walks raw settings JSON and expands scalar string references
// everywhere strings can appear (map values, array items). Without
// options the input is returned unchanged. Expansion is strict: a
// reference whose kind is not enabled, an unknown reference, a
// missing env variable, or a malformed "${" is an error.
func Expand(raw []byte, opts ...ExpandOption) (json.RawMessage, error) {
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
	expanded, err := expandValue(value, &cfg)
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

func expandValue(value any, cfg *expandConfig) (any, error) {
	switch v := value.(type) {
	case string:
		return expandString(v, cfg)
	case map[string]any:
		for key, item := range v {
			expanded, err := expandValue(item, cfg)
			if err != nil {
				return nil, err
			}
			v[key] = expanded
		}
		return v, nil
	case []any:
		for i, item := range v {
			expanded, err := expandValue(item, cfg)
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

func expandString(s string, cfg *expandConfig) (string, error) {
	if cfg.useHome && (s == "~" || strings.HasPrefix(s, "~/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errdefs.Validationf(
				"resource settings expand: home: %v", err)
		}
		return filepath.Join(home, strings.TrimPrefix(s, "~")), nil
	}
	if !strings.Contains(s, "${") {
		return s, nil
	}

	var builder strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			builder.WriteString(rest)
			break
		}
		relativeEnd := strings.Index(rest[start:], "}")
		if relativeEnd < 0 {
			return "", errdefs.Validationf(
				"resource settings expand: unterminated reference in %q", s)
		}
		end := start + relativeEnd
		builder.WriteString(rest[:start])
		replacement, err := expandExpr(rest[start+2:end], cfg)
		if err != nil {
			return "", err
		}
		builder.WriteString(replacement)
		rest = rest[end+1:]
	}
	return builder.String(), nil
}

func expandExpr(expr string, cfg *expandConfig) (string, error) {
	switch {
	case strings.HasPrefix(expr, "env:"):
		if !cfg.useEnv || cfg.env == nil {
			return "", errdefs.Validationf(
				"resource settings expand: env reference requires ExpandEnv")
		}
		name := strings.TrimSpace(strings.TrimPrefix(expr, "env:"))
		value, ok := cfg.env(name)
		if !ok {
			return "", errdefs.Validationf(
				"resource settings expand: env %q is not set", name)
		}
		return value, nil
	case expr == "base":
		if !cfg.useBase {
			return "", errdefs.Validationf(
				"resource settings expand: base reference requires ExpandBase")
		}
		return cfg.baseDir, nil
	case strings.HasPrefix(expr, "base:"):
		if !cfg.useBase {
			return "", errdefs.Validationf(
				"resource settings expand: base reference requires ExpandBase")
		}
		return filepath.Join(cfg.baseDir, strings.TrimPrefix(expr, "base:")), nil
	case expr == "home" || strings.HasPrefix(expr, "home:"):
		if !cfg.useHome {
			return "", errdefs.Validationf(
				"resource settings expand: home reference requires ExpandHome")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errdefs.Validationf(
				"resource settings expand: home: %v", err)
		}
		if expr == "home" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(expr, "home:")), nil
	default:
		return "", errdefs.Validationf(
			"resource settings expand: unknown reference ${%s}", expr)
	}
}
