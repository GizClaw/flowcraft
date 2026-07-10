package openai

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	openaishared "github.com/GizClaw/flowcraft/sdkx/llm/openai/shared"
)

// TestProviderDefaultsToOpenAI guards the historical contract: direct
// openai.New callers (no wrapper) must keep seeing "openai" in their
// OTel / metrics labels.
func TestProviderDefaultsToOpenAI(t *testing.T) {
	c, _ := New("gpt-test", "k", "")
	if got := c.Provider(); got != "openai" {
		t.Fatalf("default provider = %q, want %q", got, "openai")
	}
}

// TestWithProviderNameOverrides exercises the sub-provider plumbing:
// azure/deepseek/qwen call WithProviderName so their traffic shows up
// under their own bucket instead of being aggregated under the upstream
// "openai" tag. Empty names must be silently ignored so a caller passing
// an unset config doesn't blank out the historical default.
func TestWithProviderNameOverrides(t *testing.T) {
	c, _ := New("gpt-test", "k", "")
	c.WithProviderName("qwen")
	if got := c.Provider(); got != "qwen" {
		t.Fatalf("after WithProviderName(qwen) = %q, want qwen", got)
	}

	c.WithProviderName("")
	if got := c.Provider(); got != "qwen" {
		t.Fatalf("WithProviderName(\"\") clobbered tag: got %q, want qwen", got)
	}
}

func TestChatProviderNameOverrides(t *testing.T) {
	c, _ := NewChat("gpt-test", "k", "")
	if got := c.Provider(); got != "openai" {
		t.Fatalf("chat default provider = %q, want openai", got)
	}
	c.WithProviderName("azure")
	if got := c.Provider(); got != "azure" {
		t.Fatalf("chat provider override = %q, want azure", got)
	}
}

// TestProviderNilReceiverSafe defends against a nil-LLM Provider() call
// landing in a panic — the OTel hot path treats Provider() as
// always-safe.
func TestProviderNilReceiverSafe(t *testing.T) {
	var c *LLM
	if got := c.Provider(); got != "openai" {
		t.Fatalf("nil receiver Provider() = %q, want openai", got)
	}
}

// TestClassifyAPIErrorMethodCarriesProvider verifies that the LLM-method
// variant of classifyAPIError stamps the per-instance provider name on
// the fallback path (the one that wraps non-*oai.Error errors via
// errdefs.ClassifyProviderError). The wrapped error string is the only
// observable surface that lets eval drivers see which sub-provider
// produced a misclassified network error, so we assert it directly.
func TestClassifyAPIErrorMethodCarriesProvider(t *testing.T) {
	c, _ := New("gpt-test", "k", "")
	c.WithProviderName("deepseek")
	got := openaishared.ClassifyAPIErrorWithProvider(c.Provider(), errors.New("network: connection reset by peer"))
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable fallback, got %v", got)
	}
	if !strings.Contains(got.Error(), "deepseek") {
		t.Fatalf("expected provider tag %q in wrapped error, got %v", "deepseek", got)
	}
}
