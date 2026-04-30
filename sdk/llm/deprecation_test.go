package llm

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestModelDeprecation_IsZero(t *testing.T) {
	cases := []struct {
		name string
		d    ModelDeprecation
		want bool
	}{
		{"empty", ModelDeprecation{}, true},
		{"only retires", ModelDeprecation{RetiresAt: time.Now()}, false},
		{"only replacement", ModelDeprecation{Replacement: "openai/gpt-5"}, false},
		{"only notes", ModelDeprecation{Notes: "see X"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.IsZero(); got != tc.want {
				t.Fatalf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProviderRegistry_LookupModel(t *testing.T) {
	reg := NewProviderRegistry()
	reg.RegisterModels("p", []ModelInfo{
		{Name: "alive"},
		{
			Name: "doomed",
			Deprecation: ModelDeprecation{
				RetiresAt:   time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
				Replacement: "p/alive",
			},
		},
	})

	if info, ok := reg.LookupModel("p", "alive"); !ok || !info.Deprecation.IsZero() {
		t.Fatalf("alive: ok=%v deprecated=%v", ok, !info.Deprecation.IsZero())
	}
	if info, ok := reg.LookupModel("p", "doomed"); !ok || info.Deprecation.IsZero() {
		t.Fatalf("doomed: ok=%v deprecation should be set", ok)
	}
	if _, ok := reg.LookupModel("p", "ghost"); ok {
		t.Fatal("ghost: expected not found")
	}
}

// deprecationStore is a minimal ProviderConfigStore used to drive the
// resolver against an in-test registry without touching DefaultRegistry.
type deprecationStore struct {
	cfg *ProviderConfig
}

func (s *deprecationStore) GetProviderConfig(_ context.Context, _, _ string) (*ProviderConfig, error) {
	if s.cfg == nil {
		return nil, errdefs.NotFoundf("no config")
	}
	return s.cfg, nil
}

func TestResolver_DeprecatedModel_WarnsOncePerProcess(t *testing.T) {
	// Reset the package-level dedup state so the test can observe
	// the first-time-warn branch deterministically. This is the
	// reason resetDeprecatedModelWarnedForTest exists.
	resetDeprecatedModelWarnedForTest()

	reg := NewProviderRegistry()
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) { return &mockLLM{}, nil })
	reg.RegisterModels("p", []ModelInfo{
		{
			Name: "doomed",
			Deprecation: ModelDeprecation{
				RetiresAt:   time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
				Replacement: "p/alive",
				Notes:       "test deprecation",
			},
		},
		{Name: "alive"},
	})

	r := &defaultResolver{
		registry: reg,
		store:    &deprecationStore{cfg: &ProviderConfig{Provider: "p"}},
		cache:    make(map[cacheKey]LLM),
	}

	// First Resolve fires the warning path. We don't intercept the
	// telemetry exporter (it's a no-op in tests) — instead we assert
	// the dedup flag is set, which is the public contract of the
	// warn-once mechanism.
	if _, err := r.Resolve(context.Background(), "p/doomed"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, ok := deprecatedModelWarned.Load("p/doomed"); !ok {
		t.Fatal("expected deprecation flag set after first resolve")
	}

	// Second Resolve must not re-set anything; we can only verify
	// the flag remains. (The dedup test on the function itself is
	// in TestWarnDeprecatedModel_OnlyOnce below.)
	if _, err := r.Resolve(context.Background(), "p/doomed"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	// Resolving an alive model must not pollute the dedup map.
	if _, err := r.Resolve(context.Background(), "p/alive"); err != nil {
		t.Fatalf("alive resolve: %v", err)
	}
	if _, ok := deprecatedModelWarned.Load("p/alive"); ok {
		t.Fatal("alive model should not appear in deprecation dedup map")
	}
}

func TestWarnDeprecatedModel_OnlyOnce(t *testing.T) {
	resetDeprecatedModelWarnedForTest()
	d := ModelDeprecation{Replacement: "x/y"}

	// First call should populate the dedup entry.
	warnDeprecatedModel(context.Background(), "x", "z", d)
	if _, ok := deprecatedModelWarned.Load("x/z"); !ok {
		t.Fatal("expected dedup entry after first call")
	}
	// Second call must be a no-op as far as observable state goes;
	// we re-issue and assert the entry is still present (proxy for
	// "the early return short-circuited").
	warnDeprecatedModel(context.Background(), "x", "z", d)
	if _, ok := deprecatedModelWarned.Load("x/z"); !ok {
		t.Fatal("dedup entry vanished after second call (should be stable)")
	}
}
