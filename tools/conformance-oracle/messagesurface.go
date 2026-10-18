package main

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
)

// The rest of the message contract, after the media DTOs: the part union, the
// message/content codec, the tool DTOs and the schema builders.
//
// Same rule as the media half -- every row is a call into core with the outcome
// computed, never written down -- with one addition: a row records *which
// stage* answered, because the codec has three (decode, validate, write) and
// core moves between them. A part decodes, then validates, then writes; a
// message does the same; a tool DTO has no decode at all (callers build it), so
// it only validates and writes.
//
// `text_from` is computed, not declared: a refusal is marked
// `encoding/json` when its text carries one of that package's phrases. Those
// are the rows the port does not reproduce word for word (it keeps its own
// decoder's sentences); the replay holds them to being refusals. Everything
// else -- core's own sentences, and the `json: unknown field %q` shape -- is
// compared exactly.

type partRow struct {
	Document string `json:"document"`

	Decode         string `json:"decode"` // ok | error
	DecodeError    string `json:"decode_error,omitempty"`
	DecodeTextFrom string `json:"decode_text_from,omitempty"`
	Kind           string `json:"kind,omitempty"`

	Validate      string `json:"validate,omitempty"` // ok | error, when decoded
	ValidateError string `json:"validate_error,omitempty"`

	Wire         string `json:"wire,omitempty"`
	WireError    string `json:"wire_error,omitempty"`
	WireTextFrom string `json:"wire_text_from,omitempty"`
}

type messageRow struct {
	Document string `json:"document"`

	Decode         string `json:"decode"`
	DecodeError    string `json:"decode_error,omitempty"`
	DecodeTextFrom string `json:"decode_text_from,omitempty"`
	Role           string `json:"role,omitempty"`

	Validate      string `json:"validate,omitempty"`
	ValidateError string `json:"validate_error,omitempty"`

	Wire         string `json:"wire,omitempty"`
	WireError    string `json:"wire_error,omitempty"`
	WireTextFrom string `json:"wire_text_from,omitempty"`
}

