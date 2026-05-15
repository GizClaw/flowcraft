// Convert is silent infrastructure: every locomo eval downstream consumes
// its output, so a bug in the converter (e.g. surfacing stale image
// metadata, dropping a date-time prefix, mis-mapping speaker→role)
// regresses every benchmark run without anyone noticing until the qa.judge
// number stops matching reality. These tests pin the contract for the
// convert pipeline so any future change has to update its expectations
// deliberately.
package locomo

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestImageAnnotation pins the gate that produced a 316-turn
// false-positive on locomo10 in May 2026: the upstream LoCoMo schema
// emits orphan query / blip_caption fields on turns that no longer
// reference an actual shared image, and surfacing those would inject
// stale visual hints ("a photo of a book shelf with many books") into
// turns where the speaker only said "Congrats!". The gate must be
// img_url presence — not query or blip_caption presence.
func TestImageAnnotation(t *testing.T) {
	cases := []struct {
		name string
		turn convRawTurn
		want string
	}{
		{
			name: "img_url+query prefers query",
			turn: convRawTurn{
				ImgURL:      []string{"https://example/a.jpg"},
				Query:       "banff national park rocky mountains snow",
				BlipCaption: "a photo of a person on skis on a snowy trail",
			},
			want: "[shared image: banff national park rocky mountains snow]",
		},
		{
			name: "img_url+blip only falls back to blip",
			turn: convRawTurn{
				ImgURL:      []string{"https://example/b.jpg"},
				BlipCaption: "a photo of a chocolate tart with raspberries on top",
			},
			want: "[shared image: a photo of a chocolate tart with raspberries on top]",
		},
		{
			name: "query without img_url is NOT surfaced (stale-annotation guard)",
			turn: convRawTurn{
				Query: "becoming nicole book amy ellis nutt",
			},
			want: "",
		},
		{
			name: "blip without img_url is NOT surfaced (stale-annotation guard)",
			turn: convRawTurn{
				BlipCaption: "a photo of a book shelf with many books on it",
			},
			want: "",
		},
		{
			name: "img_url with empty hints emits nothing (no annotation noise)",
			turn: convRawTurn{
				ImgURL: []string{"https://example/c.jpg"},
			},
			want: "",
		},
		{
			name: "empty turn → empty annotation",
			turn: convRawTurn{},
			want: "",
		},
		{
			name: "whitespace-only query falls through to blip",
			turn: convRawTurn{
				ImgURL:      []string{"x"},
				Query:       "   \n  ",
				BlipCaption: "fallback caption",
			},
			want: "[shared image: fallback caption]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.turn.imageAnnotation()
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConvFlattenSessions pins the contract for the per-conversation
// turn flattening: sessions sorted by index, speaker → role mapping,
// date_time prefix injected, image annotation appended inline, and
// turns with empty text + no image are dropped.
func TestConvFlattenSessions(t *testing.T) {
	// Build a minimal upstream conversation. Use json.RawMessage so the
	// fixture's shape matches what json.Unmarshal hands the converter.
	raw := map[string]json.RawMessage{
		"speaker_a":           json.RawMessage(`"Alice"`),
		"speaker_b":           json.RawMessage(`"Bob"`),
		"session_2_date_time": json.RawMessage(`"3:00 pm on 10 May, 2024"`),
		"session_1_date_time": json.RawMessage(`"9:00 am on 7 May, 2024"`),
		"session_1": json.RawMessage(`[
			{"speaker":"Alice","dia_id":"D1:1","text":"Hello Bob!"},
			{"speaker":"Bob","dia_id":"D1:2","text":"Hi Alice."},
			{"speaker":"Bob","dia_id":"D1:3","text":"","query":"orphan caption no image"}
		]`),
		"session_2": json.RawMessage(`[
			{"speaker":"Alice","dia_id":"D2:1","text":"Here's my new bowl",
			 "img_url":["http://x/y.jpg"],
			 "query":"hand-painted ceramic bowl",
			 "blip_caption":"a photo of a bowl on a table"}
		]`),
	}

	turns := convFlattenSessions(raw, "Alice", "Bob")

	want := []convOutConvTurn{
		{Role: "user", Content: "[9:00 am on 7 May, 2024] Alice: Hello Bob!", EvidenceID: "D1:1", SessionID: "session_1"},
		{Role: "assistant", Content: "[9:00 am on 7 May, 2024] Bob: Hi Alice.", EvidenceID: "D1:2", SessionID: "session_1"},
		// session_1 turn 3: empty text + no img_url → DROPPED (query alone is not enough)
		{Role: "user", Content: "[3:00 pm on 10 May, 2024] Alice: Here's my new bowl [shared image: hand-painted ceramic bowl]", EvidenceID: "D2:1", SessionID: "session_2"},
	}

	if !reflect.DeepEqual(turns, want) {
		t.Errorf("mismatch:\n got = %#v\nwant = %#v", turns, want)
	}
}

// TestConvFlattenSessionsOrdering guards the numeric sort over
// session keys. A naïve lexicographic sort would order "session_10"
// before "session_2", silently scrambling LoCoMo conversations that
// span more than nine sessions (which all 10 conv-* samples do).
func TestConvFlattenSessionsOrdering(t *testing.T) {
	raw := map[string]json.RawMessage{
		"speaker_a":            json.RawMessage(`"Alice"`),
		"speaker_b":            json.RawMessage(`"Bob"`),
		"session_1_date_time":  json.RawMessage(`"t1"`),
		"session_2_date_time":  json.RawMessage(`"t2"`),
		"session_10_date_time": json.RawMessage(`"t10"`),
		"session_1":            json.RawMessage(`[{"speaker":"Alice","dia_id":"A","text":"first"}]`),
		"session_2":            json.RawMessage(`[{"speaker":"Alice","dia_id":"B","text":"second"}]`),
		"session_10":           json.RawMessage(`[{"speaker":"Alice","dia_id":"C","text":"tenth"}]`),
	}
	turns := convFlattenSessions(raw, "Alice", "Bob")
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d", len(turns))
	}
	if turns[0].EvidenceID != "A" || turns[1].EvidenceID != "B" || turns[2].EvidenceID != "C" {
		t.Errorf("session ordering wrong: got %v %v %v, want A B C",
			turns[0].EvidenceID, turns[1].EvidenceID, turns[2].EvidenceID)
	}
	// And the date-time prefix must follow the same order, not lex order
	if !strings.Contains(turns[2].Content, "t10") {
		t.Errorf("session_10 missing its t10 prefix: %q", turns[2].Content)
	}
}

// TestConvAnswerToStrings pins answer normalization for every shape the
// upstream qa.answer field takes (string / int / float / bool / list).
// Locomo10 currently includes 6 integer answers ("2022", "3") that must
// not round-trip through float string ("2022.000000").
func TestConvAnswerToStrings(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"string", "Sweden", []string{"Sweden"}},
		{"string trims", "  hello  ", []string{"hello"}},
		{"empty string drops", "", nil},
		{"int as float (json default)", float64(2022), []string{"2022"}},
		{"small int", float64(3), []string{"3"}},
		{"float", float64(3.14), []string{"3.14"}},
		{"bool true", true, []string{"true"}},
		{"list of strings", []any{"a", "b"}, []string{"a", "b"}},
		{"list mixed", []any{"x", float64(7)}, []string{"x", "7"}},
		{"nil → nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convAnswerToStrings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
