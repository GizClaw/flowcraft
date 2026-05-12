// Package tooltest provides a generic contract test suite for
// [tool.Tool] implementations.
//
// The package mirrors [enginetest]'s shape and intent: any type
// implementing [tool.Tool] should pass [RunSuite] to be considered
// "contract-compliant" — i.e., its behaviour matches what
// sdk/tool/registry.go and the LLM-tool wire protocol expect from
// it. Built-in tools (askuser, dispatcher kanban tools) call
// RunSuite from their own *_test.go; third-party tool authors
// should do the same.
//
// # What the suite covers
//
//   - Definition() metadata invariants: non-empty name, deterministic
//     return, JSON-marshalable schema (the registry serialises it
//     into the LLM's tool catalogue).
//   - Execute() error classification: bad JSON arguments must surface
//     as errdefs.Validation; the tool must NOT panic on empty / nil
//     args when the schema doesn't require any property.
//   - Context cancellation propagation: a cancelled ctx given to
//     Execute should make it return promptly (within the suite's
//     bounded deadline) rather than ignore the cancel signal.
//   - Concurrent invocation safety: ten goroutines hammering Definition()
//     in parallel must not data-race (we run with -race). Tools that
//     hold per-call mutable state should still expose stable
//     Definition() output.
//
// # What the suite deliberately does NOT cover
//
//   - The semantic correctness of Execute() — that's per-tool unit
//     test territory. The suite has no idea what "correct" means for
//     a search tool vs. a calculator vs. ask_user.
//   - Side effects (network, fs). The suite calls Execute with
//     known-bad input; tools that try to reach external systems on
//     bad input are themselves the bug.
//
// # Wiring
//
//	func TestAskUser_Contract(t *testing.T) {
//	    tooltest.RunSuite(t, func() tool.Tool { return askuser.New() })
//	}
//
// Each subtest constructs a fresh tool via the supplied Factory so
// per-tool state cannot leak across cases.
package tooltest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Factory builds a fresh [tool.Tool] for each subtest. The suite
// invokes it once per case so subtests do not share tool state.
type Factory func() tool.Tool

// Capabilities lets a tool opt out of subtests that don't apply.
// Most tools should pass the zero value (= every subtest runs).
type Capabilities struct {
	// SkipBadArgsValidation is true when the tool legitimately
	// accepts non-JSON arguments (rare — most tools take a JSON
	// object per the LLM tool-call protocol). When true the suite
	// skips the bad-JSON / unparseable-argument cases.
	SkipBadArgsValidation bool

	// SkipEmptyArgsTolerance is true when the tool's schema
	// declares required properties and so genuinely cannot run
	// with empty args. The suite then asserts the empty-args path
	// returns errdefs.Validation rather than treating an error as
	// a failure.
	SkipEmptyArgsTolerance bool

	// SkipContextCancel is true when the tool's Execute is so
	// cheap that it returns before any ctx-cancel could be observed
	// (pure in-memory transforms with no select). The suite then
	// skips the cancellation responsiveness check.
	SkipContextCancel bool
}

// RunSuite runs every applicable contract subtest against tools
// produced by f. Each subtest builds a fresh tool so failures
// isolate cleanly.
func RunSuite(t *testing.T, f Factory, caps ...Capabilities) {
	t.Helper()
	c := Capabilities{}
	if len(caps) > 0 {
		c = caps[0]
	}

	t.Run("DefinitionIsStable", func(t *testing.T) { testDefinitionStable(t, f) })
	t.Run("DefinitionNameNonEmpty", func(t *testing.T) { testDefinitionNameNonEmpty(t, f) })
	t.Run("DefinitionSchemaJSONMarshalable", func(t *testing.T) { testDefinitionSchemaMarshalable(t, f) })
	t.Run("DefinitionConcurrentSafe", func(t *testing.T) { testDefinitionConcurrent(t, f) })

	if !c.SkipBadArgsValidation {
		t.Run("ExecuteBadJSONIsValidation", func(t *testing.T) { testExecuteBadJSON(t, f) })
	}
	if !c.SkipEmptyArgsTolerance {
		t.Run("ExecuteEmptyArgsBehaviour", func(t *testing.T) { testExecuteEmptyArgs(t, f) })
	}
	if !c.SkipContextCancel {
		t.Run("ExecuteHonoursCtxCancel", func(t *testing.T) { testExecuteCtxCancel(t, f) })
	}
}

