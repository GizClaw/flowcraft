package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// The decision workload lowers onto the System One route:
//
//	POST {base}/systemone
//
// One state value — text, or a structured JSON value the wire carries natively
// — plus a map of typed questions, answered in one unary response whose answers
// are keyed by the caller's own question ids. Those ids are not sent to the
// model, so the answer map is folded back onto the request order here.
//
// The route publishes bounds the canonical layer deliberately leaves to the
// driver (it enforces the two-option minimum and nothing above), so they are
// enforced here, at compile time, where the vendor fact lives. Each bound below
// says which kind of fact it is: the vendor's, cited to the page that publishes
// it, or this driver's own policy where the vendor publishes none.
const (
	// systemOnePath is the route, appended to the profile's base URL, which
	// carries the version segment.
	systemOnePath = "/systemone"

	// decisionMaxQuestions bounds one request's question map. Unlike the two
	// bounds below it, this ceiling is the driver's own policy: the vendor
	// publishes no maximum (the request schema's questions map carries a
	// minimum of one and no maximum, and the guidance is to batch every
	// question into one call), so this package picks a ceiling rather than
	// leaving the request unbounded. It is a compile-time constant, not a
	// deployment setting: a deployment that needs a different one needs this
	// line changed, and that is deliberate — a knob nobody can justify with a
	// published fact is configuration surface without a fact behind it.
	decisionMaxQuestions = 128
	// decisionMaxChoices bounds one choice question's option map, the
	// maximum the vendor publishes: "you can have a maximum of 255 options
	// per Choice" (docs.typesafe.ai/api, /primitives/choice).
	decisionMaxChoices = 255
	// decisionMaxLevels bounds one score question's level list, the maximum
	// the vendor publishes: "the API accepts up to 10" levels
	// (docs.typesafe.ai/primitives/score).
	decisionMaxLevels = 10
	// decisionMaxInputBytes bounds what one request carries: the state and
	// every question, counted by what each one compiles to on the wire. It is
	// the driver's own transport ceiling, the one its OpenAI sibling
	// enforces, and not the model's context budget: the vendor publishes 64k
	// tokens for the state plus all questions (docs.typesafe.ai/models), and
	// that is a capacity of the model, filtered by the declared model limits
	// and routing rather than by a route limit. This bound is about the
	// bytes the driver would put on one request, which is what stops a
	// caller from handing the route a body no model could ever read.
	decisionMaxInputBytes = 10 << 20
	// decisionMaxResponseBytes bounds the body the driver reads. A decision
	// response echoes the request's own option and level names, so it is of
	// the same order as the request the driver accepted; eight times the
	// input ceiling is far past anything that request could produce, and it
	// keeps a rogue route from turning one call into an unbounded read.
	decisionMaxResponseBytes = 8 << 20
	// decisionDistributionTolerance bounds the rounding a published
	// distribution may carry. The wire contracts that its probabilities sum
	// to one, so a sum further off than rounding is a wire contract
	// violation rather than a value the caller could use.
	decisionDistributionTolerance = 1e-3

	// The wire's own names for the canonical kinds. Question types and answer
	// types share them, and only this mapping lives here: "noul" is the
	// vendor's word for a yes/no proposition and appears nowhere outside this
	// driver. The answer union is closed to these three (the published
	// schemas for Question and Answer map exactly noul, choice, and score, and
	// neither the API reference nor the primitive pages describe a refusal),
	// which is what makes an answer outside them readable as "no answer"
	// rather than as a kind to coerce.
	kindNoul   = "noul"
	kindChoice = "choice"
	kindScore  = "score"

	// evidencePartReason and imageEvidenceReason explain the two refusals an
	// evidence part earns. The wire has one state value and no message-part
	// concept, and its answer to non-text material is not "unsupported" but
	// "pre-process it into state", which is the vendor's own guidance.
	evidencePartReason  = "the System One wire carries one state value, not message parts"
	imageEvidenceReason = "images are not a System One concept: pre-process image " +
		"evidence into text or structured state before sending it as state"
)

