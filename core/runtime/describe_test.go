package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// A one-line entry is the form most deployments write, so it has to survive the
// same strict decode that rejects a misspelled field everywhere else.
func TestDescriptionsAcceptTheOneLineForm(t *testing.T) {
	cfg, err := decode.Decode[ToolsConfig]([]byte(`
descriptions:
  read_file: "Read a file."
  bash:
    text: "Run a command."
    params:
      workdir: "Absolute directory."
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := cfg.Descriptions["read_file"].Text; got != "Read a file." {
		t.Errorf("read_file text = %q", got)
	}
	if got := cfg.Descriptions["bash"].Params["workdir"]; got != "Absolute directory." {
		t.Errorf("bash.workdir = %q", got)
	}
}

// The custom unmarshaler must not become a hole in the document's strictness: a
// field nobody reads would otherwise be dropped in silence.
func TestDescriptionsRejectUnknownFields(t *testing.T) {
	_, err := decode.Decode[ToolsConfig]([]byte(`
descriptions:
  bash:
    txt: "typo"
`))
	if err == nil {
		t.Fatal("unknown field accepted inside a description entry")
	}
}

func TestDescriptionsRequireText(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Tools.Descriptions = DescriptionsConfig{"bash": {Text: "   "}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty text accepted")
	}
	if !strings.Contains(err.Error(), "tools.descriptions.bash") {
		t.Errorf("error %q does not name the entry", err)
	}
}

func TestDescriptionsRequireParameterText(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Tools.Descriptions = DescriptionsConfig{"bash": {
		Text:   "Run a command.",
		Params: map[string]string{"workdir": ""},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty parameter text accepted")
	}
}

// The generated text is the whole point: a deployment that writes its own
// sentence gets exactly that sentence, dynamic parts included or not.
func TestDescriptionsReplaceTheToolText(t *testing.T) {
	rt := describedRuntime(t, DescriptionsConfig{
		"exec_session": {Text: "Start and stop background processes."},
	})
	def := definitionOf(t, rt, "exec_session")
	if def.Description != "Start and stop background processes." {
		t.Errorf("description = %q, want the configured text alone", def.Description)
	}
}

// A parameter override is text and nothing else: the bounds the tool declared are
// the contract its arguments are validated against, and they stay.
func TestDescriptionsReplaceParameterTextAndKeepTheSchema(t *testing.T) {
	baseline := definitionOf(t, plainRuntime(t), "grep")
	patched := definitionOf(t, describedRuntime(t, DescriptionsConfig{"grep": {
		Text:   "Search file contents.",
		Params: map[string]string{"max_matches": "Keep it small."},
	}}), "grep")

	before := propertiesOf(t, baseline)
	after := propertiesOf(t, patched)
	if len(before) != len(after) {
		t.Fatalf("parameters = %d, want %d", len(after), len(before))
	}
	for name, body := range before {
		if name == "max_matches" {
			continue
		}
		if body != after[name] {
			t.Errorf("parameter %q changed:\n got %s\nwant %s", name, after[name], body)
		}
	}
	if got := descriptionOf(t, after["max_matches"]); got != "Keep it small." {
		t.Errorf("max_matches description = %q", got)
	}
	// Everything the patched parameter said beyond its description survives.
	if got, want := keysOf(t, after["max_matches"]), keysOf(t, before["max_matches"]); got != want {
		t.Errorf("max_matches keys = %s, want %s", got, want)
	}
	for _, key := range strings.Split(keysOf(t, before["max_matches"]), ",") {
		if key == "description" {
			continue
		}
		if got, want := fieldOf(t, after["max_matches"], key), fieldOf(t, before["max_matches"], key); got != want {
			t.Errorf("max_matches.%s = %s, want %s", key, got, want)
		}
	}
	if got, want := fieldOfRaw(t, patched.InputSchema, "required"), fieldOfRaw(t, baseline.InputSchema, "required"); got != want {
		t.Errorf("required = %s, want %s", got, want)
	}
}

func TestDescriptionsRejectUnknownTool(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Tools.Descriptions = DescriptionsConfig{"reed_file": {Text: "typo"}}
	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("unknown tool accepted")
	}
	if !strings.Contains(err.Error(), "reed_file") || !strings.Contains(err.Error(), "read_file") {
		t.Errorf("error %q does not name the typo and the wired tools", err)
	}
}

func TestDescriptionsRejectUnknownParameter(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Tools.Descriptions = DescriptionsConfig{"read_file": {
		Text:   "Read a file.",
		Params: map[string]string{"filepath": "typo"},
	}}
	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("unknown parameter accepted")
	}
	if !strings.Contains(err.Error(), "filepath") || !strings.Contains(err.Error(), "file_path") {
		t.Errorf("error %q does not name the typo and the real parameters", err)
	}
}

// A description nobody configured must not be touched, byte for byte: the
// feature exists to change some text, never to rewrite the rest.
func TestDescriptionsLeaveEveryOtherToolAlone(t *testing.T) {
	baseline := plainRuntime(t)
	described := describedRuntime(t, DescriptionsConfig{"exec_session": {Text: "Changed."}})
	for _, name := range []string{"read_file", "write_file", "grep", "glob", "list_dir", "edit"} {
		got, want := definitionOf(t, described, name), definitionOf(t, baseline, name)
		if got.Description != want.Description {
			t.Errorf("%s description changed without being configured", name)
		}
		if string(got.InputSchema) != string(want.InputSchema) {
			t.Errorf("%s input schema changed without being configured", name)
		}
	}
}

// Editing a description changes what the model reads, so it changes the
// deployment's identity: a session that started under one text must not resume
// under another. The digest is where that is enforced.
func TestDigestTracksDescriptions(t *testing.T) {
	root := t.TempDir()
	cfg := validConfig(root)
	plain := cfg.Digest()
	cfg.Tools.Descriptions = DescriptionsConfig{"read_file": {Text: "Read a file."}}
	if plain == cfg.Digest() {
		t.Error("digest ignored a description override")
	}
}

func plainRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(context.Background(), localConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func describedRuntime(t *testing.T, descriptions DescriptionsConfig) *Runtime {
	t.Helper()
	cfg := localConfig(t)
	cfg.Tools.Descriptions = descriptions
	rt, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New with descriptions: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func definitionOf(t *testing.T, rt *Runtime, name string) message.ToolDefinition {
	t.Helper()
	for _, def := range rt.Tools().Definitions() {
		if def.Name == name {
			return def
		}
	}
	t.Fatalf("tool %q is not wired; wired: %v", name, rt.Tools().Names())
	return message.ToolDefinition{}
}

func propertiesOf(t *testing.T, def message.ToolDefinition) map[string]string {
	t.Helper()
	var root struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(def.InputSchema, &root); err != nil {
		t.Fatalf("%s: input schema: %v", def.Name, err)
	}
	out := make(map[string]string, len(root.Properties))
	for name, body := range root.Properties {
		out[name] = string(body)
	}
	return out
}

func descriptionOf(t *testing.T, body string) string {
	t.Helper()
	var property struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(body), &property); err != nil {
		t.Fatalf("parameter %s: %v", body, err)
	}
	return property.Description
}

// keysOf renders a property body's keys in a stable order, so a report says what
// changed instead of printing two JSON blobs.
func keysOf(t *testing.T, body string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("parameter %s: %v", body, err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return strings.Join(keys, ",")
}

func fieldOf(t *testing.T, body, key string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("parameter %s: %v", body, err)
	}
	return string(fields[key])
}

func fieldOfRaw(t *testing.T, schema json.RawMessage, key string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		t.Fatalf("input schema: %v", err)
	}
	return string(fields[key])
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
