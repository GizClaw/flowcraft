package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustSource(t *testing.T, raw string) Source {
	t.Helper()
	var s Source
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal source %s: %v", raw, err)
	}
	return s
}

func TestSource_StringIsLiteral(t *testing.T) {
	s := mustSource(t, `"你是一个狼人杀主持人"`)
	if !s.IsLiteral() {
		t.Fatal("string source should be literal")
	}
	got, ok := s.Literal()
	if !ok || got != "你是一个狼人杀主持人" {
		t.Fatalf("literal = %q, %v", got, ok)
	}
}

func TestSource_FileRef(t *testing.T) {
	s := mustSource(t, `{"file":"./graphs/a.yaml"}`)
	if s.Kind() != SourceFile {
		t.Fatalf("kind = %v, want file", s.Kind())
	}
	path, ok := s.File()
	if !ok || path != "./graphs/a.yaml" {
		t.Fatalf("file = %q, %v", path, ok)
	}
}

func TestSource_EmbedRef(t *testing.T) {
	s := mustSource(t, `{"embed":"scenarios/werewolf/deploy.yaml"}`)
	if s.Kind() != SourceEmbed {
		t.Fatalf("kind = %v, want embed", s.Kind())
	}
	name, ok := s.Embed()
	if !ok || name != "scenarios/werewolf/deploy.yaml" {
		t.Fatalf("embed = %q, %v", name, ok)
	}
}

func TestSource_ReferenceMixedWithContentRejected(t *testing.T) {
	// A "file" key turns the object into a reference, so mixing it with
	// content keys is a decode error instead of silently becoming a
	// document with a stray "file" field.
	err := json.Unmarshal([]byte(`{"file":"./a.yaml","version":"v1"}`), &Source{})
	if err == nil {
		t.Fatal("reference mixed with content keys accepted")
	} else if !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v, want combined-keys error", err)
	}
}

func TestSource_StructuredContent(t *testing.T) {
	raw := `{"version":"v1","workspaces":{"project":{"driver":"local","settings":{"root":"."}}}}`
	src := mustSource(t, raw)
	if got, ok := src.Content(); !ok || string(got) != raw {
		t.Fatalf("content = %q, %v", got, ok)
	}
}

// The legacy {inline: ...} object is now plain structured content: the
// document itself carries an "inline" key, which the owning module's
// strict parser rejects at build time.
func TestSource_LegacyInlineIsNowContent(t *testing.T) {
	src := mustSource(t, `{"inline":{"version":"v1"}}`)
	if raw, ok := src.Content(); !ok || string(raw) != `{"inline":{"version":"v1"}}` {
		t.Fatalf("legacy inline should decode as content, got %q, %v", raw, ok)
	}
}

func TestSource_FileAndEmbedMutuallyExclusive(t *testing.T) {
	if err := json.Unmarshal(
		[]byte(`{"file":"./a.yaml","embed":"b.yaml"}`), &Source{}); err == nil {
		t.Fatal("file and embed both set accepted")
	}
}

func TestSource_EmptyObjectIsContent(t *testing.T) {
	// {} has no file/embed key, so it is structured content; the owning
	// module's parser rejects an empty document.
	src := mustSource(t, `{}`)
	if _, ok := src.Content(); !ok {
		t.Fatalf("empty object should decode as content, got %v", src.Kind())
	}
}

func TestSource_EmptyFileRejected(t *testing.T) {
	if err := json.Unmarshal([]byte(`{"file":""}`), &Source{}); err == nil {
		t.Fatal("empty file path accepted")
	}
}

func TestSource_NonStringNonObjectRejected(t *testing.T) {
	for _, raw := range []string{`123`, `null`, `[1]`, `true`} {
		if err := json.Unmarshal([]byte(raw), &Source{}); err == nil {
			t.Fatalf("source %s accepted", raw)
		}
	}
}

func TestSource_RoundTrip(t *testing.T) {
	for _, raw := range []string{
		`"content"`,
		`{"version":"v1"}`,
		`{"file":"./a.yaml"}`,
		`{"embed":"a/b.yaml"}`,
	} {
		s := mustSource(t, raw)
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %s: %v", raw, err)
		}
		if string(out) != raw {
			t.Fatalf("round trip %s -> %s", raw, out)
		}
	}
}

// Type-level protection: a plain string field is never interpreted as a
// reference, even when its value looks like one.
func TestSource_PlainStringFieldNeverResolved(t *testing.T) {
	var doc struct {
		Prompt string `json:"prompt"`
	}
	raw := `{"prompt":"{file: ./x.yaml} and @embed://y"}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Prompt != "{file: ./x.yaml} and @embed://y" {
		t.Fatalf("prompt = %q, want untouched literal", doc.Prompt)
	}
}

func TestRef_RequiresObject(t *testing.T) {
	if err := json.Unmarshal([]byte(`"./a.yaml"`), &Ref{}); err == nil {
		t.Fatal("string ref accepted")
	}
	if err := json.Unmarshal([]byte(`{"file":"./a.yaml","embed":"b"}`), &Ref{}); err == nil {
		t.Fatal("ref with both file and embed accepted")
	}
	if err := json.Unmarshal([]byte(`{"inline":{"a":1}}`), &Ref{}); err == nil {
		t.Fatal("ref with unknown field accepted")
	}
}

func TestRef_FileAndEmbed(t *testing.T) {
	var r Ref
	if err := json.Unmarshal([]byte(`{"file":"./a.yaml"}`), &r); err != nil {
		t.Fatalf("unmarshal file ref: %v", err)
	}
	if path, ok := r.File(); !ok || path != "./a.yaml" {
		t.Fatalf("file = %q, %v", path, ok)
	}
	if err := json.Unmarshal([]byte(`{"embed":"a/b.js"}`), &r); err != nil {
		t.Fatalf("unmarshal embed ref: %v", err)
	}
	if name, ok := r.Embed(); !ok || name != "a/b.js" {
		t.Fatalf("embed = %q, %v", name, ok)
	}
}