// systemOneQuestion is one question as the route receives it. Criteria is the
// only part that differs by kind — the option map of a choice, the ordered level
// list of a score, and nothing at all for a noul — so it holds whichever shape
// the kind produces and is omitted when there is none.
type systemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// systemOneParams is one compiled System One request: the body the route
// receives, plus the placement facts the response needs and the body must never
// carry.
//
// The wire answers under the caller's question ids as a map, while the canonical
// response is an ordered list that the runtime reconciles against the questions
// by position — and the ids themselves are never sent to the model. The order
// therefore lives outside the body, where only the transport can read it.
type systemOneParams struct {
	State     json.RawMessage              `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]systemOneQuestion `json:"questions"`

	// placement is not part of the body: encoding/json ignores unexported
	// fields, and the route would read the caller's ids as question data.
	placement []questionPlacement
}

// questionPlacement is one question's place in the request: the id its answer
// comes back under, and — for a choice question — the options the request
// offered, in request order. That list is both the set an answer distribution
// may name and the order the canonical distribution keeps.
type questionPlacement struct {
	id      string
	options []string
}

// wireResponse is one System One response as the route returns it. The answers
// map is unordered by contract: entries are keyed by the ids the caller chose,
// and nothing in the body says which question came first.
type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   wireUsage             `json:"usage"`
}

// wireAnswer is one answer. Which fields are populated follows Type, and the
// scalar values are pointers so an absent one is distinguishable from a reported
// zero — the difference between "the wire answered" and "the wire said nothing",
// which the decoder refuses to paper over.
type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        *string            `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// wireUsage is the route's token report: two counters and no total.
type wireUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// decisionRaw is one response in transit: the body as it arrived, and the
// placement the request needs to read it back into canonical order.
//
// The placement is here rather than in the decoder's arguments because the
// decoder is handed the request's raw value and nothing else — the ids never
// leave the compiler, and only the transport, which carried the compiled
// request, can put them beside the body. They stay inside the driver: this
// value never reaches the wire, and the ids are the caller's own locators,
// which the route is never told.
type decisionRaw struct {
	placement []questionPlacement
	body      []byte
	// truncated records that the route sent more than
	// decisionMaxResponseBytes. The bytes past the ceiling were not read, so
	// what arrived cannot be parsed as a complete response; the decoder
	// reports it as a provider whose answer cannot be read rather than as a
	// call that failed.
	truncated bool
}

// orderedAnswer is one answer placed against the question it answers: the id,
// which the canonical answer carries as its name, and — for a choice question —
// the options the request offered. A question the response leaves unanswered
// keeps the zero answer, which is what makes it indistinguishable, downstream,
// from an answer type this driver does not know: both are a question with no
// typed answer, which is what the canonical refusal records.
type orderedAnswer struct {
	id      string
	options []string
	answer  wireAnswer
}

// partField resolves a decision input part kind to its ledger field through
// core's own table, so the driver cannot drift from the field list the runtime
// activates. A miss cannot happen — core's TestDecisionInputPartFieldLookup pins
// the table against message.PartKinds — and it panics rather than recording a
// decision against an empty field, which never reaches the report.
func partField(kind message.PartKind) inference.FieldID {
	field, ok := inference.DecisionInputPartField(kind)
	if !ok {
		panic("typesafe: no ledger field for decision input part kind " + string(kind))
	}
	return field
}

// compileDecision lowers one canonical request onto the System One body. The
// declared model name is what selects the model on the wire; nothing else about
// the declaration reaches it, because the family contract has already refused a
// declaration this route cannot serve.
func compileDecision(
	modelName string,
) inference.Compiler[inference.DecisionRequest, systemOneParams] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.DecisionRequest,
	) (inference.Compiled[systemOneParams], error) {
		ledger := inference.NewLedger(
			model.OperationDecision, providerID, request.ActiveFields())
		params := systemOneParams{
			State:     compileState(request.Input, ledger),
			Model:     modelName,
			Questions: make(map[string]systemOneQuestion, len(request.Questions)),
		}
		// The state has already drawn on the request's byte budget, so the
		// questions continue from what it compiled to.
		params.placement = compileQuestions(
			request.Questions, params.Questions, ledger, len(params.State))
		if len(request.RequestMetadata) > 0 {
			// The body carries state, model, and questions, and nothing else:
			// there is no envelope for the canonical metadata bag, so it is
			// refused rather than dropped.
			ledger.Reject(
				inference.FieldDecisionRequestMetadata,
				"the System One wire carries no request metadata",
			)
		}
		// This driver ships no provider extension, so every extension that
		// reached this attempt is meant for another operation or another
		// deployment: refusing them field by field keeps the report from
		// claiming settings the caller configured and the route never saw.
		ledger.RejectExtensions("decision", request.Extensions)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[systemOneParams]{Report: report}, ledger.Err()
		}
		return inference.Compiled[systemOneParams]{
			Wire:   params,
			Report: report,
		}, nil
	}
}

