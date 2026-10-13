package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// The decision workload lowers onto the standalone Decisions surface:
//
//	POST {base}/decisions
//
// One evidence input — a string, or user messages carrying text and image
// parts — plus an ordered question list, answered in one unary response whose
// answers keep the question order. Every question goes out with its `name`,
// which the canonical layer requires and the surface echoes per answer.
//
// The surface publishes upper bounds the canonical layer deliberately leaves
// to the driver (it enforces the two-option minimum and nothing above), so
// they are enforced here, at compile time, where the vendor fact lives.
const (
	// decisionMaxQuestions bounds one request's question list.
	decisionMaxQuestions = 200
	// decisionMaxChoices bounds one choice question's option list.
	decisionMaxChoices = 255
	// decisionMaxLevels bounds one score question's level list.
	decisionMaxLevels = 10
	// decisionMaxImages bounds the image parts one request may carry, across
	// the whole evidence input.
	decisionMaxImages = 128
	// decisionMaxInputBytes bounds the evidence input, counted as the bytes
	// one request carries: the text evidence by its length, the serialized
	// json evidence by its length, and every part by what it compiles to —
	// a linked image as its URL, an inline one as the data URI about a third
	// larger than the bytes it holds. All three carriers draw on the one
	// bound, so no carrier is a way around it.
	//
	// Question text is not counted. The surface publishes no per-question
	// byte bound to enforce (core requires only non-empty instructions), so
	// how much prose a question carries is the caller's budget; this bound
	// is about the evidence, which is what a caller does not write by hand.
	decisionMaxInputBytes = 10 << 20
	// decisionDistributionTolerance bounds the rounding a full distribution
	// may carry. The surface publishes complete distributions, so a sum
	// further off than rounding is a wire contract violation rather than a
	// value the caller could use.
	decisionDistributionTolerance = 1e-3
)

// decisionRaw is one decoded Decisions response: the answers in wire order,
// the model id the provider answered with, and the token counters.
type decisionRaw struct {
	model   string
	answers []openai.DecisionAnswerUnion
	usage   rawUsage
}

// decisionPartRefusalField names the field an unsupported part kind is refused
// on. Core's decision part table has rows for text and image only and answers
// (FieldID, false) for every other kind, which is why partField is the wrong
// helper here: a miss on that table is a kind this wire cannot carry, not the
// vocabulary drift partField panics on. The refusal names the part field this
// input activates for the parts it does carry, so the report covers a field
// the request really has. The text part field stands in for a parts carrier
// made only of kinds the surface cannot carry — the one state core's own
// validator rules out before the compiler is asked.
func decisionPartRefusalField(input inference.DecisionInput) inference.FieldID {
	for _, part := range input.Parts {
		if field, ok := inference.DecisionInputPartField(part.Kind()); ok {
			return field
		}
	}
	return inference.FieldDecisionInputPartText
}

// decisionInputTooLarge is the rejection reason every evidence carrier shares.
// carried counts the bytes one request would put on the wire, which for an
// inline image is the data URI it travels as rather than the bytes it holds.
func decisionInputTooLarge(carried int) string {
	return fmt.Sprintf(
		"input is %d bytes, the decisions surface accepts at most %d",
		carried, decisionMaxInputBytes)
}

func compileDecision(
	modelName string,
	declared ModelSpec,
	wire dialect,
) inference.Compiler[inference.DecisionRequest, openai.DecisionNewParams] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.DecisionRequest,
	) (inference.Compiled[openai.DecisionNewParams], error) {
		ledger := inference.NewLedger(
			model.OperationDecision, providerID, request.ActiveFields())
		params := openai.DecisionNewParams{Model: modelName}
		params.Input = compileDecisionInput(request.Input, declared, ledger)
		params.Questions = compileDecisionQuestions(request.Questions, ledger)
		if len(request.RequestMetadata) > 0 {
			// Generate carries metadata through a body envelope this
			// deployment opts into; the Decisions surface has no metadata
			// field at all, so the canonical bag is refused rather than
			// dropped.
			ledger.Reject(
				inference.FieldDecisionRequestMetadata,
				"the decisions surface carries no request metadata",
			)
		}

		// Provider options: DecisionOptions lowers one by one; extensions
		// for other operations are rejected wholesale.
		options, other := inference.ExtensionFor[DecisionOptions](request.Extensions)
		ledger.RejectExtensions("decision", other)
		compileDecisionOptions(&params, options, ledger)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[openai.DecisionNewParams]{
				Report: report,
			}, ledger.Err()
		}
		return inference.Compiled[openai.DecisionNewParams]{
			Wire:   params,
			Report: report,
		}, nil
	}
}

