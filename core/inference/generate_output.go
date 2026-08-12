package inference

import (
	"encoding/json"
	"fmt"
)

// ProviderOutput is a typed, provider-owned structured value attached to a
// Generate response but deliberately kept out of Message. Message is the only
// channel that becomes conversation context on the next request, so
// ProviderOutputs are observational data (citations, search-call records,
// code-interpreter output, MCP metadata) and never auto-injected into a later
// model request. Provider packages implement this interface for each output
// family; the response envelope carries any number of them.
type ProviderOutput interface {
	// ProviderID addresses the provider that produced the output.
	ProviderID() string
	// ExtensionID names the output family within the provider.
	ExtensionID() string
	Validate() error
	Clone() ProviderOutput
}

// ProviderOutputs is the ordered collection of provider outputs attached to a
// Generate response or stream event.
type ProviderOutputs []ProviderOutput

type providerOutputEnvelope struct {
	Provider  string         `json:"provider"`
	Extension string         `json:"extension"`
	Value     ProviderOutput `json:"value"`
}

// MarshalJSON wraps every output with its provider/extension identity so wire
// consumers can distinguish output families without type-switching on the Go
// value. Unmarshal is intentionally not supported: outputs are produced by
// provider packages, not decoded from generic response JSON.
func (outputs ProviderOutputs) MarshalJSON() ([]byte, error) {
	entries := make([]providerOutputEnvelope, len(outputs))
	for i, output := range outputs {
		if isNilValue(output) {
			return nil, fmt.Errorf("provider output %d is nil", i)
		}
		entries[i] = providerOutputEnvelope{
			Provider:  output.ProviderID(),
			Extension: output.ExtensionID(),
			Value:     output,
		}
	}
	return json.Marshal(entries)
}

func (outputs ProviderOutputs) Clone() ProviderOutputs {
	if outputs == nil {
		return nil
	}
	cloned := make(ProviderOutputs, len(outputs))
	for i, output := range outputs {
		if !isNilValue(output) {
			cloned[i] = output.Clone()
		}
	}
	return cloned
}

func (outputs ProviderOutputs) Validate() error {
	for i, output := range outputs {
		if isNilValue(output) {
			return fmt.Errorf("provider output %d is nil", i)
		}
		if !extensionIDPattern.MatchString(output.ProviderID()) ||
			!extensionIDPattern.MatchString(output.ExtensionID()) {
			return fmt.Errorf("provider output %d has invalid identity", i)
		}
		if err := output.Validate(); err != nil {
			return fmt.Errorf("provider output %q: %w",
				output.ProviderID()+"/"+output.ExtensionID(), err)
		}
	}
	return nil
}

// Replace inserts output, replacing any existing entry with the same
// provider/extension identity. Streaming providers use this to publish a
// cumulative snapshot per output family, matching Usage's replace semantics.
func (outputs *ProviderOutputs) Replace(output ProviderOutput) {
	if isNilValue(output) {
		return
	}
	for i, existing := range *outputs {
		if !isNilValue(existing) &&
			existing.ProviderID() == output.ProviderID() &&
			existing.ExtensionID() == output.ExtensionID() {
			(*outputs)[i] = output
			return
		}
	}
	*outputs = append(*outputs, output)
}

// WebSearchCall records one provider-executed web search invocation. Hosted
// search tools run inside the provider, so these calls never need a
// ToolResultPart round-trip; they exist for tracing and audit.
type WebSearchCall struct {
	ID      string   `json:"id,omitempty"`
	Status  string   `json:"status,omitempty"`
	Action  string   `json:"action,omitempty"` // search | open_page | find_in_page
	Queries []string `json:"queries,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Citation is one source reference attached to generated text. StartIndex and
// EndIndex are character offsets into the corresponding TextPart when the
// provider reports them (OpenAI does; Ark currently does not).
type Citation struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	StartIndex  *int64 `json:"start_index,omitempty"`
	EndIndex    *int64 `json:"end_index,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	PublishTime string `json:"publish_time,omitempty"`
}