// compileState lowers the one evidence carrier onto state. The wire takes a
// string or a structured value and carries both natively — the one place this
// wire is wider than its OpenAI sibling, which has no structured field and
// flattens the same evidence to its serialized text — so text goes out as a
// JSON string and the json carrier goes out as the value it already is.
//
// Parts are refused: the wire has one state value and no message-part concept,
// so a text part would become a different state than the caller wrote and an
// image has no channel at all. Dropping either would evaluate a request the
// caller never made.
//
// A state past decisionMaxInputBytes is refused rather than sent: it draws on
// the same budget the questions do, so the state alone is not a way around the
// bound. It is counted as it marshals — quotes and escapes included — because
// that is what the request carries.
func compileState(
	input inference.DecisionInput,
	ledger *inference.Ledger,
) json.RawMessage {
	switch {
	case input.Text != "":
		encoded, err := json.Marshal(input.Text)
		if err != nil {
			// A Go string always marshals; a failure here is not a fact about
			// the request, so there is nothing to record against a field.
			return nil
		}
		if len(encoded) > decisionMaxInputBytes {
			ledger.Reject(
				inference.FieldDecisionInputText,
				decisionRequestTooLarge("the state", len(encoded)),
			)
			return nil
		}
		return encoded
	case len(bytes.TrimSpace(input.JSON)) > 0:
		if len(input.JSON) > decisionMaxInputBytes {
			ledger.Reject(
				inference.FieldDecisionInputJSON,
				decisionRequestTooLarge("the state", len(input.JSON)),
			)
			return nil
		}
		return input.JSON
	}
	for _, part := range input.Parts {
		ledger.Reject(partField(part.Kind()), partReason(part.Kind()))
	}
	return nil
}

// decisionRequestTooLarge is the rejection reason the request's carriers share.
// carrier names what drew on the budget — the state, or the questions after a
// given one — and carried counts the bytes one request would put on the wire.
func decisionRequestTooLarge(carrier string, carried int) string {
	return fmt.Sprintf(
		"%s brings the request to %d bytes, the driver accepts at most %d",
		carrier, carried, decisionMaxInputBytes)
}

// partReason explains why one evidence part cannot reach this wire. Text and
// image are the two kinds a decision input may carry — core refuses the others
// before the compiler runs — and image gets the vendor's own answer rather than
// a generic one, because pre-processing it into state is a real alternative
// while silently reshaping text evidence is not.
func partReason(kind message.PartKind) string {
	if kind == message.PartImage {
		return imageEvidenceReason
	}
	return evidencePartReason
}