// compileDecisionInput lowers the one evidence carrier. The surface takes a
// string or user messages carrying text and image parts: text passes through,
// the canonical json carrier has no structured field here and lowers to its
// serialized text (the state the caller declared, spelled the one way this
// wire can carry it), and parts become a single user message. A kind the
// surface cannot carry is rejected through the ledger rather than dropped, and
// every carrier draws on the same byte budget: the parts route inlines image
// bytes, so without it a caller could put a gigabyte into one request that the
// text carrier would have refused at compile time.
func compileDecisionInput(
	input inference.DecisionInput,
	declared ModelSpec,
	ledger *inference.Ledger,
) openai.DecisionNewParamsInputUnion {
	switch {
	case input.Text != "":
		if len(input.Text) > decisionMaxInputBytes {
			ledger.Reject(
				inference.FieldDecisionInputText,
				decisionInputTooLarge(len(input.Text)),
			)
			return openai.DecisionNewParamsInputUnion{}
		}
		return openai.DecisionNewParamsInputUnion{
			OfString: param.NewOpt(input.Text),
		}
	case len(bytes.TrimSpace(input.JSON)) > 0:
		evidence := string(input.JSON)
		if len(evidence) > decisionMaxInputBytes {
			ledger.Reject(
				inference.FieldDecisionInputJSON,
				decisionInputTooLarge(len(evidence)),
			)
			return openai.DecisionNewParamsInputUnion{}
		}
		return openai.DecisionNewParamsInputUnion{
			OfString: param.NewOpt(evidence),
		}
	}

	refusal := decisionPartRefusalField(input)
	parts := make([]openai.DecisionInputPartUnionParam, 0, len(input.Parts))
	carried := 0
	images := 0
	for index, part := range input.Parts {
		switch value := part.(type) {
		case message.TextPart:
			carried += len(value.Text)
			if carried > decisionMaxInputBytes {
				ledger.Reject(
					inference.FieldDecisionInputPartText,
					decisionInputTooLarge(carried),
				)
				continue
			}
			parts = append(parts, openai.DecisionInputPartParamOfInputText(value.Text))
		case message.ImagePart:
			if !slices.Contains(declared.Capabilities.Inputs, message.PartImage) {
				ledger.Reject(
					inference.FieldDecisionInputPartImage,
					"model does not accept image input",
				)
				continue
			}
			images++
			if images > decisionMaxImages {
				ledger.Reject(inference.FieldDecisionInputPartImage, fmt.Sprintf(
					"input carries more than %d images", decisionMaxImages,
				))
				continue
			}
			uri := sourceURI(value.Source)
			carried += len(uri)
			if carried > decisionMaxInputBytes {
				ledger.Reject(
					inference.FieldDecisionInputPartImage,
					decisionInputTooLarge(carried),
				)
				continue
			}
			parts = append(parts, openai.DecisionInputPartUnionParam{
				OfInputImage: &openai.DecisionInputImageParam{
					ImageURL: uri,
				},
			})
		default:
			ledger.Reject(
				refusal,
				fmt.Sprintf("decision input part %d: %s evidence is not supported",
					index, part.Kind()),
			)
		}
	}
	if len(parts) == 0 {
		return openai.DecisionNewParamsInputUnion{}
	}
	return openai.DecisionNewParamsInputUnion{
		OfDecisionInputMessageArray: []openai.DecisionInputMessageParam{{
			Content: openai.DecisionInputMessageContentUnionParam{OfParts: parts},
			Type:    openai.DecisionInputMessageTypeMessage,
		}},
	}
}

