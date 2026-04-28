package bindings

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// ---------------------------------------------------------------------------
// partToMap — one case per PartType + a defensive "unknown" branch
// ---------------------------------------------------------------------------

func TestPartToMap_Text(t *testing.T) {
	got := partToMap(model.Part{Type: model.PartText, Text: "hello"})
	want := map[string]any{"type": "text", "text": "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestPartToMap_Text_EmptyOmitted(t *testing.T) {
	got := partToMap(model.Part{Type: model.PartText})
	if _, has := got["text"]; has {
		t.Fatalf("empty text should be omitted, got %#v", got)
	}
	if got["type"] != "text" {
		t.Fatalf("type missing or wrong: %#v", got)
	}
}

func TestPartToMap_Image(t *testing.T) {
	p := model.Part{
		Type:  model.PartImage,
		Image: &model.MediaRef{URL: "https://x/y.png", MediaType: "image/png"},
	}
	got := partToMap(p)
	if got["type"] != "image" {
		t.Fatalf("type = %v", got["type"])
	}
	img, ok := got["image"].(map[string]any)
	if !ok {
		t.Fatalf("image missing or wrong type: %#v", got)
	}
	if img["url"] != "https://x/y.png" || img["media_type"] != "image/png" {
		t.Fatalf("image fields: %#v", img)
	}
	if _, has := img["base64"]; has {
		t.Errorf("empty base64 should be omitted: %#v", img)
	}
}

func TestPartToMap_Image_NilPointer(t *testing.T) {
	got := partToMap(model.Part{Type: model.PartImage})
	if got["type"] != "image" {
		t.Fatalf("type = %v", got["type"])
	}
	if _, has := got["image"]; has {
		t.Errorf("nil Image should not produce an image key, got %#v", got)
	}
}

func TestPartToMap_Audio(t *testing.T) {
	p := model.Part{
		Type:  model.PartAudio,
		Audio: &model.MediaRef{Base64: "AAAA", MediaType: "audio/mpeg"},
	}
	got := partToMap(p)
	audio, ok := got["audio"].(map[string]any)
	if !ok {
		t.Fatalf("audio missing: %#v", got)
	}
	if audio["base64"] != "AAAA" || audio["media_type"] != "audio/mpeg" {
		t.Fatalf("audio fields: %#v", audio)
	}
}

func TestPartToMap_File(t *testing.T) {
	p := model.Part{
		Type: model.PartFile,
		File: &model.FileRef{URI: "s3://b/k.pdf", MimeType: "application/pdf", Name: "spec.pdf"},
	}
	got := partToMap(p)
	f, ok := got["file"].(map[string]any)
	if !ok {
		t.Fatalf("file missing: %#v", got)
	}
	if f["uri"] != "s3://b/k.pdf" || f["mime_type"] != "application/pdf" || f["name"] != "spec.pdf" {
		t.Fatalf("file fields: %#v", f)
	}
}

func TestPartToMap_File_OnlyURI(t *testing.T) {
	got := partToMap(model.Part{
		Type: model.PartFile,
		File: &model.FileRef{URI: "file:///tmp/a"},
	})
	f := got["file"].(map[string]any)
	if f["uri"] != "file:///tmp/a" {
		t.Fatalf("uri lost: %#v", f)
	}
	if _, has := f["mime_type"]; has {
		t.Errorf("empty mime_type should be omitted: %#v", f)
	}
	if _, has := f["name"]; has {
		t.Errorf("empty name should be omitted: %#v", f)
	}
}

func TestPartToMap_Data(t *testing.T) {
	p := model.Part{
		Type: model.PartData,
		Data: &model.DataRef{
			MimeType: "application/vnd.example+json",
			Value:    map[string]any{"k": 1, "nested": map[string]any{"x": "y"}},
		},
	}
	got := partToMap(p)
	d, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing: %#v", got)
	}
	if d["mime_type"] != "application/vnd.example+json" {
		t.Errorf("mime_type lost: %#v", d)
	}
	v, ok := d["value"].(map[string]any)
	if !ok {
		t.Fatalf("value should pass through as map[string]any, got %T", d["value"])
	}
	// Pass-through, no copy: identity preserved (fast path the doc promises).
	if v["k"] != 1 {
		t.Errorf("value contents lost: %#v", v)
	}
}

func TestPartToMap_ToolCall(t *testing.T) {
	p := model.Part{
		Type:     model.PartToolCall,
		ToolCall: &model.ToolCall{ID: "c1", Name: "search", Arguments: `{"q":"go"}`},
	}
	got := partToMap(p)
	tc, ok := got["tool_call"].(map[string]any)
	if !ok {
		t.Fatalf("tool_call missing: %#v", got)
	}
	if tc["id"] != "c1" || tc["name"] != "search" || tc["arguments"] != `{"q":"go"}` {
		t.Fatalf("tool_call fields: %#v", tc)
	}
}

