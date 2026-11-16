package runtime

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"

	"go.opentelemetry.io/otel/attribute"
)

// DescriptionsConfig replaces the model-facing text of the tools a runtime
// assembles, keyed by the name the model calls.
//
// The description is prompt surface, not contract: it decides whether the model
// picks the right tool and fills the right arguments, and that answer differs per
// deployment — a workspace that must not run find, a router that must not spawn
// sessions. Hard-coding one answer served neither side, so the deployment owns the
// wording and the tool keeps its behaviour.
//
// A configured text is the text: nothing of the generated description survives.
// Composition was considered and rejected — a template cannot delete the sentence
// that is wrong for this deployment, and deleting is the common case. What a
// deployment needs to start from is the generated text itself, which
// `forge describe tools` prints.
type DescriptionsConfig map[string]DescriptionConfig

// DescriptionConfig replaces one tool's description and, optionally, the
// description shown for individual top-level parameters.
type DescriptionConfig struct {
	// Text replaces the tool description.
	Text string `json:"text"`
	// Params replaces parameter descriptions, keyed by the property name in the
	// tool's input schema. Only the description changes: type, required, bounds
	// and enum stay whatever the tool declared, because those are the contract
	// the arguments are validated against.
	Params map[string]string `json:"params"`
}

// UnmarshalJSON accepts the one-line form — `read_file: "text"` — as shorthand for
// `read_file: {text: "text"}`.
//
// It re-establishes the strictness the surrounding decode already asked for: a
// custom unmarshaler suspends [json.Decoder.DisallowUnknownFields] for its subtree,
// and a typo inside a description entry would otherwise be dropped in silence.
func (d *DescriptionConfig) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*d = DescriptionConfig{Text: text}
		return nil
	}
	type plain DescriptionConfig
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*d = DescriptionConfig(decoded)
	return nil
}

// validate checks the entries against each other and the document, before any
// tool is built: an override that names nothing is a configuration typo, and a
// deployment must not start with a typo silently in force.
func (c DescriptionsConfig) validate() error {
	for _, name := range c.sortedNames() {
		override := c[name]
		if strings.TrimSpace(override.Text) == "" {
			return errdefs.Validationf(
				"tools.descriptions.%s: text is required", name)
		}
		for _, param := range sortedKeys(override.Params) {
			if strings.TrimSpace(override.Params[param]) == "" {
				return errdefs.Validationf(
					"tools.descriptions.%s.params.%s: text is required", name, param)
			}
		}
	}
	return nil
}

// validateAgainst checks the entries against the tools that were actually wired.
//
// It runs after the assembly, because that is the first moment a name can be
// answered: the built-ins, the host's extra sources and an eager MCP server all
// contribute here. A name that only a deferred MCP server will publish cannot be
// answered at build time, so it is reported as unknown rather than kept for later
// — a name this deployment never gets is a typo, and a typo that waits is a typo
// nobody finds.
func (c DescriptionsConfig) validateAgainst(catalog toolLookup) error {
	known := catalog.Names()
	for _, name := range c.sortedNames() {
		registered, ok := catalog.Get(name)
		if !ok {
			return errdefs.Validationf(
				"tools.descriptions: unknown tool %q; wired: %s. "+
					"A tool a deferred MCP server publishes is not listed until it connects",
				name, summarise(known))
		}
		override := c[name]
		if len(override.Params) == 0 {
			continue
		}
		properties, err := topLevelProperties(registered.Definition().InputSchema)
		if err != nil {
			return errdefs.Validationf(
				"tools.descriptions.%s: input schema: %w", name, err)
		}
		for _, param := range sortedKeys(override.Params) {
			if _, ok := properties[param]; !ok {
				return errdefs.Validationf(
					"tools.descriptions.%s: unknown parameter %q; it takes: %s",
					name, param, summarise(sortedKeys(properties)))
			}
		}
	}
	return nil
}

