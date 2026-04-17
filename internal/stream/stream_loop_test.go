package stream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

type mockSink struct {
	events []MappedEvent
	done   *workflow.Result
	err    *[2]string
}

func (m *mockSink) Send(ev MappedEvent) error {
	m.events = append(m.events, ev)
	return nil
}

func (m *mockSink) Done(result *workflow.Result) error {
	m.done = result
	return nil
}

func (m *mockSink) Error(code, message string) error {
	m.err = &[2]string{code, message}
	return nil
}

func makeEvent(t event.EventType) event.Event {
	return event.Event{
		ID: "test-id", Type: t, RunID: "run-1", GraphID: "graph-1",
		NodeID: "node-1", Timestamp: time.Now(), Payload: map[string]any{"key": "value"},
	}
}

type chanSub struct{ ch chan event.Event }

func (s *chanSub) Events() <-chan event.Event { return s.ch }
func (s *chanSub) Close() error               { return nil }

func resultWithText(text string) *workflow.Result {
	return &workflow.Result{
		Status:   workflow.StatusCompleted,
		Messages: []sdkmodel.Message{sdkmodel.NewTextMessage(sdkmodel.RoleAssistant, text)},
	}
}

func emptyResult() *workflow.Result {
	return &workflow.Result{Status: workflow.StatusCompleted}
}

func TestStreamLoop_Done(t *testing.T) {
	sink := &mockSink{}
	done := make(chan RunResult, 1)
	done <- RunResult{Value: resultWithText("hello")}
	StreamLoop(context.Background(), nil, done, sink)
	if sink.done == nil || sink.done.Text() != "hello" {
		t.Fatalf("expected Done with answer=hello")
	}
}

func TestStreamLoop_Error(t *testing.T) {
	sink := &mockSink{}
	done := make(chan RunResult, 1)
	done <- RunResult{Err: errdefs.NotFoundf("thing %q not found", "id-1")}
	StreamLoop(context.Background(), nil, done, sink)
	if sink.err == nil || sink.err[0] != "not_found" {
		t.Fatalf("expected Error with code not_found, got %v", sink.err)
	}
}

func TestStreamLoop_Events(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 3)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	evCh <- makeEvent(event.EventNodeStart)
	evCh <- makeEvent(event.EventNodeComplete)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- RunResult{Value: resultWithText("ok")}
	}()
	StreamLoop(context.Background(), sub, done, sink)
	if len(sink.events) != 3 {
		t.Fatalf("got %d events, want 3", len(sink.events))
	}
	if sink.done == nil {
		t.Fatal("expected Done to be called")
	}
}

func TestStreamLoop_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sink := &mockSink{}
	returned := make(chan struct{})
	go func() { StreamLoop(ctx, nil, nil, sink); close(returned) }()
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("StreamLoop did not return after context cancel")
	}
}

func TestStreamLoop_NilSub(t *testing.T) {
	sink := &mockSink{}
	done := make(chan RunResult, 1)
	done <- RunResult{Value: resultWithText("no-sub")}
	StreamLoop(context.Background(), nil, done, sink)
	if sink.done == nil {
		t.Fatal("expected Done to be called with nil sub")
	}
}

func TestStreamLoop_NilDone(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 1)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	close(evCh)
	StreamLoop(context.Background(), sub, nil, sink)
	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
}

func TestStreamLoop_Enricher(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 2)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	evCh <- makeEvent(event.EventNodeStart)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- RunResult{Value: emptyResult()}
	}()
	dropper := func(_ event.Event, m *MappedEvent) bool { return m.Type != "node_start" }
	StreamLoop(context.Background(), sub, done, sink, dropper)
	if len(sink.events) != 1 || sink.events[0].Type != "graph_start" {
		t.Fatalf("expected only graph_start, got %v", sink.events)
	}
}

func TestStreamLoop_EnricherModify(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 1)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- RunResult{Value: emptyResult()}
	}()
	modifier := func(_ event.Event, m *MappedEvent) bool { m.Type = "custom_" + m.Type; return true }
	StreamLoop(context.Background(), sub, done, sink, modifier)
	if len(sink.events) != 1 || sink.events[0].Type != "custom_graph_start" {
		t.Fatalf("expected custom_graph_start, got %v", sink.events)
	}
}

func TestWrapActorDone_Callback(t *testing.T) {
	ch := make(chan RunResult, 1)
	ch <- RunResult{Value: resultWithText("result")}
	inputs := map[string]any{model.InputKeyCallback: "card-42"}
	result := <-WrapActorDone(ch, inputs)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Value.State["callback"] != true || result.Value.State["card_id"] != "card-42" {
		t.Fatalf("unexpected state: %v", result.Value.State)
	}
}

func TestWrapActorDone_NoCallback(t *testing.T) {
	ch := make(chan RunResult, 1)
	ch <- RunResult{Value: resultWithText("result")}
	inputs := map[string]any{"other": "value"}
	result := <-WrapActorDone(ch, inputs)
	if result.Value.State["callback"] != nil {
		t.Fatalf("expected no callback in state, got %v", result.Value.State)
	}
}