// compileDecisionQuestions lowers the ordered question list. Each question
// carries its canonical name onto the wire, both because the surface echoes it
// per answer — the locator every consumer reads — and because the question
// order is the answer order the surface contracts.
func compileDecisionQuestions(
	questions []inference.DecisionQuestion,
	ledger *inference.Ledger,
) []openai.DecisionNewParamsQuestionUnion {
	if len(questions) > decisionMaxQuestions {
		ledger.Reject(inference.FieldDecisionQuestionKind, fmt.Sprintf(
			"request carries %d questions, the decisions surface accepts at most %d",
			len(questions), decisionMaxQuestions,
		))
	}
	compiled := make([]openai.DecisionNewParamsQuestionUnion, 0, len(questions))
	for _, question := range questions {
		name := param.NewOpt(question.Name)
		switch question.Kind {
		case inference.DecisionQuestionPredicate:
			compiled = append(compiled, openai.DecisionNewParamsQuestionUnion{
				OfPredicate: &openai.DecisionNewParamsQuestionPredicate{
					Instructions: question.Instructions,
					Name:         name,
				},
			})
		case inference.DecisionQuestionChoice:
			if len(question.Choices) > decisionMaxChoices {
				ledger.Reject(inference.FieldDecisionQuestionChoices, fmt.Sprintf(
					"question %q carries %d choices, the decisions surface accepts at most %d",
					question.Name, len(question.Choices), decisionMaxChoices,
				))
			}
			choices := make([]openai.DecisionNewParamsQuestionChoiceChoice, 0, len(question.Choices))
			for _, choice := range question.Choices {
				choices = append(choices, openai.DecisionNewParamsQuestionChoiceChoice{
					Value:       decisionChoiceParam(choice.Value),
					Description: decisionOptional(choice.Description),
				})
			}
			compiled = append(compiled, openai.DecisionNewParamsQuestionUnion{
				OfChoice: &openai.DecisionNewParamsQuestionChoice{
					Choices:      choices,
					Instructions: question.Instructions,
					Name:         name,
				},
			})
		case inference.DecisionQuestionScore:
			if len(question.Levels) > decisionMaxLevels {
				ledger.Reject(inference.FieldDecisionQuestionLevels, fmt.Sprintf(
					"question %q carries %d levels, the decisions surface accepts at most %d",
					question.Name, len(question.Levels), decisionMaxLevels,
				))
			}
			levels := make(
				[]openai.DecisionNewParamsQuestionScoreLevel, 0, len(question.Levels))
			for _, level := range question.Levels {
				levels = append(levels, openai.DecisionNewParamsQuestionScoreLevel{
					Label:       level.Label,
					Description: decisionOptional(level.Description),
				})
			}
			compiled = append(compiled, openai.DecisionNewParamsQuestionUnion{
				OfScore: &openai.DecisionNewParamsQuestionScore{
					Levels:       levels,
					Instructions: question.Instructions,
					Name:         name,
				},
			})
		default:
			// The canonical layer validates the kind before compile; guard
			// the wire rather than shipping a question with no type.
			ledger.Reject(
				inference.FieldDecisionQuestionKind,
				"unknown decision question kind "+string(question.Kind),
			)
		}
	}
	return compiled
}

// decisionChoiceParam lowers one typed option. The wire keeps the type: a
// string and a boolean sharing their text are two distinct options on both
// sides of the boundary, so the union never coerces one into the other.
func decisionChoiceParam(
	value inference.DecisionChoiceValue,
) openai.DecisionNewParamsQuestionChoiceChoiceValueUnion {
	switch value.Kind {
	case inference.DecisionChoiceValueBoolean:
		return openai.DecisionNewParamsQuestionChoiceChoiceValueUnion{
			OfBool: param.NewOpt(value.Boolean),
		}
	default:
		return openai.DecisionNewParamsQuestionChoiceChoiceValueUnion{
			OfString: param.NewOpt(value.String),
		}
	}
}

// decisionOptional keeps an unset description off the wire instead of sending
// an empty string, which the surface would read as a declared description.
func decisionOptional(value string) param.Opt[string] {
	if value == "" {
		return param.Opt[string]{}
	}
	return param.NewOpt(value)
}

// compileDecisionOptions lowers DecisionOptions onto the request.
func compileDecisionOptions(
	params *openai.DecisionNewParams,
	options DecisionOptions,
	ledger *inference.Ledger,
) {
	if options.SafetyIdentifier != "" {
		params.SafetyIdentifier = param.NewOpt(options.SafetyIdentifier)
	}
}

func transportDecision(
	client openai.Client,
) inference.Transport[openai.DecisionNewParams, decisionRaw] {
	return func(
		ctx context.Context,
		params openai.DecisionNewParams,
	) (decisionRaw, error) {
		response, err := client.Decisions.New(ctx, params)
		if err != nil {
			classified := classifyError(err)
			inference.LogProviderCall(
				ctx, providerID, "decision", params.Model, classified, "", "")
			return decisionRaw{}, classified
		}
		inference.LogProviderCall(ctx, providerID, "decision", params.Model, nil, "", "")
		return decisionRaw{
			model:   response.Model,
			answers: response.Answers,
			usage:   decisionUsage(response.Usage),
		}, nil
	}
}

