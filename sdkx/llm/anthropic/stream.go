package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// --- Beta stream (JSON mode) ---

type anthropicBetaStreamMessage struct {
	baseCtx context.Context
	span    trace.Span
	// provider carries the OTel/metrics tag through the streaming
	// finish path so a minimax / direct-anthropic stream lands its
	// success/error metric under the right sub-provider name. See
	// sdkx/llm/openai/stream.go for the same pattern.
	provider string
	model    string
	start    time.Time
	stream   *ssestream.Stream[asdk.BetaRawMessageStreamEventUnion]

	allowPartialJSON bool

	mu    sync.Mutex
	usage llm.Usage
	// grossInputTokens is the sum of new + cache_read +
	// cache_creation input tokens. Anthropic's wire `input_tokens`
	// is the *non-cached new* portion only; we recombine it with
	// the cache buckets here to honour the TokenUsage contract
	// that InputTokens is the gross prompt size.
	grossInputTokens int64
	closeOnce        sync.Once
	finishOnce       sync.Once

	blockTypes map[int64]string
	textBuf    strings.Builder

	cur llm.StreamChunk
	err error
}

func newBetaStreamMessage(
	ctx context.Context,
	span trace.Span,
	provider, model string,
	stream *ssestream.Stream[asdk.BetaRawMessageStreamEventUnion],
) llm.StreamMessage {
	return &anthropicBetaStreamMessage{
		baseCtx:          ctx,
		span:             span,
		provider:         provider,
		model:            model,
		start:            time.Now(),
		stream:           stream,
		allowPartialJSON: true,
		blockTypes:       make(map[int64]string),
	}
}

func (s *anthropicBetaStreamMessage) Next() bool {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return false
	}
	if err := s.baseCtx.Err(); err != nil {
		s.err = errdefs.FromContext(err)
		s.mu.Unlock()
		s.betaFinish(s.err)
		return false
	}
	s.mu.Unlock()

	for {
		if !s.stream.Next() {
			err := s.stream.Err()
			if err != nil {
				err = classifyAPIErrorWithProvider(s.provider, err)
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.betaFinish(err)
			return false
		}

		ev := s.stream.Current()
		s.betaUpdateUsage(ev)
		s.betaObserveBlockType(ev)

		text := s.betaExtractDeltaText(ev)
		if text == "" {
			continue
		}

		s.mu.Lock()
		s.textBuf.WriteString(text)
		s.cur = llm.StreamChunk{
			Role:    llm.RoleAssistant,
			Content: text,
		}
		s.mu.Unlock()
		return true
	}
}

func (s *anthropicBetaStreamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *anthropicBetaStreamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *anthropicBetaStreamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *anthropicBetaStreamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		cerr = s.stream.Close()
		s.betaFinish(cerr)
	})
	return cerr
}

func (s *anthropicBetaStreamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.textBuf.Len() == 0 {
		return llm.Message{}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: s.textBuf.String()}}}
}

func (s *anthropicBetaStreamMessage) betaUpdateUsage(ev asdk.BetaRawMessageStreamEventUnion) {
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
			return
		}
		// Anthropic ships the cache buckets in message_start only;
		// later message_delta events refresh output_tokens and
		// (rarely) input_tokens but never re-publish the cache
		// counters, so we latch them here via the shared helper.
		gross, cached := normalizeAnthropicUsage(u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
		s.mu.Lock()
		s.usage.InputTokens = gross
		s.usage.CachedInputTokens = cached
		s.usage.OutputTokens = u.OutputTokens
		s.grossInputTokens = gross
		s.mu.Unlock()
	case "message_delta":
		s.mu.Lock()
		// Preserve gross input from message_start; the SDK's delta
		// only republishes the non-cached portion, so writing
		// ev.Usage.InputTokens directly would erase the cached
		// contribution we just latched.
		if s.grossInputTokens > 0 {
			s.usage.InputTokens = s.grossInputTokens
		} else {
			s.usage.InputTokens = ev.Usage.InputTokens
		}
		s.usage.OutputTokens = ev.Usage.OutputTokens
		s.mu.Unlock()
	}
}