// withDefinitionDecorator returns the runtime's definition decorator, or nil when
// no descriptions are configured — a runtime without overrides must not pay for a
// pass over definitions it will not change.
func (c DescriptionsConfig) withDefinitionDecorator(ctx context.Context) tool.DefinitionDecorator {
	if len(c) == 0 {
		return nil
	}
	state := &descriptionState{ctx: ctx}
	return func(def message.ToolDefinition) message.ToolDefinition {
		override, ok := c[def.Name]
		if !ok {
			return def
		}
		decorated := def.Clone()
		decorated.Description = strings.TrimSpace(override.Text)
		if len(override.Params) == 0 {
			return decorated
		}
		schema, err := patchParameterDescriptions(def.InputSchema, override.Params)
		if err != nil {
			// The text is what this feature is for; the schema is not. An override
			// that cannot be applied leaves the schema alone and says so, because a
			// runtime that refuses to start over a cosmetic mismatch is worse than
			// one that starts without it.
			state.warnOnce(def.Name, err)
			return decorated
		}
		decorated.InputSchema = schema
		return decorated
	}
}

// descriptionState keeps the decorator from repeating the same warning on every
// definition rebuild, which happens on every MCP tool publication.
type descriptionState struct {
	ctx  context.Context
	mu   sync.Mutex
	seen map[string]struct{}
}

func (s *descriptionState) warnOnce(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, ok := s.seen[name]; ok {
		return
	}
	s.seen[name] = struct{}{}
	telemetry.WarnErr(s.ctx, "tools.descriptions: input schema left untouched", err,
		attribute.String(telemetry.AttrToolName, name))
}

// patchParameterDescriptions rewrites the top-level property descriptions of one
// input schema and leaves everything else — types, bounds, enum, required,
// additionalProperties, nested schemas — semantically identical.
//
// Raw bodies are carried through unparsed wherever this function has no business
// editing them, so a number written as 1 cannot come back as 1.0000001 after a
// float64 round trip.
func patchParameterDescriptions(schema json.RawMessage, params map[string]string) (json.RawMessage, error) {
	var root map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	rawProperties, ok := root["properties"]
	if !ok {
		return nil, errdefs.Validationf("no properties object")
	}
	var propertySchemas map[string]json.RawMessage
	if err := json.Unmarshal(rawProperties, &propertySchemas); err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(params) {
		body, ok := propertySchemas[name]
		if !ok {
			continue // a late name; the build-time check already covered the wired set
		}
		var property map[string]json.RawMessage
		if err := json.Unmarshal(body, &property); err != nil {
			return nil, errdefs.Validationf("parameter %q: %w", name, err)
		}
		description, err := json.Marshal(strings.TrimSpace(params[name]))
		if err != nil {
			return nil, err
		}
		property["description"] = description
		updated, err := json.Marshal(property)
		if err != nil {
			return nil, err
		}
		propertySchemas[name] = updated
	}
	encoded, err := json.Marshal(propertySchemas)
	if err != nil {
		return nil, err
	}
	root["properties"] = encoded
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// topLevelProperties decodes the properties object of an input schema, keeping
// every property body as raw JSON. Only the top level is reachable: a parameter
// description nested inside $defs or oneOf belongs to a shape this feature does not
// claim to describe.
func topLevelProperties(schema json.RawMessage) (map[string]json.RawMessage, error) {
	var root struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	return root.Properties, nil
}

// toolLookup is the read side validateAgainst needs; *tool.Registry implements it.
type toolLookup interface {
	Get(name string) (tool.Tool, bool)
	Names() []string
}

// sortedNames returns the configured tool names in order, so validation reports
// the same problem first on every run.
func (c DescriptionsConfig) sortedNames() []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedKeys returns a map's keys in order, for the same reason.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// summarise renders a name list for an error, bounded so one long tool catalog
// cannot bury the sentence that says what the problem is.
func summarise(names []string) string {
	const limit = 12
	if len(names) == 0 {
		return "none"
	}
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:limit], ", ") + ", …"
}
