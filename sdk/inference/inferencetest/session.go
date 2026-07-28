package inferencetest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

type TranscriptionSessionSuite struct {
	Runtime *inference.Runtime
	Model   inference.ModelRef
	Config  func() inference.TranscriptionSessionConfig
	Chunk   func() media.AudioChunk

	SessionOpens  func() int64
	AudioSends    func() int64
	InputCloses   func() int64
	SessionCloses func() int64
	AssertSession func(*testing.T, inference.OpenedTranscriptionSession)
}

func RunTranscriptionSession(t *testing.T, suite TranscriptionSessionSuite) {
	t.Helper()
	if suite.Runtime == nil ||
		suite.Config == nil ||
		suite.Chunk == nil ||
		suite.SessionOpens == nil ||
		suite.AudioSends == nil ||
		suite.InputCloses == nil ||
		suite.SessionCloses == nil {
		t.Fatal("TranscriptionSessionSuite requires runtime, fixtures, and probes")
	}
	config := suite.Config()
	expectedConfig := config.Clone()
	opens := suite.SessionOpens()
	explanation, err := suite.Runtime.ExplainTranscriptionSession(
		context.Background(),
		suite.Model,
		config,
	)
	if err != nil {
		t.Fatalf("ExplainTranscriptionSession: %v", err)
	}
	if suite.SessionOpens() != opens ||
		explanation.Operation != inference.OperationTranscription ||
		len(explanation.Decisions) == 0 {
		t.Fatalf("Explain opened a session or lost decisions: %+v", explanation)
	}
	assertUnchanged(t, expectedConfig, config.Clone())

	session, err := suite.Runtime.OpenTranscription(
		context.Background(),
		suite.Model,
		config,
	)
	if err != nil {
		t.Fatalf("OpenTranscription: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = session.Close()
		}
	})
	if suite.SessionOpens() != opens+1 {
		t.Fatalf("session opens = %d, want %d", suite.SessionOpens(), opens+1)
	}
	assertSessionMetadata(t, session.Metadata(), suite.Model, inference.OperationTranscription)

	chunk := suite.Chunk()
	expectedChunk := chunk.Clone()
	sends := suite.AudioSends()
	if err := session.SendAudio(context.Background(), chunk); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if suite.AudioSends() != sends+1 {
		t.Fatalf("audio sends = %d, want %d", suite.AudioSends(), sends+1)
	}
	assertUnchanged(t, expectedChunk, chunk.Clone())
	inputCloses := suite.InputCloses()
	if err := session.CloseInput(context.Background()); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}
	if suite.InputCloses() != inputCloses+1 {
		t.Fatalf("input closes = %d, want %d", suite.InputCloses(), inputCloses+1)
	}
	if suite.AssertSession != nil {
		suite.AssertSession(t, session)
	}

	closes := suite.SessionCloses()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if suite.SessionCloses() != closes+1 {
		t.Fatalf("provider closes = %d, want %d", suite.SessionCloses(), closes+1)
	}
}

type RealtimeSessionSuite struct {
	Runtime *inference.Runtime
	Model   inference.ModelRef
	Config  func() inference.RealtimeConfig
	Input   func() inference.RealtimeInput

	SessionOpens  func() int64
	InputCompiles func() int64
	InputSends    func() int64
	Cancellations func() int64
	SessionCloses func() int64
	AssertSession func(*testing.T, inference.OpenedRealtimeSession)
}

func RunRealtimeSession(t *testing.T, suite RealtimeSessionSuite) {
	t.Helper()
	if suite.Runtime == nil ||
		suite.Config == nil ||
		suite.Input == nil ||
		suite.SessionOpens == nil ||
		suite.InputCompiles == nil ||
		suite.InputSends == nil ||
		suite.Cancellations == nil ||
		suite.SessionCloses == nil {
		t.Fatal("RealtimeSessionSuite requires runtime, fixtures, and probes")
	}
	config := suite.Config()
	expectedConfig := config.Clone()
	opens := suite.SessionOpens()
	explanation, err := suite.Runtime.ExplainRealtime(
		context.Background(),
		suite.Model,
		config,
	)
	if err != nil {
		t.Fatalf("ExplainRealtime: %v", err)
	}
	if suite.SessionOpens() != opens ||
		explanation.Operation != inference.OperationRealtime ||
		len(explanation.Decisions) == 0 {
		t.Fatalf("Explain opened a session or lost decisions: %+v", explanation)
	}
	assertUnchanged(t, expectedConfig, config.Clone())

	session, err := suite.Runtime.OpenRealtime(
		context.Background(),
		suite.Model,
		config,
	)
	if err != nil {
		t.Fatalf("OpenRealtime: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = session.Close()
		}
	})
	if suite.SessionOpens() != opens+1 {
		t.Fatalf("session opens = %d, want %d", suite.SessionOpens(), opens+1)
	}
	assertSessionMetadata(t, session.Metadata(), suite.Model, inference.OperationRealtime)

	input := suite.Input()
	expectedInput := input.Clone()
	compiles := suite.InputCompiles()
	sends := suite.InputSends()
	if err := session.Send(context.Background(), input); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if suite.InputCompiles() != compiles+1 {
		t.Fatalf("input compiles = %d, want %d", suite.InputCompiles(), compiles+1)
	}
	if suite.InputSends() != sends+1 {
		t.Fatalf("input sends = %d, want %d", suite.InputSends(), sends+1)
	}
	assertUnchanged(t, expectedInput, input.Clone())
	reports := session.InputReports()
	if len(reports) == 0 {
		t.Fatal("successful realtime input did not retain its compile report")
	}
	report := reports[len(reports)-1]
	if report.Operation != inference.OperationRealtime ||
		len(report.Decisions) == 0 {
		t.Fatalf("input report = %+v", report)
	}
	cancellations := suite.Cancellations()
	if err := session.CancelResponse(context.Background()); err != nil {
		t.Fatalf("CancelResponse: %v", err)
	}
	if suite.Cancellations() != cancellations+1 {
		t.Fatalf(
			"response cancellations = %d, want %d",
			suite.Cancellations(),
			cancellations+1,
		)
	}
	if suite.AssertSession != nil {
		suite.AssertSession(t, session)
	}

	closes := suite.SessionCloses()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if suite.SessionCloses() != closes+1 {
		t.Fatalf("provider closes = %d, want %d", suite.SessionCloses(), closes+1)
	}
}

func assertSessionMetadata(
	t *testing.T,
	metadata inference.Metadata,
	model inference.ModelRef,
	operation inference.Operation,
) {
	t.Helper()
	if metadata.Model != model.ID ||
		metadata.Operation != operation ||
		len(metadata.Decisions) == 0 {
		t.Fatalf("session metadata = %+v", metadata)
	}
}