func TestPartToMap_ToolResult(t *testing.T) {
	p := model.Part{
		Type:       model.PartToolResult,
		ToolResult: &model.ToolResult{ToolCallID: "c1", Content: "ok", IsError: false},
	}
	got := partToMap(p)
	tr, ok := got["tool_result"].(map[string]any)
	if !ok {
		t.Fatalf("tool_result missing: %#v", got)
	}
	if tr["tool_call_id"] != "c1" || tr["content"] != "ok" {
		t.Fatalf("tool_result fields: %#v", tr)
	}
	// is_error is intentionally always present so scripts don't have to
	// guard `result.is_error ?? false` when reading.
	if _, has := tr["is_error"]; !has {
		t.Errorf("is_error must always be present: %#v", tr)
	}
	if tr["is_error"] != false {
		t.Errorf("is_error mismatch: %v", tr["is_error"])
	}
}

func TestPartToMap_UnknownType_StillProducesType(t *testing.T) {
	got := partToMap(model.Part{Type: model.PartType("future_thing")})
	if got["type"] != "future_thing" {
		t.Fatalf("type lost: %#v", got)
	}
	if len(got) != 1 {
		t.Errorf("unknown type should produce only {type}, got %#v", got)
	}
}

// ---------------------------------------------------------------------------
// partsToList — slice projection + nil semantics
// ---------------------------------------------------------------------------

func TestPartsToList_Nil(t *testing.T) {
	if got := partsToList(nil); got != nil {
		t.Fatalf("nil slice should stay nil, got %#v", got)
	}
}

func TestPartsToList_Empty(t *testing.T) {
	got := partsToList([]model.Part{})
	if got == nil {
		t.Fatal("non-nil empty slice should produce non-nil empty list")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %#v", got)
	}
}

func TestPartsToList_Mixed(t *testing.T) {
	in := []model.Part{
		{Type: model.PartText, Text: "hi"},
		{Type: model.PartImage, Image: &model.MediaRef{URL: "u"}},
	}
	got := partsToList(in)
	if len(got) != 2 || got[0]["type"] != "text" || got[1]["type"] != "image" {
		t.Fatalf("projection lost order or type: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// messageToMap — role + parts + content convenience
// ---------------------------------------------------------------------------

func TestMessageToMap_TextOnly(t *testing.T) {
	m := model.NewTextMessage(model.RoleAssistant, "hello")
	got := messageToMap(m)
	if got["role"] != "assistant" {
		t.Errorf("role = %v", got["role"])
	}
	if got["content"] != "hello" {
		t.Errorf("content convenience missing: %#v", got)
	}
	parts := got["parts"].([]map[string]any)
	if len(parts) != 1 || parts[0]["type"] != "text" {
		t.Errorf("parts projection lost: %#v", parts)
	}
}

func TestMessageToMap_NoTextParts_OmitsContent(t *testing.T) {
	m := model.Message{
		Role:  model.RoleAssistant,
		Parts: []model.Part{{Type: model.PartImage, Image: &model.MediaRef{URL: "u"}}},
	}
	got := messageToMap(m)
	if _, has := got["content"]; has {
		t.Errorf("content should be omitted when there is no text part: %#v", got)
	}
}

func TestMessageToMap_MultiTextParts_Concatenates(t *testing.T) {
	m := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartText, Text: "hello "},
			{Type: model.PartText, Text: "world"},
		},
	}
	got := messageToMap(m)
	if got["content"] != "hello world" {
		t.Errorf("content concat failed: %v", got["content"])
	}
}

// ---------------------------------------------------------------------------
// Tool / usage projections
// ---------------------------------------------------------------------------

func TestToolCallToMap_Fields(t *testing.T) {
	tc := model.ToolCall{ID: "c1", Name: "calc", Arguments: `{"a":1}`}
	got := toolCallToMap(tc)
	if got["id"] != "c1" || got["name"] != "calc" || got["arguments"] != `{"a":1}` {
		t.Fatalf("fields lost: %#v", got)
	}
}

func TestToolCallsToList_Nil(t *testing.T) {
	if got := toolCallsToList(nil); got != nil {
		t.Fatalf("nil should stay nil, got %#v", got)
	}
}

func TestToolResultToMap_AlwaysIncludesIsError(t *testing.T) {
	tr := model.ToolResult{ToolCallID: "c1", Content: "x"}
	got := toolResultToMap(tr)
	if got["is_error"] != false {
		t.Errorf("is_error default = %v", got["is_error"])
	}
	tr.IsError = true
	got = toolResultToMap(tr)
	if got["is_error"] != true {
		t.Errorf("is_error true lost: %v", got["is_error"])
	}
}