// decisionUsage maps the surface's counters onto the shared raw usage shape,
// so the canonical mapping stays the one generate already owns.
func decisionUsage(usage openai.DecisionUsage) rawUsage {
	raw := rawUsage{
		inputTokens:      usage.InputTokens,
		outputTokens:     usage.OutputTokens,
		totalTokens:      usage.TotalTokens,
		cachedTokens:     usage.InputTokensDetails.CachedTokens,
		cacheWriteTokens: usage.InputTokensDetails.CacheWriteTokens,
		reasoningTokens:  usage.OutputTokensDetails.ReasoningTokens,
	}
	if raw.totalTokens == 0 {
		raw.totalTokens = raw.inputTokens + raw.outputTokens
	}
	return raw
}

// decodeDecision reads the answer set strictly. Which question each answer
// belongs to is reconciled against the request by the runtime's response
// validation — the decoder never sees the request — so what is checked here is
// what only the wire can be checked against: an echoed name, the kind's own
// fields, and the completeness of a published distribution.
func decodeDecision(
	_ context.Context,
	raw decisionRaw,
) (inference.DecisionResponse, error) {
	answers := make([]inference.DecisionAnswer, 0, len(raw.answers))
	for index, union := range raw.answers {
		answer, err := decodeDecisionAnswer(union)
		if err != nil {
			return inference.DecisionResponse{}, fmt.Errorf(
				"openai: decision answer %d: %w", index, err)
		}
		answers = append(answers, answer)
	}
	return inference.DecisionResponse{
		Answers: answers,
		Model:   raw.model,
		Usage:   rawUsageCanonical(raw.usage),
	}, nil
}

func decodeDecisionAnswer(
	union openai.DecisionAnswerUnion,
) (inference.DecisionAnswer, error) {
	if !union.JSON.Name.Valid() {
		// The request always declares a name and the surface echoes it.
		// An answer that carries none, or null, cannot be placed against a
		// question except by position, which this decoder refuses to guess.
		return inference.DecisionAnswer{}, errors.New("the answer carries no name")
	}
	switch union.Type {
	case "predicate":
		answer := union.AsPredicate()
		if !answer.JSON.Probability.Valid() {
			return inference.DecisionAnswer{}, errors.New(
				"the predicate answer carries no probability")
		}
		probability := answer.Probability
		return inference.DecisionAnswer{
			Name:        answer.Name,
			Kind:        inference.DecisionAnswerPredicate,
			Probability: &probability,
		}, nil
	case "choice":
		return decodeDecisionChoice(union.AsChoice())
	case "score":
		return decodeDecisionScore(union.AsScore())
	case "refusal":
		answer := union.AsRefusal()
		return inference.DecisionAnswer{
			Name: answer.Name,
			Kind: inference.DecisionAnswerRefusal,
		}, nil
	}
	return inference.DecisionAnswer{}, fmt.Errorf(
		"unknown decision answer type %q", decisionEcho(union.Type))
}

func decodeDecisionChoice(
	answer openai.DecisionAnswerChoice,
) (inference.DecisionAnswer, error) {
	// The wire declares the chosen value required, so a missing one is the
	// provider's own contract violation rather than a parse problem the
	// value check below would report as malformed JSON.
	if !answer.JSON.Choice.Valid() {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer carries no chosen value")
	}
	chosen, err := decodeTypedChoice(answer.Choice.RawJSON())
	if err != nil {
		return inference.DecisionAnswer{}, fmt.Errorf("chosen value: %w", err)
	}
	options := make(
		[]inference.DecisionOptionProbability, 0, len(answer.Probabilities))
	var total float64
	for index, option := range answer.Probabilities {
		if !option.JSON.Value.Valid() {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"option %d carries no value", index)
		}
		value, err := decodeTypedChoice(option.Value.RawJSON())
		if err != nil {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"option %d: %w", index, err)
		}
		total += option.Probability
		options = append(options, inference.DecisionOptionProbability{
			Value:       value,
			Probability: option.Probability,
		})
	}
	if err := checkDecisionDistribution(total, len(options), "choice"); err != nil {
		return inference.DecisionAnswer{}, err
	}
	if !answer.JSON.Confidence.Valid() {
		return inference.DecisionAnswer{}, errors.New(
			"the choice answer carries no confidence")
	}
	confidence := answer.Confidence
	return inference.DecisionAnswer{
		Name:          answer.Name,
		Kind:          inference.DecisionAnswerChoice,
		Choice:        &chosen,
		Confidence:    &confidence,
		Probabilities: options,
	}, nil
}

