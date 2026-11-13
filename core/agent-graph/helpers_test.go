// Fixtures shared by the graph tests: node kinds, blobs and run helpers.
package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// constKind writes one text value to one state key.
type constKind struct{}

func (constKind) Name() string { return "const" }

func (constKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{Key: config.Key, Value: Text(config.Value)}}}), nil
}

// emitKind writes one media value to one state key.
type emitKind struct{}

func (emitKind) Name() string { return "emit" }

func (emitKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key       string `json:"key"`
		MediaType string `json:"media_type"`
		Value     string `json:"value"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{
		Key:   config.Key,
		Value: Image(config.MediaType, []byte(config.Value)),
	}}}), nil
}

// countKind records how often each node ran.
type countKind struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countKind) Name() string { return "count" }

func (c *countKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[in.Node]++
	return Static(Result{}), nil
}

func (c *countKind) count(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[node]
}

// memBlobs is a blob store that keeps what it is given.
type memBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
	seq  int
}

func (m *memBlobs) PutBlob(_ context.Context, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	m.seq++
	ref := fmt.Sprintf("blob-%d", m.seq)
	m.data[ref] = append([]byte(nil), data...)
	return ref, nil
}

func registry(t *testing.T, extra ...NodeKind) *Registry {
	t.Helper()
	kinds := append([]NodeKind{constKind{}, emitKind{}}, extra...)
	r, err := NewRegistry(kinds...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func build(t *testing.T, body string, extra ...NodeKind) *Graph {
	t.Helper()
	g, err := buildErr(t, body, extra...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func buildErr(t *testing.T, body string, extra ...NodeKind) (*Graph, error) {
	t.Helper()
	doc, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return Build(doc, registry(t, extra...))
}

func drain(t *testing.T, g *Graph, messages ...message.Message) ([]agent.Event, error) {
	t.Helper()
	run, err := g.Start(context.Background(), messages)
	if err != nil {
		return nil, err
	}
	var events []agent.Event
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

// answer resumes a run with one reply, the way the runner does.
func answer(ctx context.Context, run *Run, text string) iter.Seq2[agent.Event, error] {
	return run.Answer(ctx, message.NewTextContent(text))
}

func textOf(events []agent.Event) string {
	var text string
	for _, event := range events {
		if typed, ok := event.(agent.Text); ok {
			text += typed.Text
		}
	}
	return text
}

// tickKind writes done=true once the step count reaches its configured stop.
type tickKind struct{}

func (tickKind) Name() string { return "tick" }

func (tickKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		StopAfter int `json:"stop_after"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{
		{Key: "done", Value: Bool(in.Step+1 >= config.StopAfter)},
	}}), nil
}

// flakyKind fails a configured number of times before succeeding.
type flakyKind struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (f *flakyKind) Name() string { return "flaky" }

func (f *flakyKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("transient")
	}
	return Static(Result{}), nil
}

// sleepKind runs for its configured duration, honouring its context.
type sleepKind struct{}

func (sleepKind) Name() string { return "sleep" }

