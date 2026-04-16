package llm

import (
	"encoding/json"
	"fmt"
)

// RoundConfig configures the LLM call parameters for one round.
// Board I/O (system prompt injection, output routing, etc.) is the caller's responsibility.
type RoundConfig struct {
	Model       string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	JSONMode    bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking    bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

// RoundConfigFromMap parses RoundConfig from a generic map via JSON round-trip.
func RoundConfigFromMap(m map[string]any) (RoundConfig, error) {
	var cfg RoundConfig
	if m == nil {
		return cfg, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("llm: marshal config map: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("llm: unmarshal config: %w", err)
	}
	return cfg, nil
}
