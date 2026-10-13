package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/GizClaw/flowcraft/core/message"
)

// DecisionInput is the evidence one decision call evaluates. Exactly one
// carrier is populated:
//
//   - Text carries plain text evidence;
//   - JSON carries one structured value (a JSON object or array);
//   - Parts carries ordered text and image parts.
//
// The carriers are mutually exclusive because the wires are: a provider takes
// either a text input, one structured state value, or messages carrying text
// and image parts — never two of them at once. Image support is a driver
// question, not a canonical one: a driver whose wire cannot carry images
// rejects that part at compile time through the field ledger.
type DecisionInput struct {
	Text  string          `json:"text,omitempty"`
	JSON  json.RawMessage `json:"json,omitempty"`
	Parts []message.Part  `json:"parts,omitempty"`
}

// MarshalJSON encodes the carriers with the canonical part encoding, so parts
// keep the same type-discriminated shape they have everywhere else.
func (i DecisionInput) MarshalJSON() ([]byte, error) {
	wire := struct {
		Text  string            `json:"text,omitempty"`
		JSON  json.RawMessage   `json:"json,omitempty"`
		Parts []json.RawMessage `json:"parts,omitempty"`
	}{Text: i.Text, JSON: i.JSON}
	for index, part := range i.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return nil, fmt.Errorf("decision input part %d: %w", index, err)
		}
		raw, err := message.MarshalPart(normalized)
		if err != nil {
			return nil, fmt.Errorf("decision input part %d: %w", index, err)
		}
		wire.Parts = append(wire.Parts, raw)
	}
	return json.Marshal(wire)
}