func TestUsageToMap_PreservesInt64(t *testing.T) {
	got := usageToMap(model.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	if got["input_tokens"] != int64(100) {
		t.Errorf("input_tokens type/value lost: %v (%T)", got["input_tokens"], got["input_tokens"])
	}
	if got["output_tokens"] != int64(50) {
		t.Errorf("output_tokens lost: %v (%T)", got["output_tokens"], got["output_tokens"])
	}
	if got["total_tokens"] != int64(150) {
		t.Errorf("total_tokens lost: %v (%T)", got["total_tokens"], got["total_tokens"])
	}
}

// ---------------------------------------------------------------------------
// MediaRef / FileRef / DataRef edge cases — sparse field omission
// ---------------------------------------------------------------------------

func TestMediaRefToMap_AllEmpty_StillReturnsMap(t *testing.T) {
	got := mediaRefToMap(model.MediaRef{})
	if got == nil {
		t.Fatal("should return non-nil empty map for visibility")
	}
	if len(got) != 0 {
		t.Errorf("empty MediaRef should produce empty map, got %#v", got)
	}
}

func TestDataRefToMap_PreservesValueIdentity(t *testing.T) {
	v := map[string]any{"a": 1}
	got := dataRefToMap(model.DataRef{Value: v})
	gotV, _ := got["value"].(map[string]any)
	// The doc promises pass-through (no per-key copy). Mutating v
	// must surface in got["value"] for the runtime to share state.
	v["b"] = 2
	if gotV["b"] != 2 {
		t.Errorf("Value should be passed through, mutation not visible: %#v", gotV)
	}
}

// ===========================================================================
// Reverse marshal: parseChannelMessages / parseMessage / parsePart / ...
// ===========================================================================
//
// Strategy: round-trip every PartType through messageToMap → parseMessage
// to confirm bijection. Then exercise the strict-validation paths (unknown
// fields, type mismatches, missing required fields) per shape.

func TestParseMessage_RoundTrip_TextOnly(t *testing.T) {
	original := model.NewTextMessage(model.RoleUser, "hello world")
	scriptShape := messageToMap(original)
	got, err := parseMessage(scriptShape, "test")
	if err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("not bijective:\n  got: %+v\n want: %+v", got, original)
	}
}

func TestParseMessage_RoundTrip_AllPartTypes(t *testing.T) {
	original := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartText, Text: "intro"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://x/a.png", MediaType: "image/png"}},
			{Type: model.PartAudio, Audio: &model.MediaRef{Base64: "AA==", MediaType: "audio/mpeg"}},
			{Type: model.PartFile, File: &model.FileRef{URI: "s3://b/k", MimeType: "application/pdf", Name: "doc.pdf"}},
			{Type: model.PartData, Data: &model.DataRef{MimeType: "application/json", Value: map[string]any{"k": "v"}}},
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: "search", Arguments: `{"q":"go"}`}},
			{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "c1", Content: "ok", IsError: false}},
		},
	}
	scriptShape := messageToMap(original)
	got, err := parseMessage(scriptShape, "rt")
	if err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}

	// Compare structurally — DeepEqual handles all *Ref and pointers.
	if got.Role != original.Role {
		t.Errorf("role: got %q want %q", got.Role, original.Role)
	}
	if len(got.Parts) != len(original.Parts) {
		t.Fatalf("part count mismatch: %d vs %d", len(got.Parts), len(original.Parts))
	}
	for i, want := range original.Parts {
		if !reflect.DeepEqual(got.Parts[i], want) {
			t.Errorf("part[%d] mismatch:\n  got: %+v\n want: %+v", i, got.Parts[i], want)
		}
	}
}

func TestParseMessage_RejectsNonObject(t *testing.T) {
	cases := []any{nil, "string", 42, []any{}}
	for _, in := range cases {
		_, err := parseMessage(in, "ctx")
		if err == nil {
			t.Fatalf("expected error for %T, got nil", in)
		}
		if !strings.Contains(err.Error(), "ctx") {
			t.Errorf("error should include context, got: %v", err)
		}
	}
}

func TestParseMessage_MissingRole(t *testing.T) {
	_, err := parseMessage(map[string]any{"parts": []any{}}, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"role"`) {
		t.Fatalf("expected missing-role error, got: %v", err)
	}
}

func TestParseMessage_MissingParts(t *testing.T) {
	_, err := parseMessage(map[string]any{"role": "user"}, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"parts"`) {
		t.Fatalf("expected missing-parts error, got: %v", err)
	}
}