// compileQuestions lowers the ordered canonical question list into the wire's
// question map, and returns the placement the transport needs: the ids in
// request order and each choice question's offered options.
//
// The canonical names become the map keys, which is exactly their role on this
// wire — the key is the answer's locator and is never sent to the model. The
// order the caller wrote is not lost by the map: the placement keeps it, and the
// transport is what restores it on the way back.
//
// The questions draw on the same byte budget the state does, so a question
// cannot be a way around the bound either: carried arrives holding what the
// state compiled to, and each question adds what it compiles to. The first
// question that crosses the ceiling is refused on the field carrying its bulk,
// and the compile stops there — the request is refused as a whole, so the
// questions after it need no separate accounting.
func compileQuestions(
	questions []inference.DecisionQuestion,
	compiled map[string]systemOneQuestion,
	ledger *inference.Ledger,
	carried int,
) []questionPlacement {
	if len(questions) > decisionMaxQuestions {
		ledger.Reject(inference.FieldDecisionQuestionKind, fmt.Sprintf(
			"request carries %d questions, the driver accepts at most %d",
			len(questions), decisionMaxQuestions,
		))
	}
	placement := make([]questionPlacement, 0, len(questions))
	for _, question := range questions {
		placed := questionPlacement{id: question.Name}
		switch question.Kind {
		case inference.DecisionQuestionPredicate:
			// The canonical predicate is the wire's noul. No criteria: the
			// wire makes it optional, and the canonical layer has no yes/no
			// criteria field to lower into it.
			compiled[question.Name] = systemOneQuestion{
				Type:         kindNoul,
				Instructions: question.Instructions,
			}
		case inference.DecisionQuestionChoice:
			placed.options = compileChoices(question, compiled, ledger)
		case inference.DecisionQuestionScore:
			compileScore(question, compiled, ledger)
		default:
			// The canonical layer validates the kind before the compiler runs,
			// so this guards the body rather than shipping a question with no
			// type.
			ledger.Reject(
				inference.FieldDecisionQuestionKind,
				"unknown decision question kind "+string(question.Kind),
			)
		}
		placement = append(placement, placed)
		carried += questionWeight(question.Name, compiled)
		if carried > decisionMaxInputBytes {
			ledger.Reject(
				questionCarrierField(question.Kind),
				decisionRequestTooLarge(
					fmt.Sprintf("the question %q", question.Name), carried),
			)
			return placement
		}
	}
	return placement
}

// questionWeight is what one compiled question adds to the request: the
// question as it marshals, plus the key it is filed under. Both are bytes the
// request carries — the key is the caller's locator and is sent with the
// question, even though the route does not show it to the model.
func questionWeight(name string, compiled map[string]systemOneQuestion) int {
	encoded, err := json.Marshal(compiled[name])
	if err != nil {
		// Every leaf of a compiled question is a string or a string-keyed
		// map, so marshalling cannot fail; a question that somehow did
		// would weigh its key alone rather than abort a compile whose
		// ledger is the thing that reports.
		return len(name)
	}
	return len(name) + len(encoded)
}

// questionCarrierField names the field that carries a question's bulk, which is
// where a refusal about the request's size lands: the option map of a choice,
// the level list of a score, and the instructions of a noul, which is the only
// content a predicate question has.
func questionCarrierField(kind inference.DecisionQuestionKind) inference.FieldID {
	switch kind {
	case inference.DecisionQuestionChoice:
		return inference.FieldDecisionQuestionChoices
	case inference.DecisionQuestionScore:
		return inference.FieldDecisionQuestionLevels
	case inference.DecisionQuestionPredicate:
		return inference.FieldDecisionQuestionInstructions
	}
	// An unknown kind is already refused on the kind field; a size refusal
	// for one lands beside it rather than against an empty field.
	return inference.FieldDecisionQuestionKind
}

// compileChoices lowers one choice question into the criteria map the wire
// takes — option text to rubric description, with a JSON null for an option the
// caller left undescribed — and returns the offered options in request order,
// which is the set an answer distribution may name.
//
// A boolean option has no channel here: the wire keys options by their text, so
// the boolean true and the string "true" would collide into one option and lose
// the distinction the canonical layer keeps. Sending the text would answer a
// question the caller did not ask, so the option is refused instead.
func compileChoices(
	question inference.DecisionQuestion,
	compiled map[string]systemOneQuestion,
	ledger *inference.Ledger,
) []string {
	if len(question.Choices) > decisionMaxChoices {
		ledger.Reject(inference.FieldDecisionQuestionChoices, fmt.Sprintf(
			"question %q carries %d choices, the System One route accepts at most %d",
			question.Name, len(question.Choices), decisionMaxChoices,
		))
	}
	criteria := make(map[string]*string, len(question.Choices))
	options := make([]string, 0, len(question.Choices))
	for _, choice := range question.Choices {
		if choice.Value.Kind != inference.DecisionChoiceValueString {
			ledger.Reject(inference.FieldDecisionQuestionChoices, fmt.Sprintf(
				"question %q offers %s: the System One wire keys options by "+
					"their text, so a boolean option cannot be expressed",
				question.Name, choice.Value.Describe(),
			))
			continue
		}
		option := choice.Value.String
		options = append(options, option)
		criteria[option] = description(choice.Description)
	}
	compiled[question.Name] = systemOneQuestion{
		Type:         kindChoice,
		Instructions: question.Instructions,
		Criteria:     criteria,
	}
	return options
}

