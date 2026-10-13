package typesafe

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// TestConformanceDecisionCompiler drives the shared compiler suite: the whole
// active field of the fixture request must reach the wire, and every field this
// route cannot carry must be refused by name rather than dropped.
func TestConformanceDecisionCompiler(t *testing.T) {
	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.DecisionRequest, systemOneParams]{
		Operation: model.OperationDecision,
		Model:     typesafeModel(DefaultModel),
		Request:   decisionRequestFixture,
		Snapshot: func(request inference.DecisionRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.DecisionRequest) []inference.FieldID {
			return request.ActiveFields()
		},
		Compile: compileDecision(DefaultModel),
		AssertWire: func(t *testing.T, params systemOneParams) {
			body := systemOneBody(t, params)
			if body["state"] != evidenceFixture || body["model"] != DefaultModel {
				t.Fatalf("body = %+v", body)
			}
			questions, ok := body["questions"].(map[string]any)
			if !ok || len(questions) != 3 {
				t.Fatalf("questions = %#v", body["questions"])
			}
			// The canonical names are the map keys, and the order the caller
			// wrote survives in the placement the transport reads, not in the
			// map the wire receives.
			ids := make([]string, 0, len(params.placement))
			for _, placed := range params.placement {
				ids = append(ids, placed.id)
			}
			if strings.Join(ids, ",") != "is_urgent,department,frustration" {
				t.Errorf("placement = %v, want the request order", ids)
			}
			predicate := questionBody(t, questions, "is_urgent")
			if predicate["type"] != kindNoul ||
				predicate["instructions"] != "Does this convey urgency?" {
				t.Errorf("predicate = %+v", predicate)
			}
			if _, present := predicate["criteria"]; present {
				// The canonical layer carries no yes/no criteria, so the
				// driver sends none rather than an empty object.
				t.Errorf("noul question carries criteria: %+v", predicate)
			}
			choice := questionBody(t, questions, "department")
			if choice["type"] != kindChoice {
				t.Errorf("choice = %+v", choice)
			}
			criteria, ok := choice["criteria"].(map[string]any)
			if !ok || len(criteria) != 3 {
				t.Fatalf("criteria = %#v", choice["criteria"])
			}
			if criteria["billing"] != "Payments, invoicing, refunds" {
				t.Errorf("described option = %#v", criteria["billing"])
			}
			if value, present := criteria["technical"]; !present || value != nil {
				// An option the caller left undescribed goes out as JSON
				// null, which is what the wire documents.
				t.Errorf("undescribed option = %#v (present=%v)", value, present)
			}
			score := questionBody(t, questions, "frustration")
			if score["type"] != kindScore {
				t.Errorf("score = %+v", score)
			}
			levels, ok := score["criteria"].([]any)
			if !ok || len(levels) != 3 {
				t.Fatalf("levels = %#v", score["criteria"])
			}
			for index, want := range []string{"Calm", "Frustrated", "Very angry"} {
				if levels[index] != want {
					t.Errorf("level %d = %#v, want %q", index, levels[index], want)
				}
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.DecisionRequest]{
			{
				Name: "text evidence part",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Input = inference.DecisionInput{
						Parts: []message.Part{message.TextPart{Text: evidenceFixture}},
					}
					return request
				},
				Field: inference.FieldDecisionInputPartText,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "image evidence part",
				Request: func() inference.DecisionRequest {
					image, err := media.NewImageURL("https://example.com/i.png", "image/png")
					if err != nil {
						t.Fatal(err)
					}
					request := decisionRequestFixture()
					request.Input = inference.DecisionInput{
						Parts: []message.Part{message.ImagePart{Source: image}},
					}
					return request
				},
				Field: inference.FieldDecisionInputPartImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "boolean choice option",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Questions[1].Choices = []inference.DecisionChoice{
						{Value: inference.NewDecisionStringChoice("billing")},
						{Value: inference.NewDecisionBooleanChoice(true)},
					}
					return request
				},
				Field: inference.FieldDecisionQuestionChoices,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "score level description",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Questions[2].Levels = []inference.DecisionLevel{
						{Label: "Calm"},
						{Label: "Frustrated", Description: "usable with review"},
					}
					return request
				},
				Field: inference.FieldDecisionQuestionLevels,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "request metadata",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.RequestMetadata = map[string]string{"trace": "t-1"}
					return request
				},
				Field: inference.FieldDecisionRequestMetadata,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "unknown question kind",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Questions[0].Kind = "certainty"
					return request
				},
				Field: inference.FieldDecisionQuestionKind,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "extension this provider does not serve",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Extensions = inference.Extensions{foreignExtension{}}
					return request
				},
				Field: foreignExtensionField,
				Kind:  inference.InvalidExtension,
			},
			{
				Name: "too many questions",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Questions = nil
					for index := 0; index <= decisionMaxQuestions; index++ {
						request.Questions = append(request.Questions,
							inference.DecisionQuestion{
								Name:         fmt.Sprintf("q%d", index),
								Kind:         inference.DecisionQuestionPredicate,
								Instructions: "Does this hold?",
							})
					}
					return request
				},
				Field: inference.FieldDecisionQuestionKind,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many options",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					choices := make([]inference.DecisionChoice, 0, decisionMaxChoices+1)
					for index := 0; index <= decisionMaxChoices; index++ {
						choices = append(choices, inference.DecisionChoice{
							Value: inference.NewDecisionStringChoice(
								fmt.Sprintf("option-%d", index)),
						})
					}
					request.Questions[1].Choices = choices
					return request
				},
				Field: inference.FieldDecisionQuestionChoices,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many levels",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					levels := make([]inference.DecisionLevel, 0, decisionMaxLevels+1)
					for index := 0; index <= decisionMaxLevels; index++ {
						levels = append(levels, inference.DecisionLevel{
							Label: fmt.Sprintf("level-%d", index),
						})
					}
					request.Questions[2].Levels = levels
					return request
				},
				Field: inference.FieldDecisionQuestionLevels,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

