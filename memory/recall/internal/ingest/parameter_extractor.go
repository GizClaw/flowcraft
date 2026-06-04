package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParameterProposalSchema is the typed extractor contract for slot/config
// assertions. It contains only source surface fields and evidence hints; the
// compiler owns canonical names, operation, value kind, units, normalization,
// merge keys, validity, and final content.
const ParameterProposalSchema = `{
  "type": "object",
  "properties": {
    "proposals": {
      "type": "array",
      "maxItems": 32,
      "items": {
        "type": "object",
        "properties": {
          "family": {"type": "string", "enum": ["parameter_slot"]},
          "source_ids": {"type": "array", "items": {"type": "string"}},
          "quote": {"type": "string"},
          "owner": {"type": "string"},
          "name_surface": {"type": "string"},
          "operation_surface": {"type": "string"},
          "value_surface": {"type": "string"},
          "normalized_value_hint": {"type": "string"},
          "old_value_surface": {"type": "string"},
          "condition_surface": {"type": "string"},
          "operator_surface": {"type": "string"},
          "effective_time_surface": {"type": "string"},
          "confirmation_source_ids": {"type": "array", "items": {"type": "string"}},
          "confirmation_quote": {"type": "string"}
        },
        "required": ["family", "source_ids", "quote", "owner", "name_surface", "operation_surface", "value_surface", "normalized_value_hint", "old_value_surface", "condition_surface", "operator_surface", "effective_time_surface", "confirmation_source_ids", "confirmation_quote"],
        "additionalProperties": false
      }
    },
    "overflow": {"type": "boolean"}
  },
  "required": ["proposals"],
  "additionalProperties": false
}`

type ParameterProposal struct {
	Family                string   `json:"family,omitempty"`
	SourceIDs             []string `json:"source_ids"`
	Quote                 string   `json:"quote"`
	Owner                 string   `json:"owner"`
	NameSurface           string   `json:"name_surface"`
	OperationSurface      string   `json:"operation_surface"`
	ValueSurface          string   `json:"value_surface"`
	NormalizedValueHint   string   `json:"normalized_value_hint"`
	OldValueSurface       string   `json:"old_value_surface"`
	ConditionSurface      string   `json:"condition_surface"`
	OperatorSurface       string   `json:"operator_surface"`
	EffectiveTimeSurface  string   `json:"effective_time_surface"`
	ConfirmationSourceIDs []string `json:"confirmation_source_ids"`
	ConfirmationQuote     string   `json:"confirmation_quote"`
}

type parameterProposalList struct {
	Proposals []ParameterProposal `json:"proposals"`
	Overflow  bool                `json:"overflow,omitempty"`
}

const ParameterProposalSystemPrompt = `You extract parameter_slot proposals from explicit source evidence.

## Output
A JSON object {"proposals":[...]} matching the supplied schema.

## Source Authority
<extractable_evidence> may be cited and used to promote new parameter facts.
Context and existing fact hints may disambiguate only; never cite them.

## Proposal Rules
- Extract explicit parameters, settings, flags, thresholds, options, metrics, limits, configuration values, model hyperparameters, and tool arguments.
- Parameter discovery is semantic: do not rely on a fixed dictionary of names. Extract code-like names such as top_p, maxTokens, max-tokens, and CJK/mixed text names when the source uses them as slots.
- Fill only source surfaces and evidence hints. Do not emit canonical names,
  namespace paths, value kinds, units, merge keys, or final facts.
- Fill name_surface and value_surface from the source. normalized_value_hint is only a hint.
- Use operation_surface and operator_surface only for text copied verbatim from
  the source; deterministic code maps symbols to canonical policy.
- If a setting is conditional, keep the condition in condition_surface and do not convert it into an unconditional value.
- For dialogue confirmations, set operation_surface to the source confirmation
  operation, keep the pending slot question in quote/source_ids, and put the
  answer evidence in confirmation_source_ids and confirmation_quote.
- Cite source_ids from extractable_evidence only and copy quote verbatim from that source.
- Return {"proposals": []} when no directly supported parameter exists.`

func parseParameterProposalReply(body []byte) ([]proposalCandidate, error) {
	var parsed parameterProposalList
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]proposalCandidate, 0, len(parsed.Proposals))
	for i := range parsed.Proposals {
		p := parsed.Proposals[i]
		if strings.TrimSpace(p.Family) != "" && semanticProposalFamily(proposalFamily(p.Family)) != proposalFamilyParameter {
			return nil, fmt.Errorf("proposal[%d] family %q does not match extractor family %q", i, p.Family, proposalFamilyParameter)
		}
		out = append(out, proposalCandidate{Family: proposalFamilyParameter, Parameter: &p})
	}
	return out, nil
}