func (i *DecisionInput) UnmarshalJSON(data []byte) error {
	var wire struct {
		Text  string            `json:"text"`
		JSON  json.RawMessage   `json:"json"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode decision input: %w", err)
	}
	decoded := DecisionInput{Text: wire.Text, JSON: wire.JSON}
	for index, raw := range wire.Parts {
		part, err := message.UnmarshalPart(raw)
		if err != nil {
			return fmt.Errorf("decision input part %d: %w", index, err)
		}
		decoded.Parts = append(decoded.Parts, part)
	}
	*i = decoded
	return nil
}

func (i DecisionInput) Clone() DecisionInput {
	clone := i
	clone.JSON = append(json.RawMessage(nil), i.JSON...)
	clone.Parts = make([]message.Part, len(i.Parts))
	for index, part := range i.Parts {
		if part == nil {
			continue
		}
		normalized, err := message.NormalizePart(part)
		if err != nil {
			clone.Parts[index] = part
			continue
		}
		clone.Parts[index] = normalized.Clone()
	}
	return clone
}

// Validate enforces the one-carrier rule and the populated carrier's own
// contract: JSON must be one object or array, and parts must be the kinds a
// decision input can carry (text and image).
func (i DecisionInput) Validate() error {
	hasJSON := len(bytes.TrimSpace(i.JSON)) > 0
	carriers := 0
	if i.Text != "" {
		carriers++
	}
	if hasJSON {
		carriers++
	}
	if len(i.Parts) > 0 {
		carriers++
	}
	switch {
	case carriers == 0:
		return fmt.Errorf("decision input is required")
	case carriers > 1:
		return fmt.Errorf(
			"decision input must carry exactly one of text, json, or parts",
		)
	}
	if hasJSON {
		trimmed := bytes.TrimSpace(i.JSON)
		if !json.Valid(trimmed) {
			return fmt.Errorf("decision input json is not valid JSON")
		}
		if trimmed[0] != '{' && trimmed[0] != '[' {
			return fmt.Errorf("decision input json must be an object or array")
		}
	}
	for index, part := range i.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return fmt.Errorf("decision input part %d: %w", index, err)
		}
		if _, ok := decisionInputPartFields[normalized.Kind()]; !ok {
			return fmt.Errorf(
				"decision input part %d: %s evidence is not supported",
				index,
				normalized.Kind(),
			)
		}
		if err := normalized.Validate(); err != nil {
			return fmt.Errorf("decision input part %d: %w", index, err)
		}
	}
	return nil
}

func (i DecisionInput) appendActiveFields(fields []FieldID) []FieldID {
	switch {
	case i.Text != "":
		return append(fields, FieldDecisionInputText)
	case len(bytes.TrimSpace(i.JSON)) > 0:
		return append(fields, FieldDecisionInputJSON)
	}
	kinds := make(map[message.PartKind]bool, len(i.Parts))
	for _, part := range i.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		kinds[normalized.Kind()] = true
	}
	return appendPartFields(fields, decisionInputPartFields, kinds)
}

// decisionInputPartFields maps a content kind onto the ledger field a decision
// input activates. Only text and image have rows: every decision wire carries
// text and images and nothing else, so a request carrying another kind is
// rejected rather than compiled and dropped.
var decisionInputPartFields = map[message.PartKind]FieldID{
	message.PartText:  FieldDecisionInputPartText,
	message.PartImage: FieldDecisionInputPartImage,
}

// DecisionInputPartField returns the ledger field one input part kind
// activates. The boolean is false for kinds a decision input cannot carry —
// the caller sees "not a decision input kind" rather than an empty field.
func DecisionInputPartField(kind message.PartKind) (FieldID, bool) {
	field, ok := decisionInputPartFields[kind]
	return field, ok
}

// DecisionQuestionKind is the kind of one decision question. The System One
// wire spells the predicate kind "noul"; the mapping lives in that driver.
type DecisionQuestionKind string

const (
	// DecisionQuestionPredicate estimates how likely a statement about the
	// input is true. The answer carries one probability.
	DecisionQuestionPredicate DecisionQuestionKind = "predicate"
	// DecisionQuestionChoice picks one value out of an ordered option list.
	DecisionQuestionChoice DecisionQuestionKind = "choice"
	// DecisionQuestionScore rates the input against ordered levels.
	DecisionQuestionScore DecisionQuestionKind = "score"
)

func (k DecisionQuestionKind) Validate() error {
	switch k {
	case DecisionQuestionPredicate, DecisionQuestionChoice, DecisionQuestionScore:
		return nil
	}
	return fmt.Errorf("unknown decision question kind %q", k)
}

// DecisionChoiceValueKind distinguishes the two typed choice values. A string
// and a boolean with the same text are distinct values, and a choice request
// may mix them.
type DecisionChoiceValueKind string

const (
	DecisionChoiceValueString  DecisionChoiceValueKind = "string"
	DecisionChoiceValueBoolean DecisionChoiceValueKind = "boolean"
)

// DecisionChoiceValue is one typed choice value: exactly the field Kind names
// is meaningful. The struct form (rather than an any) keeps the value
// comparable and JSON-stable.
type DecisionChoiceValue struct {
	Kind    DecisionChoiceValueKind `json:"kind"`
	String  string                  `json:"string,omitempty"`
	Boolean bool                    `json:"boolean,omitempty"`
}

// NewDecisionStringChoice builds a string choice value, including the empty
// string, which is a legal option value.
func NewDecisionStringChoice(value string) DecisionChoiceValue {
	return DecisionChoiceValue{Kind: DecisionChoiceValueString, String: value}
}

// NewDecisionBooleanChoice builds a boolean choice value.
func NewDecisionBooleanChoice(value bool) DecisionChoiceValue {
	return DecisionChoiceValue{Kind: DecisionChoiceValueBoolean, Boolean: value}
}

func (v DecisionChoiceValue) Validate() error {
	switch v.Kind {
	case DecisionChoiceValueString:
		if v.Boolean {
			return fmt.Errorf("string choice value carries a boolean")
		}
	case DecisionChoiceValueBoolean:
		if v.String != "" {
			return fmt.Errorf("boolean choice value carries a string")
		}
	default:
		return fmt.Errorf("unknown choice value kind %q", v.Kind)
	}
	return nil
}

// Describe renders the value for error messages and logs. It is not a wire
// encoding: the distinction between a string and a boolean is carried by Kind.
func (v DecisionChoiceValue) Describe() string {
	switch v.Kind {
	case DecisionChoiceValueString:
		return fmt.Sprintf("%q", v.String)
	case DecisionChoiceValueBoolean:
		return fmt.Sprintf("%t", v.Boolean)
	}
	return fmt.Sprintf("<unknown kind %q>", v.Kind)
}

// DecisionChoice is one option of a choice question.
type DecisionChoice struct {
	Value       DecisionChoiceValue `json:"value"`
	Description string              `json:"description,omitempty"`
}

// DecisionLevel is one ordered level of a score question.
type DecisionLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// DecisionQuestion is one question about the request's input. Name is the
// answer's locator and must be non-empty and unique within a request: the
// wires disagree on identity (the OpenAI wire contracts answers by array
// position and treats name as an optional label; System One keys answers by
// caller-chosen id and has no null), so the canonical layer requires the
// strictest form both wires can express losslessly.
type DecisionQuestion struct {
	Name         string               `json:"name"`
	Kind         DecisionQuestionKind `json:"kind"`
	Instructions string               `json:"instructions"`
	Choices      []DecisionChoice     `json:"choices,omitempty"`
	Levels       []DecisionLevel      `json:"levels,omitempty"`
}

func (q DecisionQuestion) Clone() DecisionQuestion {
	clone := q
	clone.Choices = append([]DecisionChoice(nil), q.Choices...)
	clone.Levels = append([]DecisionLevel(nil), q.Levels...)
	return clone
}

// Validate checks one question against its kind: instructions must ask
// something, a choice question carries at least two unique options and no
// levels, a score question at least two uniquely labelled levels and no
// choices, and a predicate question neither.
func (q DecisionQuestion) Validate() error {
	if q.Name == "" {
		return fmt.Errorf("decision question name is required")
	}
	if err := q.Kind.Validate(); err != nil {
		return err
	}
	if q.Instructions == "" {
		return fmt.Errorf(
			"decision question %q instructions are required", q.Name)
	}
	switch q.Kind {
	case DecisionQuestionPredicate:
		if len(q.Choices) > 0 || len(q.Levels) > 0 {
			return fmt.Errorf(
				"decision question %q is a predicate and cannot carry "+
					"choices or levels",
				q.Name,
			)
		}
	case DecisionQuestionChoice:
		if err := validateDecisionChoices(q); err != nil {
			return err
		}
		if len(q.Levels) > 0 {
			return fmt.Errorf(
				"decision question %q is a choice and cannot carry levels",
				q.Name,
			)
		}
	case DecisionQuestionScore:
		if err := validateDecisionLevels(q); err != nil {
			return err
		}
		if len(q.Choices) > 0 {
			return fmt.Errorf(
				"decision question %q is a score and cannot carry choices",
				q.Name,
			)
		}
	}
	return nil
}

// decisionMinOptions is the smallest option or level count a question may
// declare. It is the one bound both wires agree on, so the canonical layer
// enforces it; provider-specific upper bounds (255 options, 10 levels today)
// stay in the drivers, where a widened vendor limit costs no core change.
const decisionMinOptions = 2

func validateDecisionChoices(q DecisionQuestion) error {
	if len(q.Choices) < decisionMinOptions {
		return fmt.Errorf(
			"decision question %q needs at least %d choices, got %d",
			q.Name,
			decisionMinOptions,
			len(q.Choices),
		)
	}
	seen := make(map[DecisionChoiceValue]struct{}, len(q.Choices))
	for index, choice := range q.Choices {
		if err := choice.Value.Validate(); err != nil {
			return fmt.Errorf(
				"decision question %q choice %d: %w", q.Name, index, err)
		}
		if _, ok := seen[choice.Value]; ok {
			return fmt.Errorf(
				"decision question %q has duplicate choice %s",
				q.Name,
				choice.Value.Describe(),
			)
		}
		seen[choice.Value] = struct{}{}
	}
	return nil
}

func validateDecisionLevels(q DecisionQuestion) error {
	if len(q.Levels) < decisionMinOptions {
		return fmt.Errorf(
			"decision question %q needs at least %d levels, got %d",
			q.Name,
			decisionMinOptions,
			len(q.Levels),
		)
	}
	seen := make(map[string]struct{}, len(q.Levels))
	for index, level := range q.Levels {
		if level.Label == "" {
			return fmt.Errorf(
				"decision question %q level %d has no label", q.Name, index)
		}
		if _, ok := seen[level.Label]; ok {
			return fmt.Errorf(
				"decision question %q has duplicate level label %q",
				q.Name,
				level.Label,
			)
		}
		seen[level.Label] = struct{}{}
	}
	return nil
}

// DecisionRequest is one unary decision call: one input, an ordered list of
// questions answered in the same request. Multi-question requests are the
// shape both wires share, and splitting one into several calls is not a
// canonical transformation — it changes latency, billing, and refusal
// semantics.
type DecisionRequest struct {
	Input     DecisionInput      `json:"input"`
	Questions []DecisionQuestion `json:"questions"`
	// ModelHint is an optional per-call model preference consumed by route
	// selection, exactly as in generate requests. It is not a ledger field:
	// the compiler never sees it.
	ModelHint string `json:"model_hint,omitempty"`
	// RequestMetadata carries opaque caller/host metadata whose keys and
	// values are deployment-defined. It is a ledger field like generate's.
	RequestMetadata map[string]string `json:"request_metadata,omitempty"`
	Extensions      Extensions        `json:"-" ledger:"extension"`
}

func (r DecisionRequest) Clone() DecisionRequest {
	clone := r
	clone.Input = r.Input.Clone()
	clone.Questions = make([]DecisionQuestion, len(r.Questions))
	for index, question := range r.Questions {
		clone.Questions[index] = question.Clone()
	}
	if r.RequestMetadata != nil {
		clone.RequestMetadata = maps.Clone(r.RequestMetadata)
	}
	clone.Extensions = r.Extensions.Clone()
	return clone
}

// Validate checks the whole request: one input carrier, at least one question,
// and question names that are non-empty and unique — the locator every
// consumer (including refusal handling) reads without a position fallback.
func (r DecisionRequest) Validate() error {
	if err := r.Input.Validate(); err != nil {
		return err
	}
	if len(r.Questions) == 0 {
		return fmt.Errorf("decision questions are required")
	}
	seen := make(map[string]struct{}, len(r.Questions))
	for index, question := range r.Questions {
		if err := question.Validate(); err != nil {
			return fmt.Errorf("decision question %d: %w", index, err)
		}
		if _, ok := seen[question.Name]; ok {
			return fmt.Errorf(
				"decision question %d repeats name %q", index, question.Name)
		}
		seen[question.Name] = struct{}{}
	}
	return r.Extensions.Validate()
}

func (r DecisionRequest) ActiveFields() []FieldID {
	var fields []FieldID
	fields = r.Input.appendActiveFields(fields)
	if len(r.Questions) > 0 {
		fields = append(fields,
			FieldDecisionQuestionName,
			FieldDecisionQuestionKind,
			FieldDecisionQuestionInstructions,
		)
	}
	var hasChoices, hasLevels bool
	for _, question := range r.Questions {
		switch question.Kind {
		case DecisionQuestionChoice:
			hasChoices = true
		case DecisionQuestionScore:
			hasLevels = true
		case DecisionQuestionPredicate:
		}
	}
	if hasChoices {
		fields = append(fields, FieldDecisionQuestionChoices)
	}
	if hasLevels {
		fields = append(fields, FieldDecisionQuestionLevels)
	}
	if len(r.RequestMetadata) > 0 {
		fields = append(fields, FieldDecisionRequestMetadata)
	}
	return r.Extensions.AppendActiveFields(fields)
}