func decodeDecisionScore(
	answer openai.DecisionAnswerScore,
) (inference.DecisionAnswer, error) {
	if !answer.JSON.Score.Valid() {
		return inference.DecisionAnswer{}, errors.New(
			"the score answer carries no score")
	}
	levels := make(
		[]inference.DecisionLevelProbability, 0, len(answer.Probabilities))
	var total float64
	for index, level := range answer.Probabilities {
		// The index places the level, so it is required: a missing one would
		// silently read as level 0, and core reconciles the answer against
		// the question by that number. The label is copied rather than
		// required — core checks a non-empty label against the question and
		// tolerates an empty one, so a surface that reports no labels still
		// answers with levels every caller can read.
		if !level.JSON.Value.Valid() {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"level %d carries no index", index)
		}
		if level.Value < 0 || level.Value > math.MaxInt32 {
			return inference.DecisionAnswer{}, fmt.Errorf(
				"level %d names index %d", index, level.Value)
		}
		total += level.Probability
		levels = append(levels, inference.DecisionLevelProbability{
			Level:       int(level.Value),
			Label:       level.Label,
			Probability: level.Probability,
		})
	}
	if err := checkDecisionDistribution(total, len(levels), "score"); err != nil {
		return inference.DecisionAnswer{}, err
	}
	if !answer.JSON.Confidence.Valid() {
		return inference.DecisionAnswer{}, errors.New(
			"the score answer carries no confidence")
	}
	score := answer.Score
	confidence := answer.Confidence
	return inference.DecisionAnswer{
		Name:       answer.Name,
		Kind:       inference.DecisionAnswerScore,
		Score:      &score,
		Confidence: &confidence,
		Levels:     levels,
	}, nil
}

// decodeTypedChoice reads one option value off the wire. The value is a JSON
// string or a JSON boolean — the two types the surface publishes — and the
// type survives: it is what keeps "true" and true two distinct options.
func decodeTypedChoice(raw string) (inference.DecisionChoiceValue, error) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return inference.DecisionChoiceValue{}, fmt.Errorf(
			"choice value %s is not JSON: %w", decisionEcho(raw), err)
	}
	switch value := decoded.(type) {
	case string:
		return inference.NewDecisionStringChoice(value), nil
	case bool:
		return inference.NewDecisionBooleanChoice(value), nil
	}
	return inference.DecisionChoiceValue{}, fmt.Errorf(
		"choice value %s is neither a string nor a boolean", decisionEcho(raw))
}

// decisionEchoBytes bounds the provider text a decode error repeats. The
// answer's position and the question's name already locate the failure, so the
// echo is a fragment: a provider must not be able to put a megabyte of its own
// text into an error a deployment logs. (DashScope caps the same provider text
// at 4 KiB; a value this long is a contract violation either way.)
const decisionEchoBytes = 64

// decisionEcho renders provider text for a decode error, cut at
// decisionEchoBytes. The cut backs up to a rune boundary so the message stays
// valid UTF-8 whatever the provider sent.
func decisionEcho(raw string) string {
	if len(raw) <= decisionEchoBytes {
		return raw
	}
	cut := decisionEchoBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return raw[:cut] + "..."
}

// checkDecisionDistribution refuses a distribution the surface cannot have
// meant: an empty one, or one whose probabilities do not add up to a
// probability distribution. Per-value ranges are the canonical layer's check.
func checkDecisionDistribution(total float64, count int, kind string) error {
	if count == 0 {
		return errors.New("the " + kind + " answer carries no distribution")
	}
	if math.Abs(total-1) > decisionDistributionTolerance {
		return fmt.Errorf(
			"the %s answer distribution sums to %v, not 1", kind, total)
	}
	return nil
}

func openDecision(
	cls *clients,
	declared ModelSpec,
	wire dialect,
	id model.ModelID,
	_ string,
) (inference.DecisionDriver, error) {
	return inference.BindDecision(
		compileDecision(id.Name, declared, wire),
		transportDecision(cls.api),
		decodeDecision,
	)
}