func (s *anthropicBetaStreamMessage) betaObserveBlockType(ev asdk.BetaRawMessageStreamEventUnion) {
	if ev.Type != "content_block_start" {
		return
	}
	if t := ev.ContentBlock.Type; t != "" {
		s.mu.Lock()
		s.blockTypes[ev.Index] = t
		s.mu.Unlock()
	}
}

func (s *anthropicBetaStreamMessage) betaExtractDeltaText(ev asdk.BetaRawMessageStreamEventUnion) string {
	if ev.Type != "content_block_delta" {
		return ""
	}
	if ev.Delta.Text != "" {
		return ev.Delta.Text
	}
	if s.allowPartialJSON && ev.Delta.PartialJSON != "" {
		if t, ok := s.blockTypes[ev.Index]; ok && t != "" && t != "text" {
			return ""
		}
		return ev.Delta.PartialJSON
	}
	return ""
}

func (s *anthropicBetaStreamMessage) betaFinish(err error) {
	s.finishOnce.Do(func() {
		dur := time.Since(s.start)
		s.mu.Lock()
		usage := s.usage
		s.mu.Unlock()
		if err != nil {
			s.span.RecordError(err)
			s.span.SetStatus(codes.Error, err.Error())
			llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "error", dur, llm.TokenUsage{
				InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			})
		} else {
			final := llm.TokenUsage{
				InputTokens:       usage.InputTokens,
				CachedInputTokens: usage.CachedInputTokens,
				OutputTokens:      usage.OutputTokens,
				TotalTokens:       usage.InputTokens + usage.OutputTokens,
			}
			s.span.SetAttributes(llm.UsageSpanAttrs(final)...)
			s.span.SetStatus(codes.Ok, "OK")
			llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "success", dur, final)
		}
		s.span.End()
	})
}

// --- Stable stream ---

type anthropicStreamMessage struct {
	baseCtx context.Context
	span    trace.Span
	// provider carries the OTel/metrics tag through the streaming
	// finish path; see anthropicBetaStreamMessage above.
	provider string
	model    string
	start    time.Time
	stream   *ssestream.Stream[asdk.MessageStreamEventUnion]

	mu    sync.Mutex
	usage llm.Usage
	// grossInputTokens — see anthropicBetaStreamMessage.
	grossInputTokens int64
	closeOnce        sync.Once
	finishOnce       sync.Once

	blockTypes map[int64]string

	// accumulated state for Message()
	textBuf   strings.Builder
	toolCalls []llm.ToolCall
	curToolID map[int64]*llm.ToolCall // in-flight tool_use blocks by index

	cur llm.StreamChunk
	err error
}

func newStreamMessage(
	ctx context.Context,
	span trace.Span,
	provider, model string,
	stream *ssestream.Stream[asdk.MessageStreamEventUnion],
) llm.StreamMessage {
	return &anthropicStreamMessage{
		baseCtx:    ctx,
		span:       span,
		provider:   provider,
		model:      model,
		start:      time.Now(),
		stream:     stream,
		blockTypes: make(map[int64]string),
		curToolID:  make(map[int64]*llm.ToolCall),
	}
}

func (s *anthropicStreamMessage) Next() bool {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return false
	}
	if err := s.baseCtx.Err(); err != nil {
		s.err = errdefs.FromContext(err)
		s.mu.Unlock()
		s.finish(s.err)
		return false
	}
	s.mu.Unlock()

	for {
		if !s.stream.Next() {
			err := s.stream.Err()
			if err != nil {
				err = classifyAPIErrorWithProvider(s.provider, err)
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}

		ev := s.stream.Current()
		s.updateUsage(ev)
		s.observeBlockType(ev)
		s.accumulateToolUse(ev)

		text := s.extractDeltaText(ev)
		if text == "" {
			continue
		}

		s.mu.Lock()
		s.textBuf.WriteString(text)
		s.cur = llm.StreamChunk{
			Role:    llm.RoleAssistant,
			Content: text,
		}
		s.mu.Unlock()
		return true
	}
}