// compileScore lowers one score question into the ordered level list the wire
// takes. The list carries the level labels, because the wire has exactly one
// string per level: a declared description has no channel, so it is refused
// rather than dropped — the compile report must not claim the model was given a
// rubric it never received.
func compileScore(
	question inference.DecisionQuestion,
	compiled map[string]systemOneQuestion,
	ledger *inference.Ledger,
) {
	if len(question.Levels) > decisionMaxLevels {
		ledger.Reject(inference.FieldDecisionQuestionLevels, fmt.Sprintf(
			"question %q carries %d levels, the System One route accepts at most %d",
			question.Name, len(question.Levels), decisionMaxLevels,
		))
	}
	labels := make([]string, 0, len(question.Levels))
	for _, level := range question.Levels {
		if level.Description != "" {
			ledger.Reject(inference.FieldDecisionQuestionLevels, fmt.Sprintf(
				"question %q level %q carries a description: the System One wire "+
					"takes one string per level",
				question.Name, level.Label,
			))
		}
		labels = append(labels, level.Label)
	}
	compiled[question.Name] = systemOneQuestion{
		Type:         kindScore,
		Instructions: question.Instructions,
		Criteria:     labels,
	}
}

// description renders one optional rubric description: the wire wants JSON null
// for an option that carries none, which is what the canonical empty string
// means.
func description(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// transportDecision sends one compiled request to the route and hands the body
// it answered with to the decoder, beside the placement the answer map needs.
//
// The transport owns the exchange and the bytes: the request it sends, the
// status it maps, and reading the answer. What the bytes mean — whether they
// parse, whether they answer this request — is the decoder's, which is also
// where core classifies an unreadable answer set: a response that arrived but
// cannot be read as this contract is provider-side corruption, which the
// runtime does not retry, and reading it here would only classify the same
// fact as a failed call and buy the same answer twice.
func transportDecision(
	cls *clients,
) inference.Transport[systemOneParams, decisionRaw] {
	return func(ctx context.Context, params systemOneParams) (decisionRaw, error) {
		body, err := json.Marshal(params)
		if err != nil {
			// Every leaf was validated or compiled by the time this runs, so
			// a body that does not marshal is a fact about the driver, not
			// about the route: permanent, and no retry buys a different one.
			return decisionRaw{}, errdefs.Validationf(
				"typesafe: marshal system one request: %v", err)
		}
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			cls.base+systemOnePath,
			bytes.NewReader(body),
		)
		if err != nil {
			// The base URL was validated when the deployment was built, so a
			// URL net/http refuses here is the deployment's, not a transient
			// one: retrying it would retry the same typo.
			return decisionRaw{}, errdefs.Validationf(
				"typesafe: build system one request: %v", err)
		}
		request.Header.Set(headerAuthorization, "Bearer "+cls.key)
		request.Header.Set(headerContentType, "application/json")
		for name, value := range cls.headers {
			request.Header.Set(name, value)
		}

		response, err := cls.http.Do(request)
		if err != nil {
			// A failed exchange is transient by construction: the transport
			// never saw an answer, so the router may retry it.
			failure := errdefs.NotAvailable(fmt.Errorf(
				"typesafe: %s %s: %w", http.MethodPost, systemOnePath, err))
			inference.LogProviderCall(
				ctx, providerID, "decision", params.Model, failure, "", "")
			return decisionRaw{}, failure
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			failure := classifyFailure(response, readFailureBody(response.Body))
			inference.LogProviderCall(
				ctx, providerID, "decision", params.Model, failure,
				requestIDOf(response), "")
			return decisionRaw{}, failure
		}
		payload, truncated, err := readResponseBody(
			response.Body, decisionMaxResponseBytes)
		if err != nil {
			// The exchange completed but the body did not arrive whole: the
			// transport never held an answer, which is transient by
			// construction and what the router may retry. A body that is
			// only larger than this driver reads is not a failed read: it
			// rides to the decoder, which reports it as unreadable.
			failure := errdefs.NotAvailable(fmt.Errorf(
				"typesafe: read system one response: %w", err))
			inference.LogProviderCall(
				ctx, providerID, "decision", params.Model, failure,
				requestIDOf(response), "")
			return decisionRaw{}, failure
		}
		inference.LogProviderCall(
			ctx, providerID, "decision", params.Model, nil,
			requestIDOf(response), "")
		return decisionRaw{
			placement: params.placement,
			body:      payload,
			truncated: truncated,
		}, nil
	}
}

