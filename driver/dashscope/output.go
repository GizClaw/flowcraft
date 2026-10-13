package dashscope

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// extensionWebSearch names the hosted-web-search output family. The spelling
// matches the other drivers' web-search outputs, so a consumer reads one name
// across providers.
const extensionWebSearch = "web_search"

// SearchOutput is the provider-executed web search record of one generation:
// the sources the platform's enable_search retrieved for the answer, decoded
// from output.search_info. The search runs server-side inside the platform, so
// there is no call/result round trip to represent — the citations are the
// whole record, and the answer text already carries the marker annotations
// when citation marking is on.
type SearchOutput struct {
	// Citations lists the retrieved sources in provider order.
	Citations []inference.Citation `json:"citations,omitempty"`
}

func (SearchOutput) ProviderID() string  { return providerID }
func (SearchOutput) ExtensionID() string { return extensionWebSearch }

func (o SearchOutput) Validate() error {
	for index, citation := range o.Citations {
		if citation.URL == "" {
			return fmt.Errorf("citation %d has no url", index)
		}
	}
	return nil
}

// Clone returns a deep copy: the citation slice is copied, and every pointer
// leaf inside a citation is the caller's value, not a shared one.
func (o SearchOutput) Clone() inference.ProviderOutput {
	citations := make([]inference.Citation, len(o.Citations))
	for index, citation := range o.Citations {
		citation.StartIndex = ptr.Clone(citation.StartIndex)
		citation.EndIndex = ptr.Clone(citation.EndIndex)
		citations[index] = citation
	}
	o.Citations = citations
	return o
}
