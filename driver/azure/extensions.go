package azure

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
)

// driverID namespaces every extension this package defines.
const driverID = "azure"

const extensionGenerate = "generate_options"

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return driverID
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// GenerateOptions carries OpenAI Responses API settings that have no
// canonical representation.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "openai".
	Provider string `json:"-"`
	// WebSearch attaches OpenAI's hosted web_search tool.
	WebSearch *GenerateWebSearch `json:"web_search,omitempty"`
}

// GenerateWebSearch configures the hosted web_search tool.
type GenerateWebSearch struct {
	// SearchContextSize controls how much web search context the model can
	// consume: "low", "medium", or "high". Empty keeps the provider default.
	SearchContextSize string `json:"search_context_size,omitempty"`
	// AllowedDomains restricts search results to the listed domains and their
	// subdomains. Empty allows all domains.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// UserLocation localizes results.
	UserLocation GenerateWebSearchLocation `json:"user_location,omitempty"`
	// ExternalWebAccess controls whether the model may load pages that are not
	// directly search-engine results.
	ExternalWebAccess *bool `json:"external_web_access,omitempty"`
	// ReturnTokenBudget controls the returned-token budget for reasoning web
	// search runs: "default" or "unlimited".
	ReturnTokenBudget string `json:"return_token_budget,omitempty"`
	// ToolChoice controls whether search is optional (auto) or mandatory
	// (required). Nil behaves as auto.
	ToolChoice *GenerateWebSearchToolChoice `json:"tool_choice,omitempty"`
}

// GenerateWebSearchToolChoice selects the web search tool choice mode.
type GenerateWebSearchToolChoice struct {
	// Required forces the model to run web search when true.
	Required bool `json:"required"`
}

// GenerateWebSearchLocation is the approximate location for web search.
type GenerateWebSearchLocation struct {
	City     string `json:"city,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.WebSearch != nil {
		fields = append(fields, "web_search")
		if o.WebSearch.ToolChoice != nil {
			fields = append(fields, "web_search_tool_choice")
		}
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	if search := o.WebSearch; search != nil {
		switch search.SearchContextSize {
		case "", "low", "medium", "high":
		default:
			return fmt.Errorf(
				"web_search search_context_size must be low, medium, or high, not %q",
				search.SearchContextSize,
			)
		}
		switch search.ReturnTokenBudget {
		case "", "default", "unlimited":
		default:
			return fmt.Errorf(
				"web_search return_token_budget must be default or unlimited, not %q",
				search.ReturnTokenBudget,
			)
		}
		if slices.Contains(search.AllowedDomains, "") {
			return fmt.Errorf("web_search allowed_domains entries must not be empty")
		}
	}
	return nil
}

func (o GenerateOptions) Clone() inference.Extension {
	if o.WebSearch != nil {
		search := *o.WebSearch
		search.AllowedDomains = append([]string(nil), search.AllowedDomains...)
		search.ExternalWebAccess = clonePointer(search.ExternalWebAccess)
		search.ToolChoice = clonePointer(search.ToolChoice)
		o.WebSearch = &search
	}
	return o
}

// ---------------------------------------------------------------------------
// Consumption helpers.
// ---------------------------------------------------------------------------

func operationExtensions[T inference.Extension](
	extensions inference.Extensions,
) (T, []inference.Extension) {
	var options T
	var other []inference.Extension
	for _, extension := range extensions {
		if extension == nil {
			continue
		}
		if typed, ok := extension.(T); ok {
			options = typed
			continue
		}
		other = append(other, extension)
	}
	return options, other
}

func rejectOtherExtensions(
	operation string,
	other []inference.Extension,
	ledger *ledger,
) {
	for _, extension := range other {
		reason := fmt.Sprintf(
			"extension %q does not apply to %s",
			extension.ExtensionID(),
			operation,
		)
		for _, field := range extension.ActiveFields() {
			ledger.reject(field.Qualify(extension), reason)
		}
	}
}