// placeAnswers folds the wire's answer map onto the request's question order.
//
// The canonical response is an ordered list that the runtime reconciles against
// the questions by position, while this wire keys its answers by the caller's
// ids and contracts no order at all — and those ids never reach the model. The
// order therefore travels with the raw response, and this is where it is read
// back: one answer per question, in the order the caller asked them.
//
// An id the request never asked is refused rather than ignored: the response
// then answers something other than this request, and dropping the extra would
// make a mixed-up answer set look like a complete one. A question the response
// leaves out is not refused — it keeps the zero answer, which downstream reads
// as a question with no typed answer. The ids are the caller's own locator on
// this wire, so an absent key is the route saying it has no answer to that
// question, and the canonical contract has a slot for exactly that.
func placeAnswers(
	response wireResponse,
	placement []questionPlacement,
) ([]orderedAnswer, error) {
	asked := make(map[string]struct{}, len(placement))
	for _, placed := range placement {
		asked[placed.id] = struct{}{}
	}
	ids := make([]string, 0, len(response.Answers))
	for id := range response.Answers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if _, ok := asked[id]; !ok {
			return nil, fmt.Errorf(
				"typesafe: the system one response answers %q, which the request never asked",
				id,
			)
		}
	}
	answers := make([]orderedAnswer, 0, len(placement))
	for _, placed := range placement {
		answers = append(answers, orderedAnswer{
			id:      placed.id,
			options: placed.options,
			answer:  response.Answers[placed.id],
		})
	}
	return answers, nil
}

// decodeDecision reads the answer set strictly. Which question each answer
// belongs to is settled by placeAnswers, which folds the answer map onto the
// request order; what is checked here is what an answer alone can be checked
// against: the fields its kind requires, the completeness of a published
// distribution, and the consistency of a legend with the distribution it
// describes.
//
// The body is decoded into the fields this driver models and no stricter: a
// vendor that adds a field must not break every call. The strictness is in the
// reading of the values, one answer at a time, below.
//
// Every failure here is the same fact — a 200 whose body this driver cannot
// read as the contract it declares — and core classifies it as an invalid
// provider response, which the runtime does not retry: the retry would buy the
// same body again. The transport keeps the failures a retry can fix (an
// exchange that failed, a body that did not arrive whole).
func decodeDecision(
	_ context.Context,
	raw decisionRaw,
) (inference.DecisionResponse, error) {
	if raw.truncated {
		return inference.DecisionResponse{}, fmt.Errorf(
			"typesafe: the system one response is larger than %d bytes",
			decisionMaxResponseBytes)
	}
	var response wireResponse
	if err := json.Unmarshal(raw.body, &response); err != nil {
		return inference.DecisionResponse{}, fmt.Errorf(
			"typesafe: decode system one response: %w", err)
	}
	placed, err := placeAnswers(response, raw.placement)
	if err != nil {
		return inference.DecisionResponse{}, err
	}
	answers := make([]inference.DecisionAnswer, 0, len(placed))
	for index, placed := range placed {
		answer, err := decodeAnswer(placed)
		if err != nil {
			return inference.DecisionResponse{}, fmt.Errorf(
				"typesafe: decision answer %d (%s): %w", index, placed.id, err)
		}
		answers = append(answers, answer)
	}
	return inference.DecisionResponse{
		Answers: answers,
		Model:   response.Model,
		Usage:   decisionUsage(response.Usage),
	}, nil
}

