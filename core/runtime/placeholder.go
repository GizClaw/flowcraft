package runtime

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"go.yaml.in/yaml/v3"
)

// ResolverFunc resolves one placeholder reference to its value.
//
// args is everything after the scheme, split on ":": ${secret:openai:luna}
// reaches a resolver registered as "secret" with []string{"openai", "luna"}. This
// package never interprets those arguments — whether they name a vault path, an
// environment variable or a row in a table is the host's business.
type ResolverFunc func(ctx context.Context, args []string) (string, error)

// SchemeEnv is the built-in scheme behind bare ${NAME} and ${NAME:-fallback}.
const SchemeEnv = "env"

// LoadOption configures how placeholders are resolved.
type LoadOption func(*loadOptions)

type loadOptions struct {
	resolvers map[string]ResolverFunc
}

// WithResolver registers a scheme. A scheme is looked up only when a
// placeholder names it, so a host pays nothing for schemes it never uses.
//
// The resolver receives the load context: reading a secret store may need a
// deadline and a cancellation path, and pretending otherwise would force every
// host to hide I/O behind a package-level client.
func WithResolver(scheme string, resolve ResolverFunc) LoadOption {
	return func(o *loadOptions) {
		if o.resolvers == nil {
			o.resolvers = make(map[string]ResolverFunc, 2)
		}
		o.resolvers[scheme] = resolve
	}
}

// placeholder is one ${...} reference, already split.
type placeholder struct {
	// text is the literal run that precedes this reference.
	text string
	// scheme is SchemeEnv for a bare ${NAME}.
	scheme string
	// args is everything after the scheme, split on ":".
	args []string
	// hasFallback and fallback describe ${NAME:-fallback}. A fallback belongs to
	// the env scheme only: anywhere else it would be a default credential, which
	// is a footgun rather than a convenience.
	hasFallback bool
	fallback    string
}

// scanPlaceholders splits one value into literals and references.
//
// It is the single parser: expansion, [RequiredEnv] and [Required] all read the
// same split, so they cannot disagree about what counts as a reference.
func scanPlaceholders(value, where string) ([]placeholder, error) {
	out := make([]placeholder, 0, 4)
	var literal strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' {
			literal.WriteByte(value[i])
			i++
			continue
		}
		// "$$" is a literal dollar, so a value may contain "${NAME}" as text.
		if i+1 < len(value) && value[i+1] == '$' {
			literal.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(value) || value[i+1] != '{' {
			literal.WriteByte('$')
			i++
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			return nil, errdefs.Validationf(
				"runtime: %s: unterminated ${ in configuration", where)
		}
		body := value[i+2 : i+2+end]
		if body == "" {
			return nil, errdefs.Validationf(
				"runtime: %s: empty ${} reference", where)
		}
		parsed, err := parseReference(body, where)
		if err != nil {
			return nil, err
		}
		parsed.text = literal.String()
		literal.Reset()
		out = append(out, parsed)
		i += 2 + end + 1
	}
	out = append(out, placeholder{text: literal.String()})
	return out, nil
}

// parseReference splits the inside of ${...}.
//
// The fallback form is checked first: ${NAME:-fallback} starts with the same
// colon a scheme reference would use, so parsing the scheme first would read
// "REGION" as a scheme and "-us-east-1" as its argument.
func parseReference(body, where string) (placeholder, error) {
	if name, fallback, hasFallback := strings.Cut(body, ":-"); hasFallback {
		if name == "" {
			return placeholder{}, errdefs.Validationf(
				"runtime: %s: empty ${} reference", where)
		}
		if strings.Contains(name, ":") {
			return placeholder{}, errdefs.Validationf(
				"runtime: %s: ${%s} uses a fallback, which only an environment variable supports",
				where, body)
		}
		return placeholder{
			scheme:      SchemeEnv,
			args:        []string{name},
			hasFallback: true,
			fallback:    fallback,
		}, nil
	}
	scheme, rest, qualified := strings.Cut(body, ":")
	if !qualified {
		return placeholder{scheme: SchemeEnv, args: []string{body}}, nil
	}
	if scheme == "" || rest == "" {
		return placeholder{}, errdefs.Validationf(
			"runtime: %s: malformed ${%s}", where, body)
	}
	return placeholder{scheme: scheme, args: strings.Split(rest, ":")}, nil
}

// resolvePlaceholders expands every reference in a parsed document.
func resolvePlaceholders(ctx context.Context, node *yaml.Node, path string, options loadOptions) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := resolvePlaceholders(ctx, child, path, options); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := resolvePlaceholders(ctx, node.Content[i+1], joinPath(path, node.Content[i].Value), options); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" || !strings.Contains(node.Value, "${") {
			return nil
		}
		expanded, err := expandValue(ctx, node.Value, path, options)
		if err != nil {
			return err
		}
		node.Value = expanded
	}
	return nil
}

// expandValue resolves one string value.
func expandValue(ctx context.Context, value, path string, options loadOptions) (string, error) {
	placeholders, err := scanPlaceholders(value, path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, item := range placeholders {
		out.WriteString(item.text)
		if item.scheme == "" {
			continue
		}
		resolved, err := resolveOne(ctx, item, path, options)
		if err != nil {
			return "", err
		}
		out.WriteString(resolved)
	}
	return out.String(), nil
}

func resolveOne(ctx context.Context, item placeholder, path string, options loadOptions) (string, error) {
	if item.scheme == SchemeEnv {
		name := item.args[0]
		if value, set := os.LookupEnv(name); set {
			return value, nil
		}
		if item.hasFallback {
			return item.fallback, nil
		}
		return "", errdefs.Validationf(
			"runtime: %s: ${%s} is not set and has no ${%s:-fallback}",
			path, name, name)
	}
	resolve, known := options.resolvers[item.scheme]
	if !known {
		return "", errdefs.Validationf(
			"runtime: %s: unknown placeholder scheme %q; offered: %s",
			path, item.scheme, offeredSchemes(options.resolvers))
	}
	value, err := resolve(ctx, item.args)
	if err != nil {
		return "", errdefs.Validationf(
			"runtime: %s: ${%s:%s}: %w",
			path, item.scheme, strings.Join(item.args, ":"), err)
	}
	return value, nil
}

func offeredSchemes(resolvers map[string]ResolverFunc) string {
	names := []string{SchemeEnv}
	for scheme := range resolvers {
		names = append(names, scheme)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ExpandDocument resolves the placeholders in one YAML document and returns the
// document with them replaced.
//
// The rules are the ones LoadLayers applies to a layer: parse first, expand the
// values, and treat an unregistered scheme as an error rather than a literal.
// Nothing here knows what the document means, so a deployment document and a
// runtime layer cannot drift apart.
func ExpandDocument(ctx context.Context, body []byte, opts ...LoadOption) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, errdefs.Validationf("runtime: parse document: %v", err)
	}
	settings := loadOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if err := resolvePlaceholders(ctx, &document, "", settings); err != nil {
		return nil, err
	}
	expanded, err := yaml.Marshal(&document)
	if err != nil {
		return nil, errdefs.Validationf("runtime: encode document: %v", err)
	}
	return expanded, nil
}
