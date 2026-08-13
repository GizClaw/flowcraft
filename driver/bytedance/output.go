package bytedance

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
)

const extensionWebSearch = "web_search"

// WebSearchOutput is the provider-owned structured output of Ark's hosted
// web_search tool. Search calls execute inside Ark, so this data lives outside
// Message and is never sent back to the model implicitly.
type WebSearchOutput struct {
	Calls     []inference.WebSearchCall `json:"calls,omitempty"`
	Citations []inference.Citation      `json:"citations,omitempty"`
}

func (WebSearchOutput) ProviderID() string  { return driverID }
func (WebSearchOutput) ExtensionID() string { return extensionWebSearch }

func (o WebSearchOutput) Validate() error {
	for i, citation := range o.Citations {
		if citation.URL == "" {
			return fmt.Errorf("citation %d has no url", i)
		}
	}
	return nil
}

func (o WebSearchOutput) Clone() inference.ProviderOutput {
	o.Calls = append([]inference.WebSearchCall(nil), o.Calls...)
	o.Citations = append([]inference.Citation(nil), o.Citations...)
	return o
}

func webSearchProviderOutput(
	calls []inference.WebSearchCall,
	citations []inference.Citation,
) *WebSearchOutput {
	if len(calls) == 0 && len(citations) == 0 {
		return nil
	}
	return &WebSearchOutput{
		Calls:     append([]inference.WebSearchCall(nil), calls...),
		Citations: append([]inference.Citation(nil), citations...),
	}
}