// decodeAnswer reads one answer by the kind it declares.
//
// The vendor publishes no refusal kind, so this is where a decline — or any
// answer this driver cannot read as typed — becomes one: the canonical response
// has a refusal that replaces a question's answer and leaves the others alone,
// which is exactly "this question has no answer" without inventing a
// probability the route never sent. Refusing the whole call instead would throw
// away the answers beside it, and coercing the value into one of the three
// kinds would report a decision the model never made.
//
// The tolerance stops there: a declared kind whose own payload is missing or
// inconsistent is a failure, not a decline. That answer claims to be a noul,
// a choice, or a score and does not carry it, and reporting it as "no answer"
// would hide provider-side corruption behind a plausible-looking response.
func decodeAnswer(placed orderedAnswer) (inference.DecisionAnswer, error) {
	switch placed.answer.Type {
	case kindNoul:
		return decodeNoul(placed)
	case kindChoice:
		return decodeChoice(placed)
	case kindScore:
		return decodeScore(placed)
	}
	return inference.DecisionAnswer{
		Name: placed.id,
		Kind: inference.DecisionAnswerRefusal,
	}, nil
}

// decodeNoul reads a predicate answer. The wire's single probability is the
// whole distribution — it has two outcomes — which is why a noul carries no
// separate confidence, and why the canonical answer carries none either.
func decodeNoul(placed orderedAnswer) (inference.DecisionAnswer, error) {
	if placed.answer.Noul == nil {
		return inference.DecisionAnswer{}, errors.New(
			"the noul answer carries no probability")
	}
	probability := *placed.answer.Noul
	return inference.DecisionAnswer{
		Name:        placed.id,
		Kind:        inference.DecisionAnswerPredicate,
		Probability: &probability,
	}, nil
}

// decodeChoice reads a choice answer: the chosen option, the full distribution,
// and the confidence the wire derived from it.
//
// The distribution is walked in the request's option order, which is what makes
// the canonical list deterministic (the wire's map has no order) and what lets a
// distributed option with no counterpart in the question be refused: the driver
// will not invent an option, or a value type, to answer with. The canonical
// layer re-reconciles the same set, so this refusal is what keeps the report
// honest about which values the model could have meant.
func decodeChoice(placed orderedAnswer) (inference.DecisionAnswer, error) {
	answer := placed.answer
	if answer.Choice == nil {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer carries no chosen option")
	}
	if len(answer.Probabilities) == 0 {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer carries no distribution")
	}
	options := make([]inference.DecisionOptionProbability, 0, len(answer.Probabilities))
	var total float64
	for _, option := range placed.options {
		probability, distributed := answer.Probabilities[option]
		if !distributed {
			continue
		}
		total += probability
		options = append(options, inference.DecisionOptionProbability{
			Value:       inference.NewDecisionStringChoice(option),
			Probability: probability,
		})
	}
	if len(options) != len(answer.Probabilities) {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer distributes an option the question never offered")
	}
	if !slices.Contains(placed.options, *answer.Choice) {
		return inference.DecisionAnswer{}, fmt.Errorf(
			"the choice answer chose %q, which the question never offered",
			*answer.Choice,
		)
	}
	if err := checkDistribution(total, len(options), "choice"); err != nil {
		return inference.DecisionAnswer{}, err
	}
	if answer.Confidence == nil {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer carries no confidence")
	}
	chosen := inference.NewDecisionStringChoice(*answer.Choice)
	confidence := *answer.Confidence
	return inference.DecisionAnswer{
		Name:          placed.id,
		Kind:          inference.DecisionAnswerChoice,
		Choice:        &chosen,
		Confidence:    &confidence,
		Probabilities: options,
	}, nil
}