// TestDecisionRejectionReasons pins the reasons the ledger records, because the
// reason is what a caller reads to understand why a request the canonical layer
// accepted cannot be served on this wire.
func TestDecisionRejectionReasons(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func() inference.DecisionRequest
		field   inference.FieldID
		reason  string
	}{
		{
			name: "text evidence part",
			request: func() inference.DecisionRequest {
				request := decisionRequestFixture()
				request.Input = inference.DecisionInput{
					Parts: []message.Part{message.TextPart{Text: evidenceFixture}},
				}
				return request
			},
			field:  inference.FieldDecisionInputPartText,
			reason: "carries one state value, not message parts",
		},
		{
			name: "image evidence part",
			request: func() inference.DecisionRequest {
				image, err := media.NewImageURL("https://example.com/i.png", "image/png")
				if err != nil {
					t.Fatal(err)
				}
				request := decisionRequestFixture()
				request.Input = inference.DecisionInput{
					Parts: []message.Part{message.ImagePart{Source: image}},
				}
				return request
			},
			field:  inference.FieldDecisionInputPartImage,
			reason: "images are not a System One concept",
		},
		{
			name: "boolean choice option",
			request: func() inference.DecisionRequest {
				request := decisionRequestFixture()
				request.Questions[1].Choices = []inference.DecisionChoice{
					{Value: inference.NewDecisionStringChoice("billing")},
					{Value: inference.NewDecisionBooleanChoice(false)},
				}
				return request
			},
			field:  inference.FieldDecisionQuestionChoices,
			reason: "keys options by their text",
		},
		{
			name: "score level description",
			request: func() inference.DecisionRequest {
				request := decisionRequestFixture()
				request.Questions[2].Levels[1].Description = "an unusable rubric"
				return request
			},
			field:  inference.FieldDecisionQuestionLevels,
			reason: "takes one string per level",
		},
		{
			name: "request metadata",
			request: func() inference.DecisionRequest {
				request := decisionRequestFixture()
				request.RequestMetadata = map[string]string{"trace": "t-1"}
				return request
			},
			field:  inference.FieldDecisionRequestMetadata,
			reason: "carries no request metadata",
		},
		{
			name: "extension this provider does not serve",
			request: func() inference.DecisionRequest {
				request := decisionRequestFixture()
				request.Extensions = inference.Extensions{foreignExtension{}}
				return request
			},
			field:  foreignExtensionField,
			reason: `extension "custom_options" does not apply to decision`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compileDecision(DefaultModel)(
				context.Background(), typesafeModel(DefaultModel), tc.request())
			if err == nil {
				t.Fatal("Compile succeeded, want a rejection")
			}
			decision, ok := rejectionFor(compiled.Report, tc.field)
			if !ok {
				t.Fatalf("report records no rejection for %q: %+v",
					tc.field, compiled.Report)
			}
			if !strings.Contains(decision.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q",
					decision.Reason, tc.reason)
			}
		})
	}
}