func TestParseMessage_RejectsUnknownTopLevelKey(t *testing.T) {
	in := map[string]any{
		"role":    "user",
		"parts":   []any{},
		"unknown": "bad",
	}
	_, err := parseMessage(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"unknown"`) {
		t.Fatalf("expected unknown-key error naming the field, got: %v", err)
	}
}

func TestParseMessage_AcceptsContentKey_AsBijection(t *testing.T) {
	// messageToMap emits "content" as a convenience; parseMessage must
	// accept it on the way back so a script can naively round-trip.
	in := map[string]any{
		"role":    "assistant",
		"content": "hello", // ignored on input
		"parts": []any{
			map[string]any{"type": "text", "text": "hello"},
		},
	}
	got, err := parseMessage(in, "ctx")
	if err != nil {
		t.Fatalf("content key should be accepted, got: %v", err)
	}
	if got.Content() != "hello" {
		t.Errorf("parts content lost: %q", got.Content())
	}
}

func TestParsePart_UnknownType(t *testing.T) {
	in := map[string]any{"type": "future_thing"}
	_, err := parsePart(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), "future_thing") {
		t.Fatalf("expected unknown-type error naming the type, got: %v", err)
	}
}

func TestParsePart_TypeMismatch(t *testing.T) {
	cases := []map[string]any{
		{"type": "text", "text": 42},                                                                   // text wants string
		{"type": "image", "image": "url"},                                                              // image wants object
		{"type": "tool_call", "tool_call": map[string]any{"id": 7}},                                    // id wants string
		{"type": "tool_result", "tool_result": map[string]any{"tool_call_id": "x", "is_error": "yes"}}, // is_error wants bool
	}
	for i, in := range cases {
		_, err := parsePart(in, "ctx")
		if err == nil {
			t.Fatalf("case %d (%v): expected type error", i, in)
		}
	}
}

func TestParsePart_RejectsUnknownNestedKey(t *testing.T) {
	in := map[string]any{
		"type": "image",
		"image": map[string]any{
			"url":          "https://x",
			"unknown_attr": "bad",
		},
	}
	_, err := parsePart(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), "unknown_attr") {
		t.Fatalf("expected unknown nested key error, got: %v", err)
	}
}

func TestParseFileRef_MissingURI(t *testing.T) {
	in := map[string]any{
		"type": "file",
		"file": map[string]any{"name": "x.pdf"}, // no uri
	}
	_, err := parsePart(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"uri"`) {
		t.Fatalf("expected missing-uri error, got: %v", err)
	}
}

func TestParseToolCall_MissingFields(t *testing.T) {
	cases := []struct {
		tc      map[string]any
		missing string
	}{
		{map[string]any{"name": "x"}, `"id"`},
		{map[string]any{"id": "x"}, `"name"`},
	}
	for _, c := range cases {
		in := map[string]any{"type": "tool_call", "tool_call": c.tc}
		_, err := parsePart(in, "ctx")
		if err == nil || !strings.Contains(err.Error(), c.missing) {
			t.Fatalf("expected error mentioning %s, got: %v", c.missing, err)
		}
	}
}