// decodeScore reads a score answer: the probability-weighted value, the level
// distribution built from the legend and the probabilities, and the confidence.
//
// The legend is what names a level — its index as a string key and its
// description as the value — so the two maps have to describe the same levels,
// key for key, and the index has to parse. The canonical labels come from the
// legend rather than from the request, because the request is not this
// function's to read; the runtime reconciles them against the question.
func decodeScore(placed orderedAnswer) (inference.DecisionAnswer, error) {
	answer := placed.answer
	if answer.Score == nil {
		return inference.DecisionAnswer{}, errors.New(
			"the score answer carries no score")
	}
	if len(answer.Legend) == 0 {
		return inference.DecisionAnswer{}, errors.New(
			"the score answer carries no legend")
	}
	if len(answer.Legend) != len(answer.Probabilities) {
		return inference.DecisionAnswer{}, fmt.Errorf(
			"the score answer legend names %d levels, its distribution carries %d",
			len(answer.Legend),
			len(answer.Probabilities),
		)
	}
	levels := make([]inference.DecisionLevelProbability, 0, len(answer.Legend))
	seen := make(map[int]struct{}, len(answer.Legend))
	var total float64
	for key, label := range answer.Legend {
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"the score answer legend names %q, which is not a level index", key)
		}
		if _, duplicate := seen[index]; duplicate {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"the score answer legend names level %d twice", index)
		}
		probability, distributed := answer.Probabilities[key]
		if !distributed {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"the score answer legend names level %q, which its distribution omits",
				key,
			)
		}
		if probability < 0 || probability > 1 {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"the score answer gives level %q probability %v, outside [0,1]",
				key,
				probability,
			)
		}
		seen[index] = struct{}{}
		total += probability
		levels = append(levels, inference.DecisionLevelProbability{
			Level:       index,
			Label:       label,
			Probability: probability,
		})
	}
	if err := checkDistribution(total, len(levels), "score"); err != nil {
		return inference.DecisionAnswer{}, err
	}
	// The legend is a JSON object, so its iteration order is random; the
	// canonical distribution is ordered by level, which is the order the
	// question's levels have.
	slices.SortFunc(levels, func(left, right inference.DecisionLevelProbability) int {
		return left.Level - right.Level
	})
	if answer.Confidence == nil {
		return inference.DecisionAnswer{}, errors.New(
			"the score answer carries no confidence")
	}
	score := *answer.Score
	confidence := *answer.Confidence
	return inference.DecisionAnswer{
		Name:       placed.id,
		Kind:       inference.DecisionAnswerScore,
		Score:      &score,
		Confidence: &confidence,
		Levels:     levels,
	}, nil
}

// checkDistribution refuses a distribution the wire cannot have meant: an empty
// one, or one whose probabilities do not add up to a probability distribution.
// Completeness is this wire's own contract — the canonical layer owns the
// per-value ranges — so the sum is checked here. A value outside [0,1] is
// refused alongside it in the score path, because a pair like 1.4 and -0.4 sums
// to one: the sum alone cannot rule the range out.
func checkDistribution(total float64, count int, kind string) error {
	if count == 0 {
		return errors.New("the " + kind + " answer carries no distribution")
	}
	if math.Abs(total-1) > decisionDistributionTolerance {
		return fmt.Errorf(
			"the %s answer distribution sums to %v, not 1", kind, total)
	}
	return nil
}

// decisionUsage lowers the wire's two counters onto the canonical usage. The
// wire reports no total, so the total is their sum: a decision call's tokens are
// exactly what went in — the state plus every question — and what came back.
func decisionUsage(usage wireUsage) inference.Usage {
	return inference.Usage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
}

// openDecision binds one decision pipeline for a declared model: the compiler
// lowers the request, the transport owns the one route the wire serves, and the
// decoder reads the answer set strictly into canonical answers.
func openDecision(cls *clients, id model.ModelID) (inference.DecisionDriver, error) {
	return inference.BindDecision(
		compileDecision(id.Name),
		transportDecision(cls),
		decodeDecision,
	)
}
