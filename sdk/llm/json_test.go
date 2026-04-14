package llm

import (
	"fmt"
	"strings"
	"testing"
)

func TestExtractJSON_PlainObject(t *testing.T) {
	input := `{"name":"alice","age":30}`
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != input {
		t.Errorf("got %q, want %q", got, input)
	}
	if meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=false")
	}
}

func TestExtractJSON_PlainArray(t *testing.T) {
	input := `[1, 2, 3]`
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestExtractJSON_ScalarString(t *testing.T) {
	input := `  "hello world"  `
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `"hello world"` {
		t.Errorf("got %q, want %q", got, `"hello world"`)
	}
}

func TestExtractJSON_ScalarNumber(t *testing.T) {
	input := `  42  `
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestExtractJSON_ScalarBooleanTrue(t *testing.T) {
	got, _, err := ExtractJSON("true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "true" {
		t.Errorf("got %q, want %q", got, "true")
	}
}

func TestExtractJSON_ScalarBooleanFalse(t *testing.T) {
	got, _, err := ExtractJSON("false")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "false" {
		t.Errorf("got %q, want %q", got, "false")
	}
}

func TestExtractJSON_ScalarNull(t *testing.T) {
	got, _, err := ExtractJSON("null")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("got %q, want %q", got, "null")
	}
}

func TestExtractJSON_SurroundingProse(t *testing.T) {
	input := "Here is the result:\n{\"key\":\"value\"}\nDone."
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"key":"value"}` {
		t.Errorf("got %q, want %q", got, `{"key":"value"}`)
	}
}

func TestExtractJSON_CodeFenceJSON(t *testing.T) {
	input := "Sure, here you go:\n```json\n{\"a\":1}\n```\nHope that helps!"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q, want %q", got, `{"a":1}`)
	}
	if !meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=true")
	}
	if meta.CodeBlockLang != "json" {
		t.Errorf("got lang %q, want %q", meta.CodeBlockLang, "json")
	}
}

func TestExtractJSON_CodeFenceNoLang(t *testing.T) {
	input := "Result:\n```\n[1,2,3]\n```"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "[1,2,3]" {
		t.Errorf("got %q, want %q", got, "[1,2,3]")
	}
	if !meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=true")
	}
}

func TestExtractJSON_PrefersJSONCodeBlock(t *testing.T) {
	input := "```python\nprint('hi')\n```\n```json\n{\"chosen\":true}\n```"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"chosen":true}` {
		t.Errorf("got %q, want %q", got, `{"chosen":true}`)
	}
	if meta.CodeBlockLang != "json" {
		t.Errorf("got lang %q, want %q", meta.CodeBlockLang, "json")
	}
}

func TestExtractJSON_KeywordNotConfusedWithProse(t *testing.T) {
	input := "the following is not a valid response\n{\"ok\":true}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("got %q, want %q", got, `{"ok":true}`)
	}
}

func TestExtractJSON_ObjectBeatsScalarNull(t *testing.T) {
	input := "there is nothing here, null\n{\"found\":false}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Structured value takes priority over scalar "null".
	if string(got) != `{"found":false}` {
		t.Errorf("got %q, want %q", got, `{"found":false}`)
	}
}

func TestExtractJSON_ScalarNullOnly(t *testing.T) {
	input := "there is nothing here, null"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("got %q, want %q", got, "null")
	}
}

func TestExtractJSON_ProseWithQuotes(t *testing.T) {
	input := "use \"format\" for output\n{\"a\":1}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q, want %q", got, `{"a":1}`)
	}
}

func TestExtractJSON_ProseWithNumber(t *testing.T) {
	input := "Step 3: process the data\n{\"result\":\"done\"}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"result":"done"}` {
		t.Errorf("got %q, want %q", got, `{"result":"done"}`)
	}
}

func TestExtractJSON_ProseWithDash(t *testing.T) {
	input := "- this is a list item\n- another item\n{\"data\":1}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"data":1}` {
		t.Errorf("got %q, want %q", got, `{"data":1}`)
	}
}

func TestExtractJSON_ObjectBeatsScalarNumber(t *testing.T) {
	input := "value is 42\n{\"answer\":42}"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"answer":42}` {
		t.Errorf("got %q, want %q", got, `{"answer":42}`)
	}
}

func TestExtractJSON_ObjectBeatsScalarString(t *testing.T) {
	input := `He said "hello" and returned {"greeting":"hi"}`
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"greeting":"hi"}` {
		t.Errorf("got %q, want %q", got, `{"greeting":"hi"}`)
	}
}

