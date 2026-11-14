package openai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// decideCompile maps a decision request onto endpoint parameters.
func (d *driver) decideCompile(
	_ context.Context,
	request inference.DecisionRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.DecisionNewParams], error) {
	if shape != protocol.ShapeUnary {
		return protocol.Compiled[openai.DecisionNewParams]{}, inference.NewError(
			inference.UnsupportedFeature, inference.FieldStream,
			errors.New("openai: decisions are a unary call"))
	}
	if err := d.checkDecisionDeclarations(request); err != nil {
		return protocol.Compiled[openai.DecisionNewParams]{}, err
	}
	input, err := decisionInput(request.Input)
	if err != nil {
		return protocol.Compiled[openai.DecisionNewParams]{}, err
	}

	questions := make([]openai.DecisionNewParamsQuestionUnion, 0, len(request.Questions))
	for _, question := range request.Questions {
		built, err := decisionQuestion(question)
		if err != nil {
			return protocol.Compiled[openai.DecisionNewParams]{}, err
		}
		questions = append(questions, built)
	}

	return protocol.Compiled[openai.DecisionNewParams]{
		Wire: openai.DecisionNewParams{
			Model:     request.Model.ID.Name,
			Input:     openai.DecisionNewParamsInputUnion{OfString: openai.String(input)},
			Questions: questions,
		},
		Fields: decisionFields(request),
	}, nil
}

// decisionFields names the canonical fields this compilation honoured.
func decisionFields(request inference.DecisionRequest) []inference.Field {
	return append([]inference.Field{inference.FieldModel, inference.FieldInput, inference.FieldQuestions},
		request.Extensions.ActiveFields()...)
}

// decisionInput joins the text parts of the material being judged.
func decisionInput(content message.Content) (string, error) {
	var b strings.Builder
	for _, part := range content.Parts {
		text, ok := part.(message.TextPart)
		if !ok {
			return "", inference.NewError(
				inference.UnsupportedFeature, inference.FieldInput,
				fmt.Errorf("openai: decisions take text input, not %q", part.Kind()))
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text.Text)
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", inference.NewError(
			inference.InvalidRequest, inference.FieldInput, errors.New("openai: decision input is empty"))
	}
	return b.String(), nil
}

// decisionQuestion translates one question into its wire variant.
func decisionQuestion(question inference.Question) (openai.DecisionNewParamsQuestionUnion, error) {
	switch question.Kind {
	case inference.QuestionPredicate:
		return openai.DecisionNewParamsQuestionUnion{OfPredicate: &openai.DecisionNewParamsQuestionPredicate{
			Name: openai.String(question.Name),
			Instructions: openai.DecisionNewParamsQuestionPredicateInstructionsUnion{
				OfString: openai.String(question.Instructions),
			},
		}}, nil
	case inference.QuestionChoice:
		choices := make([]openai.DecisionNewParamsQuestionChoiceChoice, 0, len(question.Choices))
		for _, choice := range question.Choices {
			value := openai.DecisionNewParamsQuestionChoiceChoiceValueUnion{}
			if choice.Bool != nil {
				value.OfBool = openai.Bool(*choice.Bool)
			} else {
				value.OfString = openai.String(choice.Label)
			}
			choices = append(choices, openai.DecisionNewParamsQuestionChoiceChoice{Value: value})
		}
		return openai.DecisionNewParamsQuestionUnion{OfChoice: &openai.DecisionNewParamsQuestionChoice{
			Name:         openai.String(question.Name),
			Choices:      choices,
			Instructions: openai.DecisionNewParamsQuestionChoiceInstructionsUnion{OfString: openai.String(question.Instructions)},
		}}, nil
	case inference.QuestionScore:
		levels := make([]openai.DecisionNewParamsQuestionScoreLevel, 0, len(question.Levels))
		for _, level := range question.Levels {
			levels = append(levels, openai.DecisionNewParamsQuestionScoreLevel{Label: level})
		}
		return openai.DecisionNewParamsQuestionUnion{OfScore: &openai.DecisionNewParamsQuestionScore{
			Name:         openai.String(question.Name),
			Levels:       levels,
			Instructions: openai.DecisionNewParamsQuestionScoreInstructionsUnion{OfString: openai.String(question.Instructions)},
		}}, nil
	default:
		return openai.DecisionNewParamsQuestionUnion{}, inference.NewError(
			inference.InvalidRequest, inference.FieldQuestions,
			fmt.Errorf("openai: unknown question kind %q", question.Kind))
	}
}

