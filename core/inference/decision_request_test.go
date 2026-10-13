package inference

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// decisionRequestFixture is one valid request carrying all three question
// kinds, so each validation case can mutate exactly one rule.
func decisionRequestFixture() DecisionRequest {
	return DecisionRequest{
		Input: DecisionInput{Text: "the evidence"},
		Questions: []DecisionQuestion{
			{
				Name:         "is_true",
				Kind:         DecisionQuestionPredicate,
				Instructions: "is the statement true?",
			},
			{
				Name:         "which",
				Kind:         DecisionQuestionChoice,
				Instructions: "which option applies?",
				Choices: []DecisionChoice{
					{Value: NewDecisionStringChoice("yes")},
					{Value: NewDecisionBooleanChoice(true)},
				},
			},
			{
				Name:         "grade",
				Kind:         DecisionQuestionScore,
				Instructions: "how good is it?",
				Levels: []DecisionLevel{
					{Label: "low"},
					{Label: "high"},
				},
			},
		},
	}
}

func TestDecisionRequestValidate(t *testing.T) {
	fixture := decisionRequestFixture()
	if err := fixture.Validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*DecisionRequest)
		want   string
	}{
		{
			name:   "no input",
			mutate: func(r *DecisionRequest) { r.Input = DecisionInput{} },
			want:   "decision input is required",
		},
		{
			name: "two carriers",
			mutate: func(r *DecisionRequest) {
				r.Input = DecisionInput{
					Text: "evidence",
					JSON: json.RawMessage(`{"a":1}`),
				}
			},
			want: "exactly one of text, json, or parts",
		},
		{
			name: "json scalar",
			mutate: func(r *DecisionRequest) {
				r.Input = DecisionInput{JSON: json.RawMessage(`7`)}
			},
			want: "must be an object or array",
		},
		{
			name: "json broken",
			mutate: func(r *DecisionRequest) {
				r.Input = DecisionInput{JSON: json.RawMessage(`{"a":`)}
			},
			want: "not valid JSON",
		},
		{
			name: "unsupported part",
			mutate: func(r *DecisionRequest) {
				r.Input = DecisionInput{Parts: []message.Part{message.AudioPart{}}}
			},
			want: "audio evidence is not supported",
		},
		{
			name: "no questions",
			mutate: func(r *DecisionRequest) {
				r.Questions = nil
			},
			want: "decision questions are required",
		},
		{
			name: "empty name",
			mutate: func(r *DecisionRequest) {
				r.Questions[0].Name = ""
			},
			want: "name is required",
		},
		{
			name: "duplicate name",
			mutate: func(r *DecisionRequest) {
				r.Questions[1].Name = r.Questions[0].Name
			},
			want: `repeats name "is_true"`,
		},
		{
			name: "empty instructions",
			mutate: func(r *DecisionRequest) {
				r.Questions[0].Instructions = ""
			},
			want: "instructions are required",
		},
		{
			name: "vendor kind spelling",
			mutate: func(r *DecisionRequest) {
				r.Questions[0].Kind = DecisionQuestionKind("noul")
			},
			want: `unknown decision question kind "noul"`,
		},
		{
			name: "predicate with choices",
			mutate: func(r *DecisionRequest) {
				r.Questions[0].Choices = []DecisionChoice{{
					Value: NewDecisionStringChoice("yes"),
				}}
			},
			want: "cannot carry choices or levels",
		},
		{
			name: "choice with one option",
			mutate: func(r *DecisionRequest) {
				r.Questions[1].Choices = r.Questions[1].Choices[:1]
			},
			want: "needs at least 2 choices",
		},
		{
			name: "choice repeats a value",
			mutate: func(r *DecisionRequest) {
				r.Questions[1].Choices[1].Value = NewDecisionStringChoice("yes")
			},
			want: `duplicate choice "yes"`,
		},
		{
			name: "choice with levels",
			mutate: func(r *DecisionRequest) {
				r.Questions[1].Levels = []DecisionLevel{
					{Label: "low"},
					{Label: "high"},
				}
			},
			want: "cannot carry levels",
		},
		{
			name: "score with one level",
			mutate: func(r *DecisionRequest) {
				r.Questions[2].Levels = r.Questions[2].Levels[:1]
			},
			want: "needs at least 2 levels",
		},
		{
			name: "score repeats a label",
			mutate: func(r *DecisionRequest) {
				r.Questions[2].Levels[1].Label = "low"
			},
			want: `duplicate level label "low"`,
		},
		{
			name: "score with choices",
			mutate: func(r *DecisionRequest) {
				r.Questions[2].Choices = []DecisionChoice{{
					Value: NewDecisionStringChoice("yes"),
				}}
			},
			want: "cannot carry choices",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := fixture.Clone()
			testCase.mutate(&request)
			err := request.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

// TestDecisionChoiceValuesAreTyped pins the type sensitivity the wires share:
// the string "true" and the boolean true are different values, and the string
// and boolean forms of the same text do not collide in validation.
func TestDecisionChoiceValuesAreTyped(t *testing.T) {
	request := decisionRequestFixture()
	request.Questions[1].Choices = []DecisionChoice{
		{Value: NewDecisionStringChoice("true")},
		{Value: NewDecisionBooleanChoice(true)},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("typed values collided: %v", err)
	}
	request.Questions[1].Choices[1].Value = NewDecisionStringChoice("true")
	if err := request.Validate(); err == nil {
		t.Fatal("equal string values did not collide")
	}
}

func TestDecisionRequestCloneIsIndependent(t *testing.T) {
	request := decisionRequestFixture()
	request.Input.Parts = []message.Part{message.TextPart{Text: "evidence"}}
	request.Input.Text = ""
	request.RequestMetadata = map[string]string{"tenant": "acme"}
	clone := request.Clone()

	clone.Input.Parts[0] = message.TextPart{Text: "mutated"}
	clone.Questions[1].Choices[0].Value = NewDecisionStringChoice("mutated")
	clone.Questions[2].Levels[0].Label = "mutated"
	clone.RequestMetadata["tenant"] = "mutated"

	if request.Input.Parts[0].(message.TextPart).Text != "evidence" {
		t.Fatal("Clone aliases the input parts")
	}
	if request.Questions[1].Choices[0].Value.String != "yes" {
		t.Fatal("Clone aliases question choices")
	}
	if request.Questions[2].Levels[0].Label != "low" {
		t.Fatal("Clone aliases question levels")
	}
	if request.RequestMetadata["tenant"] != "acme" {
		t.Fatal("Clone aliases request metadata")
	}
}

func TestDecisionRequestActiveFields(t *testing.T) {
	request := decisionRequestFixture()
	request.RequestMetadata = map[string]string{"tenant": "acme"}
	fields := request.ActiveFields()
	want := []FieldID{
		FieldDecisionInputText,
		FieldDecisionQuestionName,
		FieldDecisionQuestionKind,
		FieldDecisionQuestionInstructions,
		FieldDecisionQuestionChoices,
		FieldDecisionQuestionLevels,
		FieldDecisionRequestMetadata,
	}
	if len(fields) != len(want) {
		t.Fatalf("ActiveFields() = %v, want %v", fields, want)
	}
	for index, field := range want {
		if fields[index] != field {
			t.Fatalf("ActiveFields() = %v, want %v", fields, want)
		}
	}

	request = decisionRequestFixture()
	request.Questions = request.Questions[:1]
	fields = request.ActiveFields()
	for _, field := range fields {
		if field == FieldDecisionQuestionChoices ||
			field == FieldDecisionQuestionLevels {
			t.Fatalf(
				"predicate-only request activates %q", field)
		}
	}

	imageURL, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	input := DecisionInput{Parts: []message.Part{
		message.ImagePart{Source: imageURL},
		message.TextPart{Text: "evidence"},
	}}
	fields = input.appendActiveFields(nil)
	wantParts := []FieldID{
		FieldDecisionInputPartText,
		FieldDecisionInputPartImage,
	}
	if len(fields) != len(wantParts) {
		t.Fatalf("part fields = %v, want %v", fields, wantParts)
	}
	for index, field := range wantParts {
		if fields[index] != field {
			t.Fatalf("part fields = %v, want %v", fields, wantParts)
		}
	}
}

// TestDecisionInputPartFieldLookup pins the exported lookup against its table
// and pins the exclusion: every other content kind is not decision evidence.
func TestDecisionInputPartFieldLookup(t *testing.T) {
	text, ok := DecisionInputPartField(message.PartText)
	if !ok || text != FieldDecisionInputPartText {
		t.Fatalf("DecisionInputPartField(text) = (%q, %v)", text, ok)
	}
	image, ok := DecisionInputPartField(message.PartImage)
	if !ok || image != FieldDecisionInputPartImage {
		t.Fatalf("DecisionInputPartField(image) = (%q, %v)", image, ok)
	}
	for _, kind := range message.PartKinds() {
		_, ok := decisionInputPartFields[kind]
		if kind == message.PartText || kind == message.PartImage {
			if !ok {
				t.Fatalf("decisionInputPartFields has no row for %q", kind)
			}
			continue
		}
		if ok {
			t.Fatalf("decisionInputPartFields unexpectedly has a row for %q", kind)
		}
	}
	if field, ok := DecisionInputPartField(message.PartKind("not-a-kind")); ok ||
		field != "" {
		t.Fatalf("unknown kind = (%q, %v), want no row", field, ok)
	}
}

func TestDecisionRequestJSONRoundTrip(t *testing.T) {
	imageURL, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := decisionRequestFixture()
	request.Input = DecisionInput{Parts: []message.Part{
		message.TextPart{Text: "evidence"},
		message.ImagePart{Source: imageURL},
	}}
	request.RequestMetadata = map[string]string{"tenant": "acme"}

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded DecisionRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded request is invalid: %v", err)
	}
	if len(decoded.Input.Parts) != 2 {
		t.Fatalf("decoded parts = %d, want 2", len(decoded.Input.Parts))
	}
	if _, ok := decoded.Input.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("decoded part 1 = %T, want message.ImagePart",
			decoded.Input.Parts[1])
	}
	if decoded.RequestMetadata["tenant"] != "acme" {
		t.Fatalf("decoded metadata = %v", decoded.RequestMetadata)
	}
	if len(decoded.Questions) != len(request.Questions) {
		t.Fatalf(
			"decoded questions = %d, want %d",
			len(decoded.Questions),
			len(request.Questions),
		)
	}
}

// TestDecisionInputRejectsUnknownFields pins the strict decode: a typo
// sibling to a carrier is rejected like every other canonical request field.
// A lenient decode would drop it, and the script bridge's outer
// DisallowUnknownFields never reaches inside this custom unmarshaler.
func TestDecisionInputRejectsUnknownFields(t *testing.T) {
	var input DecisionInput
	err := json.Unmarshal([]byte(`{"text":"evidence","txt":"typo"}`), &input)
	if err == nil {
		t.Fatal("Unmarshal accepted an unknown sibling field")
	}
	if !strings.Contains(err.Error(), `unknown field "txt"`) {
		t.Fatalf("Unmarshal = %v, want the unknown field named", err)
	}
}