// TestDecisionRequestBound pins the request's byte ceiling from both sides,
// because a bound tested only from the rejecting side is a bound whose
// comparison can flip without a test noticing. The state and the questions draw
// on one budget — the request's — so neither carrier is a way around it.
func TestDecisionRequestBound(t *testing.T) {
	// What one question adds to a request, measured through the compiler that
	// measures it, rather than restated here.
	measured := compileForTest(t, inference.DecisionRequest{
		Input:     inference.DecisionInput{Text: "x"},
		Questions: []inference.DecisionQuestion{predicateQuestion()},
	})
	questionBytes := questionWeight("is_urgent", measured.Questions)

	// The state is counted as it marshals, so a text of this many bytes
	// arrives as exactly decisionMaxInputBytes: the two bytes are its JSON
	// quotes.
	fits := decisionMaxInputBytes - questionBytes - 2

	compile := func(t *testing.T, request inference.DecisionRequest) (inference.CompileReport, error) {
		t.Helper()
		compiled, err := compileDecision(DefaultModel)(
			context.Background(), typesafeModel(DefaultModel), request)
		return compiled.Report, err
	}
	text := func(size int) inference.DecisionRequest {
		return inference.DecisionRequest{
			Input:     inference.DecisionInput{Text: strings.Repeat("a", size)},
			Questions: []inference.DecisionQuestion{predicateQuestion()},
		}
	}

	t.Run("a request exactly at the ceiling compiles", func(t *testing.T) {
		if _, err := compile(t, text(fits)); err != nil {
			t.Fatalf("Compile: %v", err)
		}
	})
	t.Run("a state past the ceiling is refused", func(t *testing.T) {
		report, err := compile(t, text(decisionMaxInputBytes))
		if err == nil {
			t.Fatal("Compile succeeded, want the state refused")
		}
		assertSizeRefusal(t, report, inference.FieldDecisionInputText,
			fmt.Sprintf("the state brings the request to %d bytes", decisionMaxInputBytes+2))
	})
	t.Run("the structured state draws on the same budget", func(t *testing.T) {
		fitsJSON := decisionMaxInputBytes - questionBytes
		structured := func(size int) inference.DecisionRequest {
			return inference.DecisionRequest{
				Input: inference.DecisionInput{
					// One JSON string inside an array: four bytes of
					// punctuation around the body.
					JSON: []byte(`["` + strings.Repeat("a", size-4) + `"]`),
				},
				Questions: []inference.DecisionQuestion{predicateQuestion()},
			}
		}
		if _, err := compile(t, structured(fitsJSON)); err != nil {
			t.Fatalf("Compile: %v", err)
		}
		report, err := compile(t, structured(decisionMaxInputBytes+1))
		if err == nil {
			t.Fatal("Compile succeeded, want the structured state refused")
		}
		assertSizeRefusal(t, report, inference.FieldDecisionInputJSON,
			fmt.Sprintf("the state brings the request to %d bytes", decisionMaxInputBytes+1))
	})
	t.Run("a state inside the ceiling does not excuse the questions", func(t *testing.T) {
		// The state fits on its own; the question after it is what crosses,
		// and the refusal says so — the two carriers draw on one budget,
		// which is what makes neither of them a way around it.
		report, err := compile(t, text(fits+1))
		if err == nil {
			t.Fatal("Compile succeeded, want the question refused")
		}
		assertSizeRefusal(t, report, inference.FieldDecisionQuestionInstructions,
			fmt.Sprintf("the question %q brings the request to %d bytes",
				"is_urgent", decisionMaxInputBytes+1))
	})
	t.Run("the questions draw on the same budget", func(t *testing.T) {
		// A state well inside the ceiling, and a choice question whose
		// rubric alone takes the request past it: the refusal names the
		// question that crossed, not the state that did not.
		request := decisionRequestFixture()
		request.Questions[1].Choices[0].Description =
			strings.Repeat("b", decisionMaxInputBytes)
		report, err := compile(t, request)
		if err == nil {
			t.Fatal("Compile succeeded, want the questions refused")
		}
		assertSizeRefusal(t, report, inference.FieldDecisionQuestionChoices,
			`the question "department"`)
	})
}

