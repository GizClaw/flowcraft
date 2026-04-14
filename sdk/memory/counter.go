package memory

import (
	"fmt"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/model"
	tiktoken "github.com/pkoukk/tiktoken-go"
)

// TokenCounter estimates or calculates the token count for text and messages.
type TokenCounter interface {
	Count(text string) int
	CountMessages(msgs []model.Message) int
}

// EstimateCounter uses a heuristic: ~4 ASCII chars/token, ~1.5 CJK chars/token.
type EstimateCounter struct{}

func (c *EstimateCounter) Count(text string) int {
	var ascii, cjk int
	for _, r := range text {
		if r <= 127 {
			ascii++
		} else if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) {
			cjk++
		} else {
			cjk++
		}
	}
	return ascii/4 + (cjk*2+2)/3
}

func (c *EstimateCounter) CountMessages(msgs []model.Message) int {
	total := 0
	for _, msg := range msgs {
		total += 4
		for _, p := range msg.Parts {
			if p.Type == model.PartText {
				total += c.Count(p.Text)
			}
		}
	}
	return total
}

// TiktokenCounter uses tiktoken-go for precise BPE token counting.
type TiktokenCounter struct {
	tk *tiktoken.Tiktoken
}

// NewTiktokenCounter creates a TiktokenCounter for the given model name
// (e.g. "gpt-4o", "gpt-4", "gpt-3.5-turbo"). Falls back to cl100k_base
// encoding if the model is not recognized.
func NewTiktokenCounter(model string) (*TiktokenCounter, error) {
	tk, err := tiktoken.EncodingForModel(model)
	if err != nil {
		tk, err = tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
		if err != nil {
			return nil, fmt.Errorf("tiktoken: init encoding: %w", err)
		}
	}
	return &TiktokenCounter{tk: tk}, nil
}

// NewTiktokenCounterFromEncoding creates a TiktokenCounter for a specific
// encoding name (e.g. "cl100k_base", "o200k_base").
func NewTiktokenCounterFromEncoding(encoding string) (*TiktokenCounter, error) {
	tk, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, fmt.Errorf("tiktoken: get encoding %q: %w", encoding, err)
	}
	return &TiktokenCounter{tk: tk}, nil
}

func (c *TiktokenCounter) Count(text string) int {
	return len(c.tk.Encode(text, nil, nil))
}

func (c *TiktokenCounter) CountMessages(msgs []model.Message) int {
	total := 0
	for _, msg := range msgs {
		total += 4
		total += c.Count(string(msg.Role))
		for _, p := range msg.Parts {
			switch p.Type {
			case model.PartText:
				total += c.Count(p.Text)
			case model.PartToolCall:
				if p.ToolCall != nil {
					total += c.Count(p.ToolCall.Name) + c.Count(p.ToolCall.Arguments)
				}
			case model.PartToolResult:
				if p.ToolResult != nil {
					total += c.Count(p.ToolResult.Content)
				}
			}
		}
	}
	total += 3
	return total
}