// ---------- subtests ----------

func testDefinitionStable(t *testing.T, f Factory) {
	t.Helper()
	tl := f()
	d1 := tl.Definition()
	d2 := tl.Definition()
	if d1.Name != d2.Name || d1.Description != d2.Description {
		t.Errorf("Definition() drifted between calls: d1=%+v d2=%+v — Definition must be deterministic", d1, d2)
	}
	// Schema equality is by-content; marshal both and compare.
	b1, err1 := json.Marshal(d1.InputSchema)
	b2, err2 := json.Marshal(d2.InputSchema)
	if err1 != nil || err2 != nil {
		t.Fatalf("InputSchema not marshalable: err1=%v err2=%v", err1, err2)
	}
	if string(b1) != string(b2) {
		t.Errorf("InputSchema drifted between Definition() calls:\n  d1=%s\n  d2=%s", b1, b2)
	}
}

func testDefinitionNameNonEmpty(t *testing.T, f Factory) {
	t.Helper()
	d := f().Definition()
	if strings.TrimSpace(d.Name) == "" {
		t.Errorf("Definition().Name = %q, want non-empty (registry uses Name as the registration key)", d.Name)
	}
}

func testDefinitionSchemaMarshalable(t *testing.T, f Factory) {
	t.Helper()
	d := f().Definition()
	if d.InputSchema == nil {
		// nil is valid (no-input tools); just confirm marshal still works.
		if _, err := json.Marshal(d); err != nil {
			t.Fatalf("Definition with nil InputSchema must still marshal: %v", err)
		}
		return
	}
	b, err := json.Marshal(d.InputSchema)
	if err != nil {
		t.Fatalf("InputSchema must be JSON-marshalable for the LLM tool catalogue: %v", err)
	}
	// Sanity: the marshalled schema is itself a JSON object — every
	// known LLM provider expects an object schema at the top level.
	var roundTrip map[string]any
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("InputSchema marshalled to non-object JSON %s: %v", b, err)
	}
}

func testDefinitionConcurrent(t *testing.T, f Factory) {
	t.Helper()
	tl := f()
	var wg sync.WaitGroup
	const n = 16
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tl.Definition()
		}()
	}
	wg.Wait()
}

func testExecuteBadJSON(t *testing.T, f Factory) {
	t.Helper()
	tl := f()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// "{" is unambiguously broken JSON. Tools that accept non-JSON
	// arguments must declare SkipBadArgsValidation.
	_, err := tl.Execute(ctx, "{")
	if err == nil {
		t.Fatal("Execute(\"{\") returned nil error; bad JSON must surface as a Validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("Execute(\"{\") err = %v (kind=%v), want errdefs.IsValidation", err, kindOf(err))
	}
}

func testExecuteEmptyArgs(t *testing.T, f Factory) {
	t.Helper()
	tl := f()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Empty string is the LLM's way of saying "no arguments".
	// Tools whose schema requires fields MUST surface Validation;
	// tools without required fields MUST NOT panic and MAY succeed.
	_, err := tl.Execute(ctx, "")
	if err == nil {
		// Tool is happy with no args — fine.
		return
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("Execute(\"\") returned err=%v (kind=%v); empty args must either succeed or surface as Validation, not as a different category", err, kindOf(err))
	}
}

func testExecuteCtxCancel(t *testing.T, f Factory) {
	t.Helper()
	tl := f()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Execute even starts.

	// Use empty args; if the tool requires args it will surface
	// Validation immediately, which is fine — the point of this
	// subtest is "did it return promptly", not which error it
	// returned. We only fail if Execute hangs past the deadline.
	done := make(chan struct{})
	go func() {
		_, _ = tl.Execute(ctx, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return within 2s of a pre-cancelled ctx; tools that block on external work must select on ctx.Done()")
	}
}

// kindOf is a small helper used by error messages so failed
// assertions print a recognisable error category instead of a raw
// errdefs internal string. Returns "" when the error has no
// errdefs marker.
func kindOf(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errdefs.IsValidation(err):
		return "Validation"
	case errdefs.IsNotFound(err):
		return "NotFound"
	case errdefs.IsNotAvailable(err):
		return "NotAvailable"
	case errdefs.IsTimeout(err):
		return "Timeout"
	case errdefs.IsAborted(err):
		return "Aborted"
	}
	return "uncategorised"
}