// assertSizeRefusal checks that a compile report records a size refusal against
// field, and that its reason says how large the request had become.
func assertSizeRefusal(
	t *testing.T,
	report inference.CompileReport,
	field inference.FieldID,
	want string,
) {
	t.Helper()
	decision, ok := rejectionFor(report, field)
	if !ok {
		t.Fatalf("report records no rejection for %q: %+v", field, report)
	}
	if !strings.Contains(decision.Reason, want) {
		t.Errorf("reason = %q, want it to contain %q", decision.Reason, want)
	}
}

// TestDecisionStateCarriers pins how each canonical carrier reaches state. Text
// goes out as a JSON string the model reads as text; structured evidence goes
// out as the value it already is, which is the one place this wire is wider than
// its OpenAI sibling — that wire has no structured field and flattens the same
// evidence to its serialized text.
func TestDecisionStateCarriers(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		params := compileForTest(t, decisionRequestFixture())
		if state := systemOneBody(t, params)["state"]; state != evidenceFixture {
			t.Fatalf("state = %#v, want the text itself", state)
		}
	})

	t.Run("json object", func(t *testing.T) {
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			JSON: []byte(`{"order": {"id": 7}, "failed": true}`),
		}
		body := systemOneBody(t, compileForTest(t, request))
		state, ok := body["state"].(map[string]any)
		if !ok {
			t.Fatalf("state = %#v, want the object itself", body["state"])
		}
		order, ok := state["order"].(map[string]any)
		if !ok || order["id"] != float64(7) || state["failed"] != true {
			t.Fatalf("state = %#v", state)
		}
	})

	t.Run("json array", func(t *testing.T) {
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			JSON: []byte(`[{"amount": 12.5, "currency": "USD"}]`),
		}
		body := systemOneBody(t, compileForTest(t, request))
		state, ok := body["state"].([]any)
		if !ok || len(state) != 1 {
			t.Fatalf("state = %#v, want the array itself", body["state"])
		}
		charge, ok := state[0].(map[string]any)
		if !ok || charge["amount"] != 12.5 || charge["currency"] != "USD" {
			t.Fatalf("state = %#v", state)
		}
	})
}

