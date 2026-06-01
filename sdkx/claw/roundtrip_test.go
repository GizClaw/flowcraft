package claw

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRoundTripStreamsTokensAndResult(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app, err := New(ws, WithConfig(defaultConfig()), WithChatModel(staticLLM{reply: "hello there"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	resp, err := app.RoundTrip("ctx", Request{Text: "hi"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var tokens strings.Builder
	var sawResult bool
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Type {
		case EventToken:
			tokens.WriteString(ev.Content)
		case EventResult:
			sawResult = ev.Result != nil
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.Err)
		}
	}
	if got := tokens.String(); got != "hello there" {
		t.Fatalf("tokens = %q, want %q", got, "hello there")
	}
	if !sawResult {
		t.Fatal("missing result event")
	}
}

func TestRoundTripRejectsConcurrentSameContext(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	llm := &blockingLLM{release: make(chan struct{})}
	app, err := New(ws, WithConfig(defaultConfig()), WithChatModel(llm))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	resp, err := app.RoundTrip("ctx", Request{Text: "hi"})
	if err != nil {
		t.Fatalf("RoundTrip first: %v", err)
	}
	if _, err := app.RoundTrip("ctx", Request{Text: "again"}); err == nil {
		t.Fatal("RoundTrip second succeeded, want conflict")
	}
	close(llm.release)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("first response did not finish")
		default:
		}
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
}

func TestRoundTripPersistsContextState(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app, err := New(ws, WithConfig(defaultConfig()), WithChatModel(staticLLM{reply: "ok"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	resp, err := app.RoundTrip("story", Request{
		Text:   "continue",
		Inputs: map[string]any{"current_arc": "arc_02_heaven"},
	})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	for {
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	st, err := app.loadContextState(context.Background(), "story")
	if err != nil {
		t.Fatalf("loadContextState: %v", err)
	}
	if st.Vars["current_arc"] != "arc_02_heaven" {
		t.Fatalf("current_arc = %v, want arc_02_heaven", st.Vars["current_arc"])
	}
}

type staticLLM struct {
	reply string
}

func (s staticLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	msg := llm.NewTextMessage(llm.RoleAssistant, s.reply)
	return msg, llm.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, nil
}

func (s staticLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg := llm.NewTextMessage(llm.RoleAssistant, s.reply)
	return &sliceStream{msg: msg, chunks: []string{s.reply}}, nil
}

type blockingLLM struct {
	release chan struct{}
}

func (b *blockingLLM) Generate(ctx context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return llm.Message{}, llm.TokenUsage{}, ctx.Err()
	}
	msg := llm.NewTextMessage(llm.RoleAssistant, "done")
	return msg, llm.TokenUsage{}, nil
}

func (b *blockingLLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := b.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return llm.NewOneChunkStream(msg, usage), nil
}

type sliceStream struct {
	msg    llm.Message
	chunks []string
	idx    int
	cur    model.StreamChunk
}

func (s *sliceStream) Next() bool {
	if s.idx >= len(s.chunks) {
		return false
	}
	s.cur = model.StreamChunk{Role: model.RoleAssistant, Content: s.chunks[s.idx]}
	s.idx++
	return true
}

func (s *sliceStream) Current() model.StreamChunk { return s.cur }
func (s *sliceStream) Err() error                 { return nil }
func (s *sliceStream) Close() error               { return nil }
func (s *sliceStream) Message() model.Message     { return s.msg }
func (s *sliceStream) Usage() model.Usage         { return model.Usage{} }