func (s *anthropicStreamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *anthropicStreamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *anthropicStreamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *anthropicStreamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		cerr = s.stream.Close()
		s.finish(cerr)
	})
	return cerr
}

func (s *anthropicStreamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	var parts []llm.Part
	if s.textBuf.Len() > 0 {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: s.textBuf.String()})
	}
	for i := range s.toolCalls {
		parts = append(parts, llm.Part{
			Type:     llm.PartToolCall,
			ToolCall: &s.toolCalls[i],
		})
	}
	if len(parts) == 0 {
		return llm.Message{}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func (s *anthropicStreamMessage) updateUsage(ev asdk.MessageStreamEventUnion) {
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
			return
		}
		gross, cached := normalizeAnthropicUsage(u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
		s.mu.Lock()
		s.usage.InputTokens = gross
		s.usage.CachedInputTokens = cached
		s.usage.OutputTokens = u.OutputTokens
		s.grossInputTokens = gross
		s.mu.Unlock()
	case "message_delta":
		s.mu.Lock()
		if s.grossInputTokens > 0 {
			s.usage.InputTokens = s.grossInputTokens
		} else {
			s.usage.InputTokens = ev.Usage.InputTokens
		}
		s.usage.OutputTokens = ev.Usage.OutputTokens
		s.mu.Unlock()
	}
}

func (s *anthropicStreamMessage) observeBlockType(ev asdk.MessageStreamEventUnion) {
	if ev.Type != "content_block_start" {
		return
	}
	if t := ev.ContentBlock.Type; t != "" {
		s.mu.Lock()
		s.blockTypes[ev.Index] = t
		s.mu.Unlock()
	}
}

func (s *anthropicStreamMessage) accumulateToolUse(ev asdk.MessageStreamEventUnion) {
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			tc := &llm.ToolCall{
				ID:   ev.ContentBlock.ID,
				Name: ev.ContentBlock.Name,
			}
			s.mu.Lock()
			s.curToolID[ev.Index] = tc
			s.mu.Unlock()
		}
	case "content_block_delta":
		if ev.Delta.PartialJSON != "" {
			s.mu.Lock()
			if tc, ok := s.curToolID[ev.Index]; ok {
				tc.Arguments += ev.Delta.PartialJSON
			}
			s.mu.Unlock()
		}
	case "content_block_stop":
		s.mu.Lock()
		if tc, ok := s.curToolID[ev.Index]; ok {
			// Validate JSON arguments
			if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
				tc.Arguments = "{}"
			}
			s.toolCalls = append(s.toolCalls, *tc)
			delete(s.curToolID, ev.Index)
		}
		s.mu.Unlock()
	}
}

func (s *anthropicStreamMessage) extractDeltaText(ev asdk.MessageStreamEventUnion) string {
	if ev.Type != "content_block_delta" {
		return ""
	}
	if ev.Delta.Text != "" {
		return ev.Delta.Text
	}
	return ""
}

func (s *anthropicStreamMessage) finish(err error) {
	s.finishOnce.Do(func() {
		dur := time.Since(s.start)
		s.mu.Lock()
		usage := s.usage
		s.mu.Unlock()

		if err != nil {
			s.span.RecordError(err)
			s.span.SetStatus(codes.Error, err.Error())
			llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "error", dur, llm.TokenUsage{
				InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			})
		} else {
			final := llm.TokenUsage{
				InputTokens:       usage.InputTokens,
				CachedInputTokens: usage.CachedInputTokens,
				OutputTokens:      usage.OutputTokens,
				TotalTokens:       usage.InputTokens + usage.OutputTokens,
			}
			s.span.SetAttributes(llm.UsageSpanAttrs(final)...)
			s.span.SetStatus(codes.Ok, "OK")
			llm.RecordLLMMetrics(s.baseCtx, s.provider, s.model, "success", dur, final)
		}
		s.span.End()
	})
}