// TestDecisionDecodeAnswers pins the canonical reading of one full response:
// the answers come back in request order even though the wire keys them by id
// with no order at all, the typed choice is parsed back into a canonical
// string-typed value, the score distribution is rebuilt from the legend, and
// the usage total is the sum of the two counters the wire reports.
func TestDecisionDecodeAnswers(t *testing.T) {
	response, err := decodeWireBytes(
		t, wireResponseFixture(), compileForTest(t, decisionRequestFixture()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Model != "jev-1.13.0" {
		t.Errorf("model = %q, want the versioned id that answered", response.Model)
	}
	if len(response.Answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(response.Answers))
	}
	names := make([]string, 0, len(response.Answers))
	for _, answer := range response.Answers {
		names = append(names, answer.Name)
	}
	if strings.Join(names, ",") != "is_urgent,department,frustration" {
		t.Fatalf("answer order = %v, want the request order", names)
	}

	predicate := response.Answers[0]
	if predicate.Kind != inference.DecisionAnswerPredicate ||
		predicate.Probability == nil || *predicate.Probability != 0.99 {
		t.Errorf("predicate = %+v", predicate)
	}
	if predicate.Confidence != nil {
		// A noul is a two-outcome distribution: one number describes it
		// completely, and the wire publishes no separate confidence.
		t.Errorf("predicate carries a confidence: %+v", predicate)
	}

	choice := response.Answers[1]
	if choice.Kind != inference.DecisionAnswerChoice ||
		choice.Choice == nil || *choice.Choice != inference.NewDecisionStringChoice("billing") {
		t.Errorf("choice = %+v", choice)
	}
	if choice.Confidence == nil || *choice.Confidence != 0.81 {
		t.Errorf("confidence = %+v, want 0.81", choice.Confidence)
	}
	if len(choice.Probabilities) != 3 {
		t.Fatalf("distribution = %+v", choice.Probabilities)
	}
	// The distribution keeps the request's option order, not the wire's map
	// order, and every option is a string-typed canonical value.
	for index, want := range []string{"billing", "technical", "sales"} {
		option := choice.Probabilities[index]
		if option.Value != inference.NewDecisionStringChoice(want) {
			t.Errorf("option %d = %+v, want %q", index, option.Value, want)
		}
	}
	probabilities := []float64{0.88, 0.12, 0.0}
	for index, want := range probabilities {
		if got := choice.Probabilities[index].Probability; got != want {
			t.Errorf("option %d probability = %v, want %v", index, got, want)
		}
	}

	score := response.Answers[2]
	if score.Kind != inference.DecisionAnswerScore ||
		score.Score == nil || *score.Score != 1.05 {
		t.Errorf("score = %+v", score)
	}
	if score.Confidence == nil || *score.Confidence != 0.92 {
		t.Errorf("confidence = %+v, want 0.92", score.Confidence)
	}
	if len(score.Levels) != 3 {
		t.Fatalf("levels = %+v", score.Levels)
	}
	for index, want := range []string{"Calm", "Frustrated", "Very angry"} {
		level := score.Levels[index]
		if level.Level != index || level.Label != want {
			t.Errorf("level %d = %+v, want %q", index, level, want)
		}
	}
	if score.Levels[2].Probability != 0.05 {
		t.Errorf("level probabilities = %+v", score.Levels)
	}

	usage := response.Usage
	if usage.InputTokens != 318 || usage.OutputTokens != 34 ||
		usage.TotalTokens != 352 {
		t.Errorf("usage = %+v, want the halves to add up", usage)
	}
}

// TestDecisionDecodeRefusals pins the wire's contract one violation at a time.
// The decoder refuses rather than guessing, so a caller never receives an answer
// the model did not give: a declared kind missing its value, an incomplete
// distribution, a value outside the question, a legend that disagrees with the
// distribution it describes, or an answer to a question nobody asked. The one
// shape that is read rather than refused — a question with no typed answer —
// has its own test below.
func TestDecisionDecodeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request inference.DecisionQuestion
		body    string
		want    string
	}{
		{
			name:    "noul without a probability",
			request: predicateQuestion(),
			body:    `{"answers": {"is_urgent": {"type": "noul"}}}`,
			want:    "the noul answer carries no probability",
		},
		{
			name:    "choice without a chosen option",
			request: choiceQuestion(),
			body:    `{"answers": {"department": {"type": "choice"}}}`,
			want:    "carries no chosen option",
		},
		{
			name:    "choice without a distribution",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "billing",` +
				`"confidence": 0.7}}}`,
			want: "carries no distribution",
		},
		{
			name:    "choice distribution that does not sum",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "billing",` +
				`"confidence": 0.7, "probabilities": {"billing": 0.5, "technical": 0.2}}}}`,
			want: "distribution sums to 0.7, not 1",
		},
		{
			name:    "choice distribution naming an option nobody offered",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "billing",` +
				`"confidence": 0.7, "probabilities": {"billing": 0.5, "spam": 0.5}}}}`,
			want: "distributes an option the question never offered",
		},
		{
			name:    "chosen option nobody offered",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "spam",` +
				`"confidence": 0.7, "probabilities": {"billing": 0.5, "technical": 0.5}}}}`,
			want: `chose "spam", which the question never offered`,
		},
		{
			name:    "choice without a confidence",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "billing",` +
				`"probabilities": {"billing": 0.5, "technical": 0.5}}}}`,
			want: "carries no confidence",
		},
		{
			name:    "score without a score",
			request: scoreQuestion(),
			body:    `{"answers": {"frustration": {"type": "score", "confidence": 0.9}}}`,
			want:    "the score answer carries no score",
		},
		{
			name:    "score without a legend",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"confidence": 0.9, "probabilities": {"0": 1}}}}`,
			want: "carries no legend",
		},
		{
			name:    "score legend and distribution disagree",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"confidence": 0.9, "legend": {"0": "Calm", "1": "Frustrated"},` +
				`"probabilities": {"0": 0.1, "1": 0.45, "2": 0.45}}}}`,
			want: "legend names 2 levels, its distribution carries 3",
		},
		{
			name:    "score legend naming a level its distribution omits",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"confidence": 0.9, "legend": {"0": "Calm", "7": "Frustrated"},` +
				`"probabilities": {"0": 0.5, "1": 0.5}}}}`,
			want: `legend names level "7", which its distribution omits`,
		},
		{
			name:    "score legend key that is not a level index",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"confidence": 0.9, "legend": {"calm": "Calm", "loud": "Loud"},` +
				`"probabilities": {"calm": 0.5, "loud": 0.5}}}}`,
			// The legend is a JSON object, so which of the two keys the
			// decoder visits first is not the driver's to control: the
			// assertion is the refusal itself, not its iteration order.
			want: "which is not a level index",
		},
		{
			name:    "score probability outside [0,1]",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"confidence": 0.9, "legend": {"0": "Calm", "1": "Frustrated"},` +
				`"probabilities": {"0": 1.4, "1": -0.4}}}}`,
			want: "outside [0,1]",
		},
		{
			name:    "score without a confidence",
			request: scoreQuestion(),
			body: `{"answers": {"frustration": {"type": "score", "score": 1.05,` +
				`"legend": {"0": "Calm", "1": "Frustrated"},` +
				`"probabilities": {"0": 0.5, "1": 0.5}}}}`,
			want: "carries no confidence",
		},
		{
			name:    "answer map carrying an id nobody asked",
			request: choiceQuestion(),
			body: `{"answers": {"department": {"type": "choice", "choice": "billing",` +
				`"confidence": 1, "probabilities": {"billing": 1, "technical": 0}},` +
				`"sentiment": {"type": "noul", "noul": 0.5}}}`,
			want: `answers "sentiment", which the request never asked`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := singleQuestionParams(t, tc.request)
			_, err := decodeWireBytes(t, tc.body, params)
			if err == nil {
				t.Fatal("Decode succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestDecisionDecodeLeavesUnansweredQuestionsOpen pins the one leniency this
// wire gets, and how far it stops. The route publishes no refusal kind, but the
// canonical response has one: a question with no typed answer — whether the
// route left it out of the answer map or answered it with a type this driver
// cannot read, which is what a decline would look like — is recorded as that
// refusal, so the answers beside it survive instead of the whole call failing.
//
// Everything the answer does claim is still read strictly: a declared kind
// whose payload is missing stays a failure, which the table above pins. The
// refusal is the driver's reading, not the model's word, so it carries nothing
// but the question's name.
func TestDecisionDecodeLeavesUnansweredQuestionsOpen(t *testing.T) {
	const answered = `"department": {"type": "choice", "choice": "billing",` +
		`"probabilities": {"billing": 0.9, "technical": 0.1, "sales": 0.0},` +
		`"confidence": 0.8},` +
		`"frustration": {"type": "score", "score": 1.0,` +
		`"legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},` +
		`"probabilities": {"0": 0.0, "1": 1.0, "2": 0.0},` +
		`"confidence": 0.9}`

	for _, tc := range []struct {
		name string
		body string
		open string
	}{
		{
			name: "an answer type this driver does not know",
			body: `{"model": "jev-1.13.0", "answers": {` +
				`"is_urgent": {"type": "decline"},` + answered + `},` +
				`"usage": {"input_tokens": 12, "output_tokens": 3}}`,
			open: "is_urgent",
		},
		{
			name: "a question the response leaves out",
			body: `{"model": "jev-1.13.0", "answers": {` + answered + `},` +
				`"usage": {"input_tokens": 12, "output_tokens": 3}}`,
			open: "is_urgent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := decodeWireBytes(
				t, tc.body, compileForTest(t, decisionRequestFixture()))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(response.Answers) != 3 {
				t.Fatalf("answers = %d, want one per question", len(response.Answers))
			}
			// The wire answers in map order; the canonical list keeps the
			// request's, which is what the sibling answers assert.
			wantKinds := []inference.DecisionAnswerKind{
				inference.DecisionAnswerRefusal,
				inference.DecisionAnswerChoice,
				inference.DecisionAnswerScore,
			}
			for index, answer := range response.Answers {
				if answer.Kind != wantKinds[index] {
					t.Errorf("answer %d kind = %q, want %q",
						index, answer.Kind, wantKinds[index])
				}
			}
			open := response.Answers[0]
			if open.Name != tc.open {
				t.Errorf("open answer names %q, want %q", open.Name, tc.open)
			}
			if open.Probability != nil || open.Choice != nil ||
				open.Confidence != nil || open.Score != nil ||
				len(open.Probabilities) > 0 || len(open.Levels) > 0 {
				t.Errorf("refusal = %+v, want only the question's name", open)
			}
			// The siblings are the answers the wire did give, read as usual:
			// a refusal must not flatten them.
			if chosen := response.Answers[1].Choice; chosen == nil ||
				chosen.String != "billing" {
				t.Errorf("choice answer = %+v, want the chosen option", response.Answers[1])
			}
			if score := response.Answers[2].Score; score == nil || *score != 1.0 {
				t.Errorf("score answer = %+v, want the value the wire sent",
					response.Answers[2])
			}
			if response.Model != "jev-1.13.0" || response.Usage.TotalTokens != 15 {
				t.Errorf("response = %+v, want the model and usage the wire sent",
					response)
			}
		})
	}
}

// TestDecisionUsageWithoutTotal pins the counter mapping when the wire reports
// no total of its own: the canonical total is the sum of the halves, because a
// zero total would read downstream as a call that used no tokens.
func TestDecisionUsageWithoutTotal(t *testing.T) {
	response, err := decodeWireBytes(t, `{
		"model": "jev-1.13.0",
		"answers": {"department": {"type": "choice", "choice": "billing",
			"confidence": 0.9, "probabilities": {"billing": 0.9, "technical": 0.1}}},
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`, singleQuestionParams(t, choiceQuestion()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	usage := response.Usage
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want the halves to add up", usage)
	}
}

// TestDecisionWire pins the whole path once, through the provider the resource
// factory builds: the request the route receives, and the canonical answers the
// response decodes into.
func TestDecisionWire(t *testing.T) {
	route := &fakeRoute{body: wireResponseFixture()}
	server := route.server(t)
	provider := mustBuildProvider(t, providerSettingsJSON(t,
		`{"endpoint": {"base_url": "`+server.URL+`/v1"}, "models": [`+
			`{"name": "`+DefaultModel+`", "kind": "decision",`+
			` "capabilities": {"inputs": ["text"]}}]}`))

	if len(provider.Models) != 1 || provider.Models[0].Openers.Decision == nil {
		t.Fatalf("provider = %+v", provider.Models)
	}
	driver, err := provider.Models[0].Openers.Decision(
		context.Background(), typesafeModel(DefaultModel))
	if err != nil {
		t.Fatalf("open decision driver: %v", err)
	}
	response, err := driver.Execute(
		context.Background(), typesafeModel(DefaultModel), decisionRequestFixture())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	call := route.lastCall(t)
	if call.path != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", call.path)
	}
	body := call.body
	if body["state"] != evidenceFixture || body["model"] != DefaultModel {
		t.Errorf("body = %+v", body)
	}
	questions, ok := body["questions"].(map[string]any)
	if !ok || len(questions) != 3 {
		t.Fatalf("questions = %#v", body["questions"])
	}
	if questionBody(t, questions, "is_urgent")["type"] != kindNoul {
		t.Errorf("is_urgent = %+v", questions["is_urgent"])
	}

	if response.Model != "jev-1.13.0" {
		t.Errorf("model = %q", response.Model)
	}
	if len(response.Answers) != 3 ||
		response.Answers[0].Name != "is_urgent" ||
		response.Answers[1].Name != "department" ||
		response.Answers[2].Name != "frustration" {
		t.Fatalf("answers = %+v", response.Answers)
	}
	if response.Usage.TotalTokens != 352 {
		t.Errorf("usage = %+v", response.Usage)
	}
}

// questionBody returns one question of a compiled body, which a caller may only
// read by name — the wire's question map is keyed by the caller's own ids.
func questionBody(t *testing.T, questions map[string]any, id string) map[string]any {
	t.Helper()
	question, ok := questions[id].(map[string]any)
	if !ok {
		t.Fatalf("question %q = %#v", id, questions[id])
	}
	return question
}