func TestParseToolResult_MissingToolCallID(t *testing.T) {
	in := map[string]any{
		"type":        "tool_result",
		"tool_result": map[string]any{"content": "ok"},
	}
	_, err := parsePart(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"tool_call_id"`) {
		t.Fatalf("expected missing tool_call_id error, got: %v", err)
	}
}

func TestParseDataRef_MissingValue(t *testing.T) {
	in := map[string]any{
		"type": "data",
		"data": map[string]any{"mime_type": "application/json"}, // no value
	}
	_, err := parsePart(in, "ctx")
	if err == nil || !strings.Contains(err.Error(), `"value"`) {
		t.Fatalf("expected missing-value error, got: %v", err)
	}
}

func TestParseChannelMessages_NilAndEmpty(t *testing.T) {
	got, err := parseChannelMessages(nil, "setChannel")
	if err != nil {
		t.Fatalf("nil input should be accepted, got: %v", err)
	}
	if got != nil {
		t.Errorf("nil should produce nil slice, got %#v", got)
	}

	got, err = parseChannelMessages([]any{}, "setChannel")
	if err != nil {
		t.Fatalf("empty input should be accepted, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty should produce empty slice, got %#v", got)
	}
}

func TestParseChannelMessages_PathPrefix_PointsToBadIndex(t *testing.T) {
	in := []any{
		map[string]any{"role": "user", "parts": []any{}},
		map[string]any{"role": "assistant"}, // missing parts — error here
	}
	_, err := parseChannelMessages(in, "setChannel")
	if err == nil {
		t.Fatal("expected error from messages[1]")
	}
	if !strings.Contains(err.Error(), "messages[1]") {
		t.Errorf("error should point to messages[1], got: %v", err)
	}
}

func TestParseChannelMessages_NonArray(t *testing.T) {
	_, err := parseChannelMessages("not-an-array", "setChannel")
	if err == nil || !strings.Contains(err.Error(), "expected array") {
		t.Fatalf("expected array-required error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// roundResultToMap — composite projection
// ---------------------------------------------------------------------------

func TestRoundResultToMap_TextOnly_OmitsOptionalCollections(t *testing.T) {
	r := &roundResult{
		Content:  "hello",
		Message:  model.NewTextMessage(model.RoleAssistant, "hello"),
		Messages: []model.Message{model.NewTextMessage(model.RoleAssistant, "hello")},
		Usage:    model.TokenUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
	}
	got := roundResultToMap(r)

	if got["content"] != "hello" {
		t.Errorf("content = %v", got["content"])
	}
	if got["tool_pending"] != false {
		t.Errorf("tool_pending = %v", got["tool_pending"])
	}
	if _, has := got["tool_calls"]; has {
		t.Errorf("tool_calls should be omitted when empty: %#v", got)
	}
	if _, has := got["tool_results"]; has {
		t.Errorf("tool_results should be omitted when empty: %#v", got)
	}
	if _, ok := got["message"].(map[string]any); !ok {
		t.Errorf("message should be a map: %#v", got["message"])
	}
	if _, ok := got["messages"].([]map[string]any); !ok {
		t.Errorf("messages should be a list: %T", got["messages"])
	}
	if _, ok := got["usage"].(map[string]any); !ok {
		t.Errorf("usage should be a map: %T", got["usage"])
	}
}

func TestRoundResultToMap_WithToolActivity_IncludesCollections(t *testing.T) {
	calls := []model.ToolCall{{ID: "c1", Name: "search"}}
	results := []model.ToolResult{{ToolCallID: "c1", Content: "ok"}}
	r := &roundResult{
		Content:     "",
		Message:     model.NewToolCallMessage(calls),
		Messages:    []model.Message{model.NewToolCallMessage(calls), model.NewToolResultMessage(results)},
		ToolCalls:   calls,
		ToolResults: results,
		ToolPending: true,
		Usage:       model.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	got := roundResultToMap(r)

	if got["tool_pending"] != true {
		t.Errorf("tool_pending = %v", got["tool_pending"])
	}
	tcs, ok := got["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 || tcs[0]["id"] != "c1" {
		t.Errorf("tool_calls projection lost: %#v", got["tool_calls"])
	}
	trs, ok := got["tool_results"].([]map[string]any)
	if !ok || len(trs) != 1 || trs[0]["tool_call_id"] != "c1" {
		t.Errorf("tool_results projection lost: %#v", got["tool_results"])
	}
}

// ---------------------------------------------------------------------------
// optional* error paths — table-driven coverage of the validation branches
// each helper shares (wrong-type root, unknown sub-key, missing required
// sub-field, sub-field type mismatch). Existing tests above cover the
// happy paths; this batch nails the negative branches that pulled the
// per-function coverage below 80%.
// ---------------------------------------------------------------------------

func TestOptional_ErrorPaths(t *testing.T) {
	type call func(map[string]any) error
	mk := func(name string, c call) (string, call) { return name, c }

	cases := []struct {
		name      string
		invoke    call
		input     map[string]any
		wantInErr string
	}{
		// ----- optionalMediaRef ----------------------------------------
		{"mediaRef wrong root type", func(m map[string]any) error {
			_, e := optionalMediaRef(m, "ref", "ctx")
			return e
		}, map[string]any{"ref": "not-a-map"}, "ctx.ref"},
		{"mediaRef unknown sub-field", func(m map[string]any) error {
			_, e := optionalMediaRef(m, "ref", "ctx")
			return e
		}, map[string]any{"ref": map[string]any{"url": "x", "bogus": 1}}, "bogus"},
		{"mediaRef wrong url type", func(m map[string]any) error {
			_, e := optionalMediaRef(m, "ref", "ctx")
			return e
		}, map[string]any{"ref": map[string]any{"url": 42}}, "url"},
		{"mediaRef wrong base64 type", func(m map[string]any) error {
			_, e := optionalMediaRef(m, "ref", "ctx")
			return e
		}, map[string]any{"ref": map[string]any{"base64": 42}}, "base64"},
		{"mediaRef wrong media_type type", func(m map[string]any) error {
			_, e := optionalMediaRef(m, "ref", "ctx")
			return e
		}, map[string]any{"ref": map[string]any{"media_type": 42}}, "media_type"},

		// ----- optionalFileRef (uri required) --------------------------
		{"fileRef missing required uri", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": map[string]any{"name": "x"}}, "uri"},
		{"fileRef uri wrong type", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": map[string]any{"uri": 42}}, "uri"},
		{"fileRef wrong root type", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": 7}, "ctx.f"},
		{"fileRef unknown sub-field", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": map[string]any{"uri": "x", "bogus": 1}}, "bogus"},
		{"fileRef wrong mime_type type", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": map[string]any{"uri": "x", "mime_type": 1}}, "mime_type"},
		{"fileRef wrong name type", func(m map[string]any) error {
			_, e := optionalFileRef(m, "f", "ctx")
			return e
		}, map[string]any{"f": map[string]any{"uri": "x", "name": 1}}, "name"},

		// ----- optionalDataRef (value required, value must be map) -----
		{"dataRef missing required value", func(m map[string]any) error {
			_, e := optionalDataRef(m, "d", "ctx")
			return e
		}, map[string]any{"d": map[string]any{"mime_type": "x"}}, "value"},
		{"dataRef value wrong type", func(m map[string]any) error {
			_, e := optionalDataRef(m, "d", "ctx")
			return e
		}, map[string]any{"d": map[string]any{"value": "not-a-map"}}, "value"},
		{"dataRef wrong root type", func(m map[string]any) error {
			_, e := optionalDataRef(m, "d", "ctx")
			return e
		}, map[string]any{"d": []any{}}, "ctx.d"},
		{"dataRef unknown sub-field", func(m map[string]any) error {
			_, e := optionalDataRef(m, "d", "ctx")
			return e
		}, map[string]any{"d": map[string]any{"value": map[string]any{}, "bogus": 1}}, "bogus"},
		{"dataRef wrong mime_type type", func(m map[string]any) error {
			_, e := optionalDataRef(m, "d", "ctx")
			return e
		}, map[string]any{"d": map[string]any{"mime_type": 1, "value": map[string]any{}}}, "mime_type"},

		// ----- optionalToolCall (id + name required) -------------------
		{"toolCall missing required id", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"name": "x"}}, "id"},
		{"toolCall id wrong type", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"id": 1, "name": "x"}}, "id"},
		{"toolCall missing required name", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"id": "x"}}, "name"},
		{"toolCall name wrong type", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"id": "x", "name": 1}}, "name"},
		{"toolCall unknown sub-field", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"id": "x", "name": "y", "bogus": 1}}, "bogus"},
		{"toolCall wrong root type", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": "not-a-map"}, "ctx.tc"},
		{"toolCall arguments wrong type", func(m map[string]any) error {
			_, e := optionalToolCall(m, "tc", "ctx")
			return e
		}, map[string]any{"tc": map[string]any{"id": "x", "name": "y", "arguments": 1}}, "arguments"},

		// ----- optionalToolResult (tool_call_id required, is_error must be bool) -----
		{"toolResult missing required tool_call_id", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": map[string]any{"content": "x"}}, "tool_call_id"},
		{"toolResult tool_call_id wrong type", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": map[string]any{"tool_call_id": 1}}, "tool_call_id"},
		{"toolResult wrong root type", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": "not-a-map"}, "ctx.tr"},
		{"toolResult unknown sub-field", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": map[string]any{"tool_call_id": "x", "bogus": 1}}, "bogus"},
		{"toolResult content wrong type", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": map[string]any{"tool_call_id": "x", "content": 1}}, "content"},
		{"toolResult is_error wrong type", func(m map[string]any) error {
			_, e := optionalToolResult(m, "tr", "ctx")
			return e
		}, map[string]any{"tr": map[string]any{"tool_call_id": "x", "is_error": "true"}}, "is_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.invoke(tc.input)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantInErr)
			}
		})
	}

	// Absent-key contract: every helper returns (nil, nil) when the field
	// is simply not present. Spot-check all five helpers — schema evolution
	// must never accidentally turn an absent optional into a hard error.
	t.Run("absent key returns nil", func(t *testing.T) {
		empty := map[string]any{}
		if v, e := optionalMediaRef(empty, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalMediaRef: got (%v, %v)", v, e)
		}
		if v, e := optionalFileRef(empty, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalFileRef: got (%v, %v)", v, e)
		}
		if v, e := optionalDataRef(empty, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalDataRef: got (%v, %v)", v, e)
		}
		if v, e := optionalToolCall(empty, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalToolCall: got (%v, %v)", v, e)
		}
		if v, e := optionalToolResult(empty, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalToolResult: got (%v, %v)", v, e)
		}
	})

	// Also exercise the explicit-nil branch (key present, value == nil) —
	// distinct from absent-key under JS/Lua semantics where `null` is a
	// real value but should map to "not provided" for these refs.
	t.Run("explicit nil returns nil", func(t *testing.T) {
		nilv := map[string]any{"k": nil}
		if v, e := optionalMediaRef(nilv, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalMediaRef: got (%v, %v)", v, e)
		}
		if v, e := optionalFileRef(nilv, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalFileRef: got (%v, %v)", v, e)
		}
		if v, e := optionalDataRef(nilv, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalDataRef: got (%v, %v)", v, e)
		}
		if v, e := optionalToolCall(nilv, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalToolCall: got (%v, %v)", v, e)
		}
		if v, e := optionalToolResult(nilv, "k", "ctx"); v != nil || e != nil {
			t.Errorf("optionalToolResult: got (%v, %v)", v, e)
		}
	})

	_ = mk // silence helper introducer
}

// TestOptional_HappyPaths exercises the success branches of all five
// optional* helpers — including all-fields-present forms — so that future
// refactors (e.g. swapping the underlying decoder) cannot quietly break the
// "round trip from script-shaped map" contract that every parsePart case
// depends on.
func TestOptional_HappyPaths(t *testing.T) {
	t.Run("mediaRef url+base64+media_type", func(t *testing.T) {
		got, err := optionalMediaRef(map[string]any{
			"k": map[string]any{"url": "u", "base64": "b", "media_type": "image/png"},
		}, "k", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.URL != "u" || got.Base64 != "b" || got.MediaType != "image/png" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("fileRef uri+mime_type+name", func(t *testing.T) {
		got, err := optionalFileRef(map[string]any{
			"k": map[string]any{"uri": "ws://x", "mime_type": "text/plain", "name": "a.txt"},
		}, "k", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.URI != "ws://x" || got.MimeType != "text/plain" || got.Name != "a.txt" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("dataRef mime_type+value", func(t *testing.T) {
		got, err := optionalDataRef(map[string]any{
			"k": map[string]any{
				"mime_type": "application/json",
				"value":     map[string]any{"x": 1.0, "y": "z"},
			},
		}, "k", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.MimeType != "application/json" {
			t.Fatalf("mime_type lost: %#v", got)
		}
		if got.Value["x"] != 1.0 || got.Value["y"] != "z" {
			t.Fatalf("value contents lost: %#v", got.Value)
		}
	})

	t.Run("toolCall id+name+arguments", func(t *testing.T) {
		got, err := optionalToolCall(map[string]any{
			"k": map[string]any{"id": "c1", "name": "search", "arguments": `{"q":"x"}`},
		}, "k", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.ID != "c1" || got.Name != "search" || got.Arguments != `{"q":"x"}` {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("toolResult full incl. is_error true", func(t *testing.T) {
		got, err := optionalToolResult(map[string]any{
			"k": map[string]any{"tool_call_id": "c1", "content": "boom", "is_error": true},
		}, "k", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.ToolCallID != "c1" || got.Content != "boom" || !got.IsError {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("toolResult is_error explicit nil treated as false", func(t *testing.T) {
		// Confirms the `v != nil` guard inside optionalToolResult — script
		// passing `{ ..., is_error: null }` must not throw.
		got, err := optionalToolResult(map[string]any{
			"k": map[string]any{"tool_call_id": "c1", "is_error": nil},
		}, "k", "ctx")
		if err != nil {
			t.Fatalf("explicit-nil is_error should not error: %v", err)
		}
		if got == nil || got.IsError {
			t.Fatalf("is_error should default to false, got %#v", got)
		}
	})
}

// TestParsePart_AllPartTypes_HappyPath drives every PartType branch of
// parsePart through a successful conversion. Required because parsePart is
// the one entry point scripts hit when sending multimodal messages back to
// the bridge; unbalanced coverage there means a script-side bug in a less
// common modality (e.g. audio) would slip past CI.
func TestParsePart_AllPartTypes_HappyPath(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want model.PartType
		// extra is an optional assertion that runs after type matching.
		extra func(*testing.T, model.Part)
	}{
		{
			name: "text", want: model.PartText,
			in: map[string]any{"type": "text", "text": "hi"},
			extra: func(t *testing.T, p model.Part) {
				if p.Text != "hi" {
					t.Errorf("text lost: %q", p.Text)
				}
			},
		},
		{
			name: "image", want: model.PartImage,
			in: map[string]any{"type": "image", "image": map[string]any{"url": "u"}},
			extra: func(t *testing.T, p model.Part) {
				if p.Image == nil || p.Image.URL != "u" {
					t.Errorf("image ref lost: %#v", p.Image)
				}
			},
		},
		{
			name: "audio", want: model.PartAudio,
			in: map[string]any{"type": "audio", "audio": map[string]any{"url": "a.mp3"}},
			extra: func(t *testing.T, p model.Part) {
				if p.Audio == nil || p.Audio.URL != "a.mp3" {
					t.Errorf("audio ref lost: %#v", p.Audio)
				}
			},
		},
		{
			name: "file", want: model.PartFile,
			in: map[string]any{"type": "file", "file": map[string]any{"uri": "ws://f"}},
			extra: func(t *testing.T, p model.Part) {
				if p.File == nil || p.File.URI != "ws://f" {
					t.Errorf("file ref lost: %#v", p.File)
				}
			},
		},
		{
			name: "data", want: model.PartData,
			in: map[string]any{"type": "data", "data": map[string]any{
				"value": map[string]any{"k": "v"},
			}},
			extra: func(t *testing.T, p model.Part) {
				if p.Data == nil || p.Data.Value["k"] != "v" {
					t.Errorf("data ref lost: %#v", p.Data)
				}
			},
		},
		{
			name: "tool_call", want: model.PartToolCall,
			in: map[string]any{"type": "tool_call", "tool_call": map[string]any{
				"id": "c1", "name": "search",
			}},
			extra: func(t *testing.T, p model.Part) {
				if p.ToolCall == nil || p.ToolCall.Name != "search" {
					t.Errorf("tool_call lost: %#v", p.ToolCall)
				}
			},
		},
		{
			name: "tool_result", want: model.PartToolResult,
			in: map[string]any{"type": "tool_result", "tool_result": map[string]any{
				"tool_call_id": "c1", "content": "ok",
			}},
			extra: func(t *testing.T, p model.Part) {
				if p.ToolResult == nil || p.ToolResult.Content != "ok" {
					t.Errorf("tool_result lost: %#v", p.ToolResult)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePart(tc.in, "ctx")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Type != tc.want {
				t.Fatalf("type mismatch: got %q want %q", got.Type, tc.want)
			}
			if tc.extra != nil {
				tc.extra(t, got)
			}
		})
	}
}

func TestParsePart_RejectsNonObjectPart(t *testing.T) {
	// parsePart's first guard (asStringMap) rejects parts that aren't maps —
	// e.g. a script accidentally appending a bare string to its parts list.
	// Without this branch, type assertion in the switch below would panic
	// or silently zero-fill the part.
	_, err := parsePart("not-a-map", "ctx.parts[0]")
	if err == nil {
		t.Fatal("expected error for non-object part")
	}
	if !strings.Contains(err.Error(), "ctx.parts[0]") {
		t.Fatalf("error should be path-prefixed: %v", err)
	}
}

func TestParsePart_MissingType(t *testing.T) {
	// type is required — confirms the requiredString branch (line 318-321)
	// rather than relying on the empty-string default falling into the
	// switch's default case (which would also error but with a less
	// helpful message).
	_, err := parsePart(map[string]any{"text": "hi"}, "ctx")
	if err == nil {
		t.Fatal("expected error for missing type")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Fatalf("error should name 'type': %v", err)
	}
}

func TestParseMessage_PartsNotAnArray(t *testing.T) {
	// parts must be a list — confirms parseMessage propagates asAnyList's
	// error path (line 295-297) instead of silently producing an empty
	// message that scripts could mistake for "no error".
	_, err := parseMessage(map[string]any{
		"role":  "user",
		"parts": "not-a-list",
	}, "ctx")
	if err == nil {
		t.Fatal("expected error when parts is not an array")
	}
	if !strings.Contains(err.Error(), "ctx.parts") {
		t.Fatalf("error should be path-prefixed: %v", err)
	}
}

func TestParseMessage_PropagatesParsePartError(t *testing.T) {
	// A bad sub-part inside parts[] must surface to the caller — confirms
	// the parsePart error propagation in parseMessage (line 301-304) and
	// that the error path includes the offending index for debuggability.
	_, err := parseMessage(map[string]any{
		"role": "user",
		"parts": []any{
			map[string]any{"type": "text", "text": "ok"},
			map[string]any{"type": "text", "bogus": 1}, // index 1: unknown field
		},
	}, "ctx")
	if err == nil {
		t.Fatal("expected error from bad sub-part")
	}
	if !strings.Contains(err.Error(), "parts[1]") {
		t.Fatalf("error should include offending index, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error should name the bad field: %v", err)
	}
}

// TestParsePart_RejectsUnknownFieldPerType walks every PartType once with a
// "bogus" extra field, exercising the rejectUnknownKeys branch inside each
// case arm of parsePart. parsePart is the script-facing gatekeeper; if a
// new modality is ever added without updating its allowed-keys set, this
// test catches it before scripts can smuggle data through.
func TestParsePart_RejectsUnknownFieldPerType(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
	}{
		{"text", map[string]any{"type": "text", "text": "x", "bogus": 1}},
		{"image", map[string]any{"type": "image", "bogus": 1}},
		{"audio", map[string]any{"type": "audio", "bogus": 1}},
		{"file", map[string]any{"type": "file", "bogus": 1}},
		{"data", map[string]any{"type": "data", "bogus": 1}},
		{"tool_call", map[string]any{"type": "tool_call", "bogus": 1}},
		{"tool_result", map[string]any{"type": "tool_result", "bogus": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePart(tc.in, "ctx")
			if err == nil {
				t.Fatalf("expected rejection of unknown field for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "bogus") {
				t.Fatalf("error should name the bogus field: %v", err)
			}
		})
	}
}
