package resource

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Reference is one parsed ${scheme:path} span inside a string. The path
// is opaque to the parser — each scheme interprets it (e.g. board
// parses "user.name" or "limit:3" with its default separator).
type Reference struct {
	// Scheme is the text before the first colon ("env", "secret",
	// "board", ...).
	Scheme string
	// Path is everything after the first colon.
	Path string
	// Raw is the exact original span, including "${" / "}" and a
	// leading backslash when escaped ("\${board:x}").
	Raw string
	// Escaped reports whether Raw began with a backslash.
	Escaped bool
	// Whole reports whether the reference spans the entire string, so
	// the resolved value may keep its type (a list stays a list).
	// Embedded references must resolve to text.
	Whole bool
}

// Literal returns the standard interpretation of an escaped reference:
// the raw span with the escaping backslash dropped ("${env:NAME}").
func (r Reference) Literal() string {
	if r.Escaped && len(r.Raw) > 0 {
		return r.Raw[1:]
	}
	return r.Raw
}

// Resolver materializes references. Resolvers receive both plain and
// escaped references; the standard escape (drop the backslash, emit the
// literal) is provided by [Reference.Literal]. A scheme that defers to
// a later phase (e.g. the deploy-time passthrough of agent board
// references) returns Raw unchanged for both forms.
type Resolver interface {
	Resolve(ctx context.Context, r Reference) (any, error)
}

// ResolverFunc adapts a function to [Resolver].
type ResolverFunc func(context.Context, Reference) (any, error)

func (f ResolverFunc) Resolve(ctx context.Context, r Reference) (any, error) {
	return f(ctx, r)
}

// ExpandRefs walks a string and replaces every ${scheme:path} span with
// the resolver's output, honoring "\${" escapes and "\}" / "\\" inside
// refs. When the whole string is exactly one reference the resolved
// value is returned as-is (it may be non-string, e.g. a lazy secret
// ref); embedded references must resolve to strings.
func ExpandRefs(ctx context.Context, s string, resolve Resolver) (any, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var sb strings.Builder
	rest := s
	for {
		relPlain := strings.Index(rest, "${")
		relEsc := strings.Index(rest, `\${`)
		if relEsc >= 0 && (relPlain < 0 || relEsc < relPlain) {
			start := relEsc + 1 // at "${"
			end := refEnd(rest, start+2)
			if end < 0 {
				return "", errdefs.Validationf(
					"resource settings expand: unterminated escaped reference in %q", s)
			}
			sb.WriteString(rest[:relEsc])
			replacement, err := resolve.Resolve(ctx, parseReference(rest[relEsc:end], true, false))
			if err != nil {
				return "", err
			}
			str, ok := replacement.(string)
			if !ok {
				return "", errdefs.Validationf(
					"resource settings expand: escaped reference %q must resolve to text", rest[relEsc:end])
			}
			sb.WriteString(str)
			rest = rest[end:]
			continue
		}
		if relPlain < 0 {
			sb.WriteString(rest)
			break
		}
		end := refEnd(rest, relPlain+2)
		if end < 0 {
			return "", errdefs.Validationf(
				"resource settings expand: unterminated reference in %q", s)
		}
		raw := rest[relPlain:end]
		if strings.Contains(raw[2:len(raw)-1], "${") {
			return "", errdefs.Validationf(
				"resource settings expand: nested references are not supported: %q", s)
		}
		whole := relPlain == 0 && end == len(rest)
		replacement, err := resolve.Resolve(ctx, parseReference(raw, false, whole))
		if err != nil {
			return "", err
		}
		if whole {
			return replacement, nil
		}
		str, ok := replacement.(string)
		if !ok {
			return "", errdefs.Validationf(
				"resource settings expand: reference ${%s} cannot be embedded in text",
				raw[2:len(raw)-1])
		}
		sb.WriteString(rest[:relPlain])
		sb.WriteString(str)
		rest = rest[end:]
	}
	return sb.String(), nil
}

// ExpandValue walks a decoded JSON value (maps, slices, strings) and
// expands references in every string, mirroring [ExpandRefs].
func ExpandValue(ctx context.Context, v any, resolve Resolver) (any, error) {
	switch t := v.(type) {
	case string:
		return ExpandRefs(ctx, t, resolve)
	case map[string]any:
		for key, item := range t {
			expanded, err := ExpandValue(ctx, item, resolve)
			if err != nil {
				return nil, err
			}
			t[key] = expanded
		}
		return t, nil
	case []any:
		for i, item := range t {
			expanded, err := ExpandValue(ctx, item, resolve)
			if err != nil {
				return nil, err
			}
			t[i] = expanded
		}
		return t, nil
	default:
		return v, nil
	}
}

func parseReference(raw string, escaped bool, whole bool) Reference {
	body := raw
	if escaped {
		body = raw[1:]
	}
	scheme, path, _ := strings.Cut(body[2:len(body)-1], ":")
	return Reference{
		Scheme:  strings.TrimSpace(scheme),
		Path:    strings.TrimSpace(path),
		Raw:     raw,
		Escaped: escaped,
		Whole:   whole,
	}
}

// refEnd returns the index just past the closing brace of the reference
// starting at from, or -1 when unterminated. A "\}" inside the body is
// part of a default, not the terminator.
func refEnd(s string, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == '}' && !backslashed(s, i) {
			return i + 1
		}
	}
	return -1
}

// backslashed reports whether s[i] is preceded by an odd number of
// backslashes, i.e. escaped.
func backslashed(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// PassthroughScheme resolves every reference under one scheme by
// returning the raw span unchanged (escaped form included), deferring
// the reference to a later phase — the deploy-time handling of agent
// board references.
type PassthroughScheme struct {
	Prefix string
}

func (p PassthroughScheme) Name() string { return p.Prefix }

func (p PassthroughScheme) Resolve(_ context.Context, r Reference) (any, error) {
	return r.Raw, nil
}

func (p PassthroughScheme) ResolveEscaped(_ context.Context, r Reference) (any, error) {
	return r.Raw, nil
}
