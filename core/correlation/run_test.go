package correlation

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestRunRoundTrip(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "acme"})
	run, ok := From(ctx)
	if !ok {
		t.Fatal("RunFrom reported nothing")
	}
	if run.RuntimeID != "rt-1" || run.TenantID != "acme" {
		t.Errorf("run = %+v", run)
	}
}

// The two facts are discovered at different depths: a layer that knows the
// tenant must not erase the runtime id it inherited.
func TestRunMergesNestedCalls(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	ctx = WithRun(ctx, Run{TenantID: "acme"})
	run, _ := From(ctx)
	if run.RuntimeID != "rt-1" || run.TenantID != "acme" {
		t.Errorf("run = %+v, want both fields", run)
	}
}

func TestAttrsOmitsEmptyFields(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	attrs := Attrs(ctx)
	if len(attrs) != 1 {
		t.Fatalf("attrs = %v, want only the runtime id", attrs)
	}
	if string(attrs[0].Key) != AttrRuntimeID || attrs[0].Value.AsString() != "rt-1" {
		t.Errorf("attr = %v, want %s=rt-1", attrs[0], AttrRuntimeID)
	}

	both := Attrs(WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "acme"}))
	if len(both) != 2 {
		t.Fatalf("attrs = %v, want two", both)
	}
	if string(both[1].Key) != AttrTenantID {
		t.Errorf("second attr = %s, want %s", both[1].Key, AttrTenantID)
	}
}

// A hand-built context stays free: no attachment means no allocation and no
// attributes to filter.
func TestAttrsAbsent(t *testing.T) {
	if attrs := Attrs(context.Background()); attrs != nil {
		t.Errorf("attrs = %v, want nil", attrs)
	}
	var absent context.Context
	if attrs := Attrs(absent); attrs != nil {
		t.Errorf("attrs on nil ctx = %v, want nil", attrs)
	}
	if _, ok := From(absent); ok {
		t.Error("From(nil) reported a run")
	}
}

func TestWithRunOnNilContext(t *testing.T) {
	//nolint:staticcheck // WithRun documents that it tolerates a nil context
	ctx := WithRun(nil, Run{RuntimeID: "rt-1"})
	if run, ok := From(ctx); !ok || run.RuntimeID != "rt-1" {
		t.Errorf("run = %+v, ok = %v", run, ok)
	}
}

// A runtime attaches its own id; a higher layer adds the tenant on the same
// context and neither field is lost.
func TestRunAddsTenantToRuntimeContext(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	ctx = WithRun(ctx, Run{TenantID: "acme"})
	attrs := Attrs(ctx)
	if len(attrs) != 2 {
		t.Fatalf("attrs = %v, want runtime id and tenant", attrs)
	}
}

// The attributes are built when the identity is attached: a span decorating the
// same context must not rebuild them.
func TestAttrsAllocateNothingOnceAttached(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "tenant-1"})
	if got := testing.AllocsPerRun(1000, func() { _ = Attrs(ctx) }); got != 0 {
		t.Fatalf("Attrs allocates %.0f times, want 0", got)
	}
}

// The returned slice is shared, so a caller's append must copy rather than write
// into the cache.
func TestAttrsSurviveCallerAppend(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	attrs := Attrs(ctx)
	if got := len(append(attrs, attribute.String("extra", "x"))); got != 2 {
		t.Fatalf("append = %d attributes, want 2", got)
	}
	if got := len(Attrs(ctx)); got != 1 {
		t.Fatalf("Attrs after a caller append = %d attributes, want 1", got)
	}
}

func BenchmarkAttrs(b *testing.B) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "tenant-1"})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Attrs(ctx)
	}
}

// The session identity deploy attaches survives nesting: a graph adds the turn
// and manifest without erasing the session it inherited.
func TestAttrsCarrySessionIdentity(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	ctx = WithRun(ctx, Run{SessionID: "s1", AgentName: "writer"})
	ctx = WithRun(ctx, Run{Turn: 3, Manifest: "sha256:m"})
	run, _ := From(ctx)
	if run.RuntimeID != "rt-1" || run.SessionID != "s1" || run.AgentName != "writer" ||
		run.Turn != 3 || run.Manifest != "sha256:m" {
		t.Fatalf("run = %+v, want every field", run)
	}
	attrs := make(map[string]any, 6)
	for _, kv := range Attrs(ctx) {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if attrs[AttrSessionID] != "s1" || attrs[AttrAgentName] != "writer" {
		t.Errorf("identity attrs = %v", attrs)
	}
	if attrs[AttrTurn] != int64(3) || attrs[AttrManifestID] != "sha256:m" {
		t.Errorf("turn/manifest attrs = %v", attrs)
	}
}

// TaskID is identity: it is the checkpoint key and must survive a merge.
// Turn is correlation: it is the in-process Run ordinal, not identity.
func TestRunCarriesTaskAndTurn(t *testing.T) {
	ctx := WithRun(context.Background(), Run{
		RuntimeID: "rt-1",
		SessionID: "s-1",
		AgentName: "writer",
		TaskID:    "t-1",
		Turn:      2,
	})
	run, ok := From(ctx)
	if !ok || run.TaskID != "t-1" || run.Turn != 2 {
		t.Fatalf("run = %+v, ok = %v", run, ok)
	}

	// A later layer that only knows the turn must not erase the task id.
	ctx = WithRun(ctx, Run{Turn: 3})
	run, _ = From(ctx)
	if run.TaskID != "t-1" || run.Turn != 3 {
		t.Fatalf("merged run = %+v", run)
	}

	attrs := map[string]string{}
	for _, attr := range Attrs(ctx) {
		attrs[string(attr.Key)] = attr.Value.String()
	}
	if attrs[AttrTaskID] != "t-1" {
		t.Errorf("task attr = %v", attrs)
	}
	if attrs[AttrTurn] != "3" {
		t.Errorf("turn attr = %v", attrs)
	}
}

// The delegation lineage travels in the same value: a layer that stamps only the
// depth keeps the chain it inherited, and a run that was never delegated carries
// neither. Lineage is not rendered as an attribute — it is not the identity a
// span is correlated by, and the delegation layer owns those records.
func TestRunCarriesDelegationLineage(t *testing.T) {
	ctx := WithRun(context.Background(), Run{
		SessionID:       "s1",
		ChainID:         "chain-1",
		DelegationDepth: 1,
	})
	ctx = WithRun(ctx, Run{DelegationDepth: 2})

	run, ok := From(ctx)
	if !ok || run.SessionID != "s1" || run.ChainID != "chain-1" || run.DelegationDepth != 2 {
		t.Fatalf("run = %+v, ok = %v", run, ok)
	}
	if attrs := Attrs(ctx); len(attrs) != 1 {
		t.Fatalf("attrs = %v, want only session.id: lineage is not identity", attrs)
	}

	plain, _ := From(WithRun(context.Background(), Run{SessionID: "s2"}))
	if plain.DelegationDepth != 0 || plain.ChainID != "" {
		t.Fatalf("undelegated run carries lineage: %+v", plain)
	}
}
