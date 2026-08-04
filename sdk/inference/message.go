package inference

import (
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/message"
)

// InputContent is the request-side content with execution intent attached.
// The embedded [message.Content] carries the raw parts; Intent is the
// caller-declared "how to run this turn" envelope (text, image, audio,
// video, tools, ...). Each operation accepts only the intents it can
// compile natively; foreign intents are a structured rejection, never a
// silent drop.
type InputContent struct {
	message.Content
	Intent Intent `json:"intent"`
}

// MarshalJSON is explicit because message.Content implements json.Marshaler; without
// this method the embedded marshaler would otherwise hide Intent.
func (c InputContent) MarshalJSON() ([]byte, error) {
	contentData, err := json.Marshal(c.Content)
	if err != nil {
		return nil, err
	}
	var content struct {
		Parts json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(contentData, &content); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Parts  json.RawMessage `json:"parts"`
		Intent Intent          `json:"intent"`
	}{Parts: content.Parts, Intent: c.Intent})
}

func (c *InputContent) UnmarshalJSON(data []byte) error {
	var wire struct {
		Parts  json.RawMessage `json:"parts"`
		Intent Intent          `json:"intent"`
	}
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	contentData, err := json.Marshal(struct {
		Parts json.RawMessage `json:"parts"`
	}{Parts: wire.Parts})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(contentData, &c.Content); err != nil {
		return err
	}
	c.Intent = wire.Intent
	return nil
}

func (c InputContent) Clone() InputContent {
	c.Content = c.Content.Clone()
	c.Intent = c.Intent.Clone()
	return c
}

func (c InputContent) Validate() error {
	if err := c.Content.Validate(); err != nil {
		return fmt.Errorf("input content: %w", err)
	}
	return c.Intent.Validate()
}
