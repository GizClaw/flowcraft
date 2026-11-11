package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

type responseSchema struct {
	Type       string `json:"type"`
	Required   []string
	Properties map[string]struct {
		Type                 string `json:"type"`
		AdditionalProperties bool   `json:"additionalProperties"`
		Required             []string
		Properties           map[string]json.RawMessage `json:"properties"`
		Items                struct {
			Type                 string `json:"type"`
			AdditionalProperties bool   `json:"additionalProperties"`
			Required             []string
			Properties           map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	} `json:"properties"`
}

// TestResponseSchemaIsStrictCompatible pins the rule that broke OpenAI
// extraction: a strict json_schema response format must list every property in
// required ("'required' is required to be supplied and to be an array
// including every key in properties"). DeepSeek accepts the lax shape, so
// nothing caught this until a candidate model was measured -- the extraction
// A/B probe failed all 30 calls with 400 invalid_json_schema.
func TestResponseSchemaIsStrictCompatible(t *testing.T) {
	for _, strategy := range []FactStrategy{StrategySimple, StrategyRich} {
		for _, quota := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quota=%v", strategy, quota), func(t *testing.T) {
				var schema responseSchema
				if err := json.Unmarshal(schemaFor(strategy, quota), &schema); err != nil {
					t.Fatal(err)
				}
				facts := schema.Properties["facts"]
				if facts.Type != "array" {
					t.Fatalf("facts type = %q, want array", facts.Type)
				}
				item := facts.Items
				if item.Type != "object" || item.AdditionalProperties {
					t.Fatalf("item type/additionalProperties = %q/%v, want object/false",
						item.Type, item.AdditionalProperties)
				}
				if got, want := slices.Sorted(slices.Values(item.Required)), slices.Sorted(slices.Values(propertiesOf(item.Properties))); !equalStrings(got, want) {
					t.Fatalf("required = %v, want every property %v; strict structured outputs reject the schema otherwise", got, want)
				}
				for _, key := range []string{"text", "entities", "event_time"} {
					if _, ok := item.Properties[key]; !ok {
						t.Errorf("property %q is missing from the schema", key)
					}
				}
				if strategy == StrategyRich {
					for _, key := range []string{"predicate", "temporal_detail"} {
						if _, ok := item.Properties[key]; !ok {
							t.Errorf("rich property %q is missing from the schema", key)
						}
					}
				}
				// The quota's priority key is a required property like every
				// other one (the required check above covers it), and a schema
				// without the quota must not declare it: a provider enforcing
				// additionalProperties would reject a response that carries it.
				if _, ok := item.Properties["priority"]; ok != quota {
					t.Errorf("priority property present = %v, want %v", ok, quota)
				}
			})
		}
	}
}

// TestPromptExplainsEmptyValues pins the other half of strict compatibility: a
// required key the model cannot omit needs a documented empty form, and the
// prompt must stop calling a key optional.
func TestPromptExplainsEmptyValues(t *testing.T) {
	for _, prompt := range []struct {
		name string
		text string
	}{
		{"simple", factSystem},
		{"rich", factSystemRich},
		{"simple/quota", factSystemFor(StrategySimple, true, 64)},
		{"rich/quota", factSystemFor(StrategyRich, true, 64)},
	} {
		for _, needle := range []string{"every key is required", "empty array", "empty string", `Send []`, `Send ""`} {
			if !strings.Contains(prompt.text, needle) {
				t.Errorf("%s prompt does not explain how to express a missing value (%q)", prompt.name, needle)
			}
		}
		if strings.Contains(prompt.text, "(optional)") {
			t.Errorf("%s prompt still labels a key optional, contradicting the schema", prompt.name)
		}
	}
}

func propertiesOf(values map[string]json.RawMessage) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
