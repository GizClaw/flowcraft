package inferencetest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

type TranscriptionSessionSuite struct {
	Runtime *inference.Runtime
	Model   inference.ModelRef
	Config  func() inference.TranscriptionSessionConfig
	Chunk   func() media.AudioChunk

	SessionOpens  func() int64
	AudioSends    func() int64
	InputCloses   func() int64
	NextCalls     func() int64
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
		suite.NextCalls == nil ||
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
	assertTranscriptionConcurrency(t, suite, session)
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
	assertSessionClosesIdempotently(t, "transcription", session)
	closed = true
	assertTranscriptionCloseUnblocksNext(t, suite)
	if suite.SessionCloses() != closes+2 {
		t.Fatalf("provider closes = %d, want %d", suite.SessionCloses(), closes+2)
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
	NextCalls     func() int64
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
		suite.NextCalls == nil ||
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

	assertRealtimeConcurrency(t, suite, session)
	closes := suite.SessionCloses()
	assertSessionClosesIdempotently(t, "realtime", session)
	closed = true
	assertRealtimeCloseUnblocksNext(t, suite)
	if suite.SessionCloses() != closes+2 {
		t.Fatalf("provider closes = %d, want %d", suite.SessionCloses(), closes+2)
	}
}

func assertTranscriptionConcurrency(
	t *testing.T,
	suite TranscriptionSessionSuite,
	session inference.OpenedTranscriptionSession,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	nextCalls := suite.NextCalls()
	nextDone := make(chan error, 1)
	go func() {
		_, err := session.Next(ctx)
		nextDone <- err
	}()
	waitForCounter(t, "transcription Next", suite.NextCalls, nextCalls+1)
	assertStillWaiting(t, "transcription Next", nextDone)
	if err := session.SendAudio(context.Background(), suite.Chunk()); err != nil {
		cancel()
		t.Fatalf("SendAudio concurrent with Next: %v", err)
	}
	cancel()
	assertCanceledNext(t, "transcription", nextDone)
	if err := session.SendAudio(context.Background(), suite.Chunk()); err != nil {
		t.Fatalf("SendAudio after canceled Next: %v", err)
	}
}

func assertRealtimeConcurrency(
	t *testing.T,
	suite RealtimeSessionSuite,
	session inference.OpenedRealtimeSession,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	nextCalls := suite.NextCalls()
	nextDone := make(chan error, 1)
	go func() {
		_, err := session.Next(ctx)
		nextDone <- err
	}()
	waitForCounter(t, "realtime Next", suite.NextCalls, nextCalls+1)
	assertStillWaiting(t, "realtime Next", nextDone)
	if err := session.Send(context.Background(), suite.Input()); err != nil {
		cancel()
		t.Fatalf("Send concurrent with Next: %v", err)
	}
	cancel()
	assertCanceledNext(t, "realtime", nextDone)
	if err := session.Send(context.Background(), suite.Input()); err != nil {
		t.Fatalf("Send after canceled Next: %v", err)
	}
}

func assertStillWaiting(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned before cancellation or Close: %v", name, err)
	default:
	}
}

func assertCanceledNext(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !errdefs.IsAborted(err) {
			t.Fatalf("%s canceled Next error = %v, want aborted", name, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s Next did not stop after context cancellation", name)
	}
}

func assertSessionClosesIdempotently(
	t *testing.T,
	name string,
	session interface{ Close() error },
) {
	t.Helper()
	if err := session.Close(); err != nil {
		t.Fatalf("%s Close after canceled Next = %v", name, err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("%s second Close = %v", name, err)
	}
}

func assertTranscriptionCloseUnblocksNext(
	t *testing.T,
	suite TranscriptionSessionSuite,
) {
	t.Helper()
	session, err := suite.Runtime.OpenTranscription(
		context.Background(),
		suite.Model,
		suite.Config(),
	)
	if err != nil {
		t.Fatalf("OpenTranscription for Close/Next: %v", err)
	}
	nextCalls := suite.NextCalls()
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := session.Next(context.Background())
		nextDone <- nextErr
	}()
	waitForCounter(t, "transcription Next", suite.NextCalls, nextCalls+1)
	assertStillWaiting(t, "transcription Next", nextDone)
	if err := session.Close(); err != nil {
		t.Fatalf("transcription Close concurrent with Next: %v", err)
	}
	assertNextStoppedAfterClose(t, "transcription", nextDone)
}

func assertRealtimeCloseUnblocksNext(t *testing.T, suite RealtimeSessionSuite) {
	t.Helper()
	session, err := suite.Runtime.OpenRealtime(
		context.Background(),
		suite.Model,
		suite.Config(),
	)
	if err != nil {
		t.Fatalf("OpenRealtime for Close/Next: %v", err)
	}
	nextCalls := suite.NextCalls()
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := session.Next(context.Background())
		nextDone <- nextErr
	}()
	waitForCounter(t, "realtime Next", suite.NextCalls, nextCalls+1)
	assertStillWaiting(t, "realtime Next", nextDone)
	if err := session.Close(); err != nil {
		t.Fatalf("realtime Close concurrent with Next: %v", err)
	}
	assertNextStoppedAfterClose(t, "realtime", nextDone)
}

func assertNextStoppedAfterClose(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s Next succeeded after Close", name)
		}
		if !errors.Is(err, io.EOF) && !inference.IsKind(err, inference.ProviderFailure) {
			t.Fatalf("%s Next after Close error = %v", name, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s Next did not stop after Close", name)
	}
}

func waitForCounter(
	t *testing.T,
	name string,
	load func() int64,
	want int64,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s calls = %d, want at least %d", name, load(), want)
		}
		time.Sleep(time.Millisecond)
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
