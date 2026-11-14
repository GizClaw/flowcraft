package protocol

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// TestFirstTokenRecordsOnce is the guard itself, tested directly because the
// bug it prevents only reproduces where the clock cannot resolve the interval: a
// platform that returns exactly zero for `time.Since(start)` made `ttft != 0`
// read a real first delta as "not yet", emitted the event again for the next
// delta and dropped the time-to-first-token metric. The state is a flag now, so
// the assertion holds on every platform.
func TestFirstTokenRecordsOnce(t *testing.T) {
	recorder := telemetrytest.New(t)
	observation := generateObservation()
	_, call := startCall(context.Background(), observation, "openai",
		inference.GenerateRequest{Model: model.ModelRef{ID: model.ModelID{Name: "m"}}},
		ShapeUnary)
	if call == nil {
		t.Fatal("no call: the observation is disabled")
	}

	call.firstToken()
	// Zero the recorded delay: this is what a platform with a coarse clock
	// produces, and it must not be read as "no token yet".
	call.ttft = 0
	call.firstToken()
	call.finish(inference.GenerateResponse{}, nil)

	span := recorder.RequireNamed(t, observation.spanName)
	telemetrytest.RequireEventCount(t, span, "llm.first_token", 1)
}