func TestExtractJSON_CodeFenceOtherLang(t *testing.T) {
	input := "```text\n{\"inside\":true}\n```"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"inside":true}` {
		t.Errorf("got %q, want %q", got, `{"inside":true}`)
	}
	if !meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=true")
	}
	if meta.CodeBlockLang != "text" {
		t.Errorf("got lang %q, want %q", meta.CodeBlockLang, "text")
	}
}

func TestExtractJSON_FenceFallbackToFullText(t *testing.T) {
	input := "```python\nprint(\"hello\")\n```\n{\"actual\":\"data\"}"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"actual":"data"}` {
		t.Errorf("got %q, want %q", got, `{"actual":"data"}`)
	}
	if meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=false after fallback")
	}
}

func TestExtractJSON_FenceFallbackArray(t *testing.T) {
	input := "```text\nno json here\n```\n[1,2,3]"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "[1,2,3]" {
		t.Errorf("got %q, want %q", got, "[1,2,3]")
	}
	if meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=false after fallback")
	}
}

func TestExtractJSON_FenceNoFallbackNeeded(t *testing.T) {
	input := "```json\n{\"in_fence\":true}\n```\n{\"outside\":true}"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// JSON found inside fence — no fallback needed.
	if string(got) != `{"in_fence":true}` {
		t.Errorf("got %q, want %q", got, `{"in_fence":true}`)
	}
	if !meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=true")
	}
}

func TestExtractJSON_FenceAndFullTextBothEmpty(t *testing.T) {
	input := "```text\nno json\n```\nno json here either"
	_, _, err := ExtractJSON(input)
	if err == nil {
		t.Fatal("expected error when neither fence nor full text has JSON")
	}
}

func TestExtractJSON_DashNotNumber(t *testing.T) {
	input := "- not a number\n42"
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// `-` followed by space is skipped, falls through to `42`.
	if string(got) != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestExtractJSON_EmptyInput(t *testing.T) {
	_, _, err := ExtractJSON("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestExtractJSON_WhitespaceOnly(t *testing.T) {
	_, _, err := ExtractJSON("   \n\t  ")
	if err == nil {
		t.Fatal("expected error for whitespace-only input")
	}
}

func TestExtractJSON_NoParseable(t *testing.T) {
	_, _, err := ExtractJSON("this is just plain english text with no json at all")
	if err == nil {
		t.Fatal("expected error when no JSON is present")
	}
}

func TestExtractJSON_NegativeNumber(t *testing.T) {
	got, _, err := ExtractJSON("value is -3.14 ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "-3.14" {
		t.Errorf("got %q, want %q", got, "-3.14")
	}
}

func TestExtractJSON_WindowsLineEndings(t *testing.T) {
	input := "Result:\r\n```json\r\n{\"a\":1}\r\n```\r\n"
	got, meta, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q, want %q", got, `{"a":1}`)
	}
	if !meta.FromCodeBlock {
		t.Error("expected FromCodeBlock=true")
	}
}

func TestExtractJSON_NestedObject(t *testing.T) {
	input := `Here: {"outer":{"inner":[1,2,{"deep":true}]}} done`
	got, _, err := ExtractJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"outer":{"inner":[1,2,{"deep":true}]}}`
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFindFencedCodeBlock_PreferJSON(t *testing.T) {
	text := "```py\ncode\n```\n```json\n{}\n```"
	block, ok := findFencedCodeBlock(text, true)
	if !ok {
		t.Fatal("expected to find json block")
	}
	if block.Lang != "json" {
		t.Errorf("got lang %q, want %q", block.Lang, "json")
	}
	if block.Content != "{}" {
		t.Errorf("got content %q, want %q", block.Content, "{}")
	}
}

func TestFindFencedCodeBlock_FirstBlock(t *testing.T) {
	text := "```py\ncode\n```\n```json\n{}\n```"
	block, ok := findFencedCodeBlock(text, false)
	if !ok {
		t.Fatal("expected to find block")
	}
	if block.Lang != "py" {
		t.Errorf("got lang %q, want %q", block.Lang, "py")
	}
	if block.Content != "code" {
		t.Errorf("got content %q, want %q", block.Content, "code")
	}
}

func TestFindFencedCodeBlock_NoBlock(t *testing.T) {
	_, ok := findFencedCodeBlock("no code here", true)
	if ok {
		t.Error("expected no block found")
	}
}

func TestKeywordAt(t *testing.T) {
	tests := []struct {
		input string
		i     int
		want  string
	}{
		{"true", 0, "true"},
		{"false", 0, "false"},
		{"null", 0, "null"},
		{"true,", 0, "true"},
		{"trueblue", 0, ""},
		{"nullify", 0, ""},
		{"falsehood", 0, ""},
		{"nothing", 0, ""},
	}
	for _, tc := range tests {
		got := keywordAt([]byte(tc.input), tc.i)
		if got != tc.want {
			t.Errorf("keywordAt(%q, %d) = %q, want %q", tc.input, tc.i, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// makeLargeJSON builds a JSON object with n key-value pairs (~30 bytes each).
func makeLargeJSON(n int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"key_%04d":"value_%04d"`, i, i)
	}
	b.WriteByte('}')
	return b.String()
}