func (sleepKind) Invoke(ctx context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		D string `json:"d"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	wait, err := time.ParseDuration(config.D)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(wait):
		return Static(Result{}), nil
	}
}

// askKind suspends until it is answered, then writes the answer.
type askKind struct{}

func (askKind) Name() string { return "ask" }

func (askKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(in.Config, &config); err != nil {
		return nil, err
	}
	if answer := in.Answer.Text(); answer != "" {
		return Static(Result{State: []StateUpdate{{Key: config.Key, Value: Text(answer)}}}), nil
	}
	return Static(Result{
		State:   []StateUpdate{{Key: config.Key, Value: Text("draft")}},
		Suspend: &Suspend{Question: message.NewTextContent("what value?")},
	}), nil
}

// parallelKind records how many invocations overlapped.
type parallelKind struct {
	mu      sync.Mutex
	current int
	max     int
}

func (p *parallelKind) Name() string { return "parallel" }

func (p *parallelKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	p.mu.Lock()
	p.current++
	if p.current > p.max {
		p.max = p.current
	}
	p.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	p.mu.Lock()
	p.current--
	p.mu.Unlock()
	return Static(Result{}), nil
}

func drainRun(t *testing.T, run *Run) ([]agent.Event, error) {
	t.Helper()
	var events []agent.Event
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

// streamKind emits a fixed list of text deltas before finishing.
type streamKind struct {
	mu    sync.Mutex
	calls int
	texts []string
}

func (s *streamKind) Name() string { return "stream" }

func (s *streamKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return &textStream{texts: append([]string(nil), s.texts...)}, nil
}

func (s *streamKind) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type textStream struct {
	texts []string
}

func (s *textStream) Next(context.Context) (agent.Event, error) {
	if len(s.texts) == 0 {
		return nil, io.EOF
	}
	text := s.texts[0]
	s.texts = s.texts[1:]
	return agent.Text{Text: text}, nil
}

// emitStatusKind tries to report a lifecycle event the engine owns.
type emitStatusKind struct{}

func (emitStatusKind) Name() string { return "status" }

func (emitStatusKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return &statusStream{}, nil
}

type statusStream struct{ sent bool }

func (s *statusStream) Next(context.Context) (agent.Event, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return agent.Status{State: agent.StateCompleted}, nil
}

// recordingKind streams a fixed list and records that it was closed.
type recordingKind struct {
	texts  []string
	closed *atomic.Bool
}

func (recordingKind) Name() string { return "recording" }

func (k recordingKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return &recordingStream{texts: k.texts, closed: k.closed}, nil
}

type recordingStream struct {
	texts  []string
	closed *atomic.Bool
}

func (s *recordingStream) Next(context.Context) (agent.Event, error) {
	if len(s.texts) == 0 {
		return nil, io.EOF
	}
	text := s.texts[0]
	s.texts = s.texts[1:]
	return agent.Text{Text: text}, nil
}

func (s *recordingStream) Close() error {
	if s.closed != nil {
		s.closed.Store(true)
	}
	return nil
}

// kindFunc adapts a function into a kind.
type kindFunc struct {
	name string
	run  func(context.Context, Invoke) (Result, error)
}

func (k kindFunc) Name() string { return k.name }

func (k kindFunc) Invoke(ctx context.Context, in Invoke) (NodeStream, error) {
	result, err := k.run(ctx, in)
	if err != nil {
		return nil, err
	}
	return Static(result), nil
}

// streamKind that fails after emitting one delta: a retry would duplicate text,
// so it must not be retried.
type failAfterTextKind struct {
	mu    sync.Mutex
	calls int
}

func (k *failAfterTextKind) Name() string { return "fail-after-text" }

func (k *failAfterTextKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	k.mu.Lock()
	k.calls++
	k.mu.Unlock()
	return &failAfterTextStream{}, nil
}

func (k *failAfterTextKind) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

type failAfterTextStream struct{ sent bool }

func (s *failAfterTextStream) Next(context.Context) (agent.Event, error) {
	if !s.sent {
		s.sent = true
		return agent.Text{Text: "partial"}, nil
	}
	return nil, errors.New("transient")
}

// refKind writes a media reference instead of bytes.
type refKind struct{}

func (refKind) Name() string { return "ref" }

func (refKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key string `json:"key"`
	}
	if err := DecodeConfig(in.Config, &config); err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{
		Key:   config.Key,
		Value: ImageRef("image/png", "blob-7"),
	}}}), nil
}

type failingBlobs struct{}

func (failingBlobs) PutBlob(context.Context, []byte) (string, error) {
	return "", errors.New("disk full")
}

// appendKind appends one assistant message to a state key.
type appendKind struct{}

func (appendKind) Name() string { return "append" }

func (appendKind) Invoke(_ context.Context, in Invoke) (NodeStream, error) {
	var config struct {
		Key   string `json:"key"`
		Text  string `json:"text"`
		Delta bool   `json:"delta"`
	}
	if err := DecodeConfig(in.Config, &config); err != nil {
		return nil, err
	}
	msg := message.NewTextMessage(message.RoleAssistant, config.Text)
	if config.Delta {
		return Static(Result{State: []StateUpdate{{
			Key:   config.Key,
			Value: MessagesValue(MessagesFromSlice([]message.Message{msg})),
		}}}), nil
	}
	current, err := in.State(config.Key).Messages()
	if err != nil {
		return nil, err
	}
	return Static(Result{State: []StateUpdate{{
		Key:   config.Key,
		Value: MessagesValue(current.Append(msg)),
	}}}), nil
}

// barrierKind blocks until released and records the maximum overlap.
type barrierKind struct {
	mu      sync.Mutex
	current int
	max     int
	started chan struct{}
	release chan struct{}
}

func (k *barrierKind) Name() string { return "barrier" }

func (k *barrierKind) Invoke(ctx context.Context, _ Invoke) (NodeStream, error) {
	k.mu.Lock()
	k.current++
	if k.current > k.max {
		k.max = k.current
	}
	k.mu.Unlock()
	k.started <- struct{}{}
	select {
	case <-k.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	k.mu.Lock()
	k.current--
	k.mu.Unlock()
	return Static(Result{}), nil
}