// toolRow is one tool DTO sample: `definition`, `call` or `result`. The fields
// it uses are the ones its op needs; the outcome columns are shared.
type toolRow struct {
	Op          string `json:"op"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	InputSchema string `json:"input_schema,omitempty"`
	ID          string `json:"id,omitempty"`
	Arguments   string `json:"arguments,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	Text        string `json:"text,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`

	Validate      string `json:"validate,omitempty"`
	ValidateError string `json:"validate_error,omitempty"`

	Wire         string `json:"wire,omitempty"`
	WireError    string `json:"wire_error,omitempty"`
	WireTextFrom string `json:"wire_text_from,omitempty"`
}

// propSample is one property fed to the schema builder. `items`, `properties`
// and `default` are JSON text because core's helpers take a map, an `any` and a
// `...string`: the same call is rebuilt on the port's side from this.
type propSample struct {
	Kind        string   `json:"kind"` // property | array | object | string_map | default | enum
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Items       string   `json:"items,omitempty"`
	Properties  string   `json:"properties,omitempty"`
	Default     string   `json:"default,omitempty"`
	Values      []string `json:"values,omitempty"`
}

type schemaRow struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Props       []propSample `json:"props"`
	Required    []string     `json:"required,omitempty"`
	Closed      bool         `json:"closed,omitempty"`

	Schema        string `json:"schema"`
	Validate      string `json:"validate"`
	ValidateError string `json:"validate_error,omitempty"`
	Wire          string `json:"wire"`
}

func partOutcome(document string) partRow {
	row := partRow{Document: document}
	part, err := message.UnmarshalPart([]byte(document))
	if err != nil {
		row.Decode = "error"
		row.DecodeError = err.Error()
		row.DecodeTextFrom = textFrom(err.Error())
		return row
	}
	row.Decode = "ok"
	row.Kind = string(part.Kind())
	if err := part.Validate(); err != nil {
		row.Validate = "error"
		row.ValidateError = err.Error()
	} else {
		row.Validate = "ok"
	}
	wire, err := message.MarshalPart(part)
	if err != nil {
		row.WireError = err.Error()
		row.WireTextFrom = textFrom(err.Error())
		return row
	}
	row.Wire = string(wire)
	return row
}

func messageOutcome(document string) messageRow {
	row := messageRow{Document: document}
	var msg message.Message
	if err := json.Unmarshal([]byte(document), &msg); err != nil {
		row.Decode = "error"
		row.DecodeError = err.Error()
		row.DecodeTextFrom = textFrom(err.Error())
		return row
	}
	row.Decode = "ok"
	row.Role = string(msg.Role)
	if err := msg.Validate(); err != nil {
		row.Validate = "error"
		row.ValidateError = err.Error()
	} else {
		row.Validate = "ok"
	}
	wire, err := json.Marshal(msg)
	if err != nil {
		row.WireError = err.Error()
		row.WireTextFrom = textFrom(err.Error())
		return row
	}
	row.Wire = string(wire)
	return row
}

// toolValue builds the sample's DTO; the bool says whether the row named an op
// this file knows.
func toolValue(row toolRow) (interface {
	Validate() error
}, bool) {
	switch row.Op {
	case "definition":
		return message.ToolDefinition{
			Name:        row.Name,
			Description: row.Description,
			InputSchema: rawOrNil(row.InputSchema),
		}, true
	case "call":
		return message.ToolCall{
			ID:        row.ID,
			Name:      row.Name,
			Arguments: rawOrNil(row.Arguments),
		}, true
	case "result":
		return message.ToolResult{
			CallID:  row.CallID,
			Content: message.NewTextContent(row.Text),
			IsError: row.IsError,
		}, true
	}
	return nil, false
}

func toolOutcome(row toolRow) toolRow {
	value, ok := toolValue(row)
	if !ok {
		row.Validate = "error"
		row.ValidateError = "unknown tool sample op " + row.Op
		return row
	}
	if err := value.Validate(); err != nil {
		row.Validate = "error"
		row.ValidateError = err.Error()
	} else {
		row.Validate = "ok"
	}
	wire, err := json.Marshal(value)
	if err != nil {
		row.WireError = err.Error()
		row.WireTextFrom = textFrom(err.Error())
		return row
	}
	row.Wire = string(wire)
	return row
}

func schemaOutcome(row schemaRow) schemaRow {
	props := make([]message.ToolPropertyDef, 0, len(row.Props))
	for _, sample := range row.Props {
		props = append(props, buildProperty(sample))
	}
	builder := message.DefineSchema(row.Name, row.Description, props...)
	if row.Closed {
		builder = builder.DisallowAdditionalProperties()
	}
	if len(row.Required) > 0 {
		builder = builder.Required(row.Required...)
	}
	definition := builder.Build()
	row.Schema = string(definition.InputSchema)
	if err := definition.Validate(); err != nil {
		row.Validate = "error"
		row.ValidateError = err.Error()
	} else {
		row.Validate = "ok"
	}
	wire, err := json.Marshal(definition)
	if err != nil {
		row.Validate = "error"
		row.ValidateError = err.Error()
		return row
	}
	row.Wire = string(wire)
	return row
}

func buildProperty(sample propSample) message.ToolPropertyDef {
	switch sample.Kind {
	case "array":
		return message.ToolArrayProperty(sample.Name, sample.Description, objectOf(sample.Items))
	case "object":
		return message.ToolObjectProperty(sample.Name, sample.Description, objectOf(sample.Properties))
	case "string_map":
		return message.ToolStringMapProperty(sample.Name, sample.Description)
	case "default":
		var value any
		_ = json.Unmarshal([]byte(sample.Default), &value)
		return message.ToolPropertyWithDefault(sample.Name, sample.Type, sample.Description, value)
	case "enum":
		return message.ToolEnumProperty(sample.Name, sample.Type, sample.Description, sample.Values...)
	default:
		return message.ToolProperty(sample.Name, sample.Type, sample.Description)
	}
}

func objectOf(document string) map[string]any {
	if document == "" {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		return nil
	}
	return value
}

func rawOrNil(document string) json.RawMessage {
	if document == "" {
		return nil
	}
	return json.RawMessage(document)
}

// textFrom names the writer of a refusal when it is not core: encoding/json's
// phrases are its own, and the port spells refusals in its own words. The list
// is deliberately short and specific -- `json: unknown field %q` is *not* on it,
// because that sentence is small enough to reproduce and the port does.
func textFrom(text string) string {
	for _, phrase := range []string{
		"json: cannot unmarshal",
		"json: error calling MarshalJSON",
		"invalid character ",
		"unexpected end of JSON input",
		"into Go value of type",
		"into Go struct field",
	} {
		if strings.Contains(text, phrase) {
			return "encoding/json"
		}
	}
	return ""
}

func writeSurface(contract *messageContract) {
	for _, document := range partSamples {
		contract.Parts = append(contract.Parts, partOutcome(document))
	}
	for _, document := range messageSamples {
		contract.Messages = append(contract.Messages, messageOutcome(document))
	}
	for _, sample := range toolSamples {
		contract.Tools = append(contract.Tools, toolOutcome(sample))
	}
	for _, sample := range schemaSamples {
		contract.Schemas = append(contract.Schemas, schemaOutcome(sample))
	}
}