// makeProse generates n words of filler text (no JSON start bytes at word boundaries).
func makeProse(n int) string {
	words := []string{
		"Lorem", "ipsum", "dolor", "sit", "amet", "consectetur",
		"adipiscing", "elit", "sed", "do", "eiusmod", "tempor",
		"incididunt", "ut", "labore", "et", "dolore", "magna", "aliqua",
	}
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[i%len(words)])
	}
	return b.String()
}

// BenchmarkExtractJSON_PlainSmall: small JSON object, no noise.
func BenchmarkExtractJSON_PlainSmall(b *testing.B) {
	input := `{"name":"alice","age":30}`
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_PlainLarge: ~500-field JSON object, no noise.
func BenchmarkExtractJSON_PlainLarge(b *testing.B) {
	input := makeLargeJSON(500)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_CodeFence: JSON inside ```json code fence with surrounding prose.
func BenchmarkExtractJSON_CodeFence(b *testing.B) {
	json := makeLargeJSON(100)
	input := "Here is the structured output:\n```json\n" + json + "\n```\nLet me know if you need anything else."
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_ShortProse: short prose prefix (~20 words) before JSON.
func BenchmarkExtractJSON_ShortProse(b *testing.B) {
	input := makeProse(20) + "\n" + `{"result":"ok"}` + "\n" + makeProse(10)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_LongProse: heavy prose prefix (~2000 words) before JSON.
// Exercises the worst-case scanning path.
func BenchmarkExtractJSON_LongProse(b *testing.B) {
	input := makeProse(2000) + "\n" + `{"result":"ok"}` + "\n" + makeProse(500)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_NoJSON: no parseable JSON at all — worst case scan.
func BenchmarkExtractJSON_NoJSON(b *testing.B) {
	input := makeProse(2000)
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_KeywordHeavyProse: prose full of t/f/n starting words
// to stress the keyword filtering optimization.
func BenchmarkExtractJSON_KeywordHeavyProse(b *testing.B) {
	words := []string{
		"the", "following", "note", "that", "this", "format",
		"needs", "to", "be", "fixed", "then", "try", "not",
	}
	var sb strings.Builder
	for i := range 500 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(words[i%len(words)])
	}
	sb.WriteString("\n")
	sb.WriteString(`{"found":true}`)
	input := sb.String()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_QuoteHeavyProse: prose with many quoted words before a JSON object.
func BenchmarkExtractJSON_QuoteHeavyProse(b *testing.B) {
	var sb strings.Builder
	for i := range 200 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, `use "word_%d" here`, i)
	}
	sb.WriteString("\n")
	sb.WriteString(`{"result":"ok"}`)
	input := sb.String()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_NumberHeavyProse: prose with many standalone numbers before a JSON object.
func BenchmarkExtractJSON_NumberHeavyProse(b *testing.B) {
	var sb strings.Builder
	for i := range 200 {
		fmt.Fprintf(&sb, "Step %d: do something. ", i)
	}
	sb.WriteString("\n")
	sb.WriteString(`{"result":"ok"}`)
	input := sb.String()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_DashHeavyProse: prose with many dash-prefixed list items before JSON.
func BenchmarkExtractJSON_DashHeavyProse(b *testing.B) {
	var sb strings.Builder
	for i := range 200 {
		fmt.Fprintf(&sb, "- item %d\n", i)
	}
	sb.WriteString(`{"result":"ok"}`)
	input := sb.String()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}

// BenchmarkExtractJSON_ScalarOnly: input with only a scalar value, no structured data.
func BenchmarkExtractJSON_ScalarOnly(b *testing.B) {
	input := makeProse(100) + " true"
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		_, _, _ = ExtractJSON(input)
	}
}