// checkDecisionDeclarations enforces the declared line-up.
func (d *driver) checkDecisionDeclarations(request inference.DecisionRequest) error {
	if len(d.settings.Models) == 0 {
		return nil
	}
	descriptor, ok := findModel(d.settings.Models, request.Model.ID)
	if !ok {
		return inference.NewError(inference.UnknownModel, inference.FieldModel,
			fmt.Errorf("openai: model %q is not declared by this deployment", request.Model.ID.Name))
	}
	if !descriptor.Capabilities.Decision {
		return inference.NewError(inference.UnsupportedOperation, inference.FieldModel,
			fmt.Errorf("openai: model %q declares no decision support", descriptor.ID.Name))
	}
	return nil
}

// decideCall performs the provider I/O.
func (d *driver) decideCall(
	ctx context.Context,
	request inference.DecisionRequest,
	params openai.DecisionNewParams,
) (openai.Decision, error) {
	response, err := d.client.Decisions.New(ctx, params)
	if err != nil {
		return openai.Decision{}, err
	}
	return *response, nil
}

// decideDecode maps the wire answers back onto the canonical vocabulary.
func (d *driver) decideDecode(
	_ context.Context,
	request inference.DecisionRequest,
	raw openai.Decision,
) (inference.DecisionResponse, error) {
	answers := make([]inference.Answer, 0, len(raw.Answers))
	for _, item := range raw.Answers {
		answer := inference.Answer{Name: item.Name, Confidence: item.Confidence}
		switch item.Type {
		case "refusal":
			answer.Kind = inference.AnswerRefusal
			answer.Refusal = "the model declined this question"
		case string(inference.QuestionPredicate):
			probability := item.Probability
			answer.Kind = inference.AnswerKind(inference.QuestionPredicate)
			answer.Probability = &probability
		case string(inference.QuestionChoice):
			answer.Kind = inference.AnswerKind(inference.QuestionChoice)
			value := choiceFromUnion(item.Choice)
			answer.Choice = &value
			for _, probability := range item.Probabilities.OfDecisionAnswerChoiceProbabilities {
				answer.Probabilities = append(answer.Probabilities, inference.Probability{
					Label: probabilityLabel(probability.Value),
					Value: probability.Probability,
				})
			}
		case string(inference.QuestionScore):
			score := item.Score
			answer.Kind = inference.AnswerKind(inference.QuestionScore)
			answer.Score = &score
			for _, probability := range item.Probabilities.OfDecisionAnswerScoreProbabilities {
				answer.Probabilities = append(answer.Probabilities, inference.Probability{
					Label: probability.Label,
					Value: probability.Probability,
				})
			}
		default:
			return inference.DecisionResponse{}, inference.NewError(
				inference.InvalidProviderResponse, inference.FieldAnswers,
				fmt.Errorf("openai: unknown answer kind %q", item.Type))
		}
		answers = append(answers, answer)
	}

	return inference.DecisionResponse{
		Answers: answers,
		Usage:   decisionUsage(raw),
		Metadata: inference.Metadata{
			Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		},
	}, nil
}

// choiceFromUnion renders a typed choice value.
func choiceFromUnion(value openai.DecisionAnswerChoiceChoiceUnion) inference.ChoiceValue {
	if value.OfString != "" {
		return inference.ChoiceValue{Label: value.OfString}
	}
	boolean := value.OfBool
	return inference.ChoiceValue{Label: fmt.Sprintf("%t", boolean), Bool: &boolean}
}

// probabilityLabel renders a distribution entry's typed value.
func probabilityLabel(value openai.DecisionAnswerChoiceProbabilityValueUnion) string {
	if value.OfString != "" {
		return value.OfString
	}
	return fmt.Sprintf("%t", value.OfBool)
}

// decisionUsage maps the token counters the endpoint reports.
func decisionUsage(raw openai.Decision) usage.Usage {
	return usage.Usage{
		InputTokens:  raw.Usage.InputTokens,
		OutputTokens: raw.Usage.OutputTokens,
	}
}
