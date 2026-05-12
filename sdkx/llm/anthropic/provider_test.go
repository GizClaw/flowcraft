package anthropic

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// TestProviderDefaultsToAnthropic guards the historical contract: direct
// anthropic.New callers (no wrapper) keep seeing "anthropic" in OTel
// and metrics labels.
func TestProviderDefaultsToAnthropic(t *testing.T) {
	c, _ := New("claude-test", "k", "", nil)
	if got := c.Provider(); got != "anthropic" {
		t.Fatalf("default provider = %q, want %q", got, "anthropic")
	}
}

// TestWithProviderNameOverrides covers the minimax-style wrapping:
// sub-providers call WithProviderName so their traffic separates out
// from the base anthropic bucket. Empty names must not clobber the
// default.
func TestWithProviderNameOverrides(t *testing.T) {
	c, _ := New("claude-test", "k", "", nil)
	c.WithProviderName("minimax")
	if got := c.Provider(); got != "minimax" {
		t.Fatalf("after WithProviderName(minimax) = %q, want minimax", got)
	}

	c.WithProviderName("")
	if got := c.Provider(); got != "minimax" {
		t.Fatalf("WithProviderName(\"\") clobbered tag: got %q, want minimax", got)
	}
}

// TestProviderNilReceiverSafe defends Provider() against nil-LLM hot
// paths.
func TestProviderNilReceiverSafe(t *testing.T) {
	var c *LLM
	if got := c.Provider(); got != "anthropic" {
		t.Fatalf("nil receiver Provider() = %q, want anthropic", got)
	}
}

// TestClassifyAPIErrorMethodCarriesProvider asserts the LLM-method
// variant stamps the per-instance provider tag onto the fallback path
// (non-*anth.Error → errdefs.ClassifyProviderError). MiniMax-shaped
// network errors must surface under "minimax", not the upstream
// "anthropic" name.
func TestClassifyAPIErrorMethodCarriesProvider(t *testing.T) {
	c, _ := New("claude-test", "k", "", nil)
	c.WithProviderName("minimax")
	got := c.classifyAPIError(errors.New("network: connection reset by peer"))
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable fallback, got %v", got)
	}
	if !strings.Contains(got.Error(), "minimax") {
		t.Fatalf("expected provider tag %q in wrapped error, got %v", "minimax", got)
	}
}