func TestStreamLoop_DrainAfterDone(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 3)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	evCh <- makeEvent(event.EventNodeStart)
	evCh <- makeEvent(event.EventNodeComplete)
	done := make(chan RunResult, 1)
	done <- RunResult{Value: resultWithText("drained")}
	StreamLoop(context.Background(), sub, done, sink)
	if len(sink.events) != 3 {
		t.Fatalf("got %d events, want 3", len(sink.events))
	}
	if sink.done == nil || sink.done.Text() != "drained" {
		t.Fatalf("expected done with answer=drained")
	}
}

type errSink struct {
	count    int
	errAfter int
	events   []MappedEvent
	done     *workflow.Result
}

func (s *errSink) Send(ev MappedEvent) error {
	s.count++
	if s.count >= s.errAfter {
		return fmt.Errorf("send failed")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *errSink) Done(result *workflow.Result) error {
	s.done = result
	return nil
}

func (s *errSink) Error(code, message string) error {
	return nil
}

func TestStreamLoop_SendError(t *testing.T) {
	sink := &errSink{errAfter: 2}
	evCh := make(chan event.Event, 3)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	evCh <- makeEvent(event.EventNodeStart)
	evCh <- makeEvent(event.EventNodeComplete)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		done <- RunResult{Value: emptyResult()}
	}()
	StreamLoop(context.Background(), sub, done, sink)
	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	if sink.done != nil {
		t.Fatalf("expected Done not to be called")
	}
}

func TestStreamLoop_MultipleEnrichers(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 1)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- RunResult{Value: emptyResult()}
	}()
	enricher1 := func(_ event.Event, m *MappedEvent) bool { m.Type = "a_" + m.Type; return true }
	enricher2 := func(_ event.Event, m *MappedEvent) bool { m.Type = "b_" + m.Type; return true }
	StreamLoop(context.Background(), sub, done, sink, enricher1, enricher2)
	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	if sink.events[0].Type != "b_a_graph_start" {
		t.Fatalf("expected b_a_graph_start, got %s", sink.events[0].Type)
	}
}

func TestStreamLoop_MultipleEnrichers_FirstReturnsFalse(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event, 1)
	sub := &chanSub{ch: evCh}
	evCh <- makeEvent(event.EventGraphStart)
	done := make(chan RunResult, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- RunResult{Value: emptyResult()}
	}()
	enricher1 := func(_ event.Event, m *MappedEvent) bool { return false }
	enricher2 := func(_ event.Event, m *MappedEvent) bool { m.Type = "b_" + m.Type; return true }
	StreamLoop(context.Background(), sub, done, sink, enricher1, enricher2)
	if len(sink.events) != 0 {
		t.Fatalf("got %d events, want 0 (event dropped)", len(sink.events))
	}
}

func TestStreamLoop_DoneChannelClosed(t *testing.T) {
	sink := &mockSink{}
	done := make(chan RunResult)
	close(done)
	returned := make(chan struct{})
	go func() {
		StreamLoop(context.Background(), nil, done, sink)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("StreamLoop did not return after done channel closed")
	}
}

func TestStreamLoop_SubClosedBeforeDone(t *testing.T) {
	sink := &mockSink{}
	evCh := make(chan event.Event)
	close(evCh)
	sub := &chanSub{ch: evCh}
	done := make(chan RunResult)
	returned := make(chan struct{})
	go func() {
		StreamLoop(context.Background(), sub, done, sink)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("StreamLoop did not return after sub channel closed")
	}
}

func TestWrapActorDone_ErrorPassthrough(t *testing.T) {
	origErr := errdefs.NotFoundf("thing %q not found", "id-1")
	ch := make(chan RunResult, 1)
	ch <- RunResult{Err: origErr}
	inputs := map[string]any{model.InputKeyCallback: "card-42"}
	result := <-WrapActorDone(ch, inputs)
	if result.Err == nil {
		t.Fatalf("expected error, got nil")
	}
	if result.Value != nil {
		t.Fatalf("expected nil Value, got %v", result.Value)
	}
	if result.Err != origErr {
		t.Fatalf("expected error to be passed through unchanged, got %v", result.Err)
	}
}

func TestWrapActorDone_NilValue(t *testing.T) {
	ch := make(chan RunResult, 1)
	ch <- RunResult{Value: nil}
	inputs := map[string]any{model.InputKeyCallback: "card-42"}
	result := <-WrapActorDone(ch, inputs)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Value != nil {
		t.Fatalf("expected nil Value, got %v", result.Value)
	}
}

func TestWrapActorDone_EmptyInputs(t *testing.T) {
	ch := make(chan RunResult, 1)
	ch <- RunResult{Value: resultWithText("ok")}
	result := <-WrapActorDone(ch, nil)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
}
