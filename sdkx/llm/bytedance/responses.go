package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func appendResponsesMessages(req *arkresponses.ResponsesRequest, msgs []llm.Message) error {
	list := req.Input.GetListValue()
	if list == nil {
		list = &arkresponses.InputItemList{}
		req.Input = &arkresponses.ResponsesInput{Union: &arkresponses.ResponsesInput_ListValue{ListValue: list}}
	}

	for _, msg := range msgs {
		if msg.Role == llm.RoleSystem {
			text := strings.TrimSpace(msg.Content())
			if text == "" {
				continue
			}
			if req.Instructions == nil {
				req.Instructions = &text
			} else {
				joined := *req.Instructions + "\n\n" + text
				req.Instructions = &joined
			}
			continue
		}

		if msg.Role == llm.RoleTool {
			for _, result := range msg.ToolResults() {
				list.ListValue = append(list.ListValue, &arkresponses.InputItem{Union: &arkresponses.InputItem_FunctionToolCallOutput{
					FunctionToolCallOutput: &arkresponses.ItemFunctionToolCallOutput{
						Type:   arkresponses.ItemType_function_call_output,
						CallId: result.ToolCallID,
						Output: result.Content,
					},
				}})
			}
			continue
		}

		if msg.Role == llm.RoleAssistant && msg.HasToolCalls() {
			if text := strings.TrimSpace(msg.Content()); text != "" {
				list.ListValue = append(list.ListValue, inputMessage(msg.Role, &arkresponses.MessageContent{
					Union: &arkresponses.MessageContent_StringValue{StringValue: text},
				}))
			}
			for _, call := range msg.ToolCalls() {
				list.ListValue = append(list.ListValue, &arkresponses.InputItem{Union: &arkresponses.InputItem_FunctionToolCall{
					FunctionToolCall: &arkresponses.ItemFunctionToolCall{
						Type:      arkresponses.ItemType_function_call,
						CallId:    call.ID,
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				}})
			}
			continue
		}

		content, ok := messageContentForResponses(msg)
		if !ok {
			continue
		}
		list.ListValue = append(list.ListValue, inputMessage(msg.Role, content))
	}
	return nil
}

func inputMessage(role llm.Role, content *arkresponses.MessageContent) *arkresponses.InputItem {
	return &arkresponses.InputItem{Union: &arkresponses.InputItem_EasyMessage{
		EasyMessage: &arkresponses.ItemEasyMessage{
			Type:    arkresponses.ItemType_message.Enum(),
			Role:    responsesRole(role),
			Content: content,
		},
	}}
}

func responsesRole(role llm.Role) arkresponses.MessageRole_Enum {
	switch role {
	case llm.RoleAssistant:
		return arkresponses.MessageRole_assistant
	case llm.RoleSystem:
		return arkresponses.MessageRole_system
	default:
		return arkresponses.MessageRole_user
	}
}

func messageContentForResponses(msg llm.Message) (*arkresponses.MessageContent, bool) {
	var b strings.Builder
	var items []*arkresponses.ContentItem
	hasStructured := false
	flushText := func() {
		if b.Len() == 0 {
			return
		}
		text := b.String()
		b.Reset()
		if strings.TrimSpace(text) == "" {
			return
		}
		items = append(items, &arkresponses.ContentItem{Union: &arkresponses.ContentItem_Text{
			Text: &arkresponses.ContentItemText{
				Type: arkresponses.ContentItemType_input_text,
				Text: text,
			},
		}})
	}

	for _, part := range msg.Parts {
		switch part.Type {
		case llm.PartText:
			b.WriteString(part.Text)
		case llm.PartData:
			if part.Data != nil {
				raw, _ := json.Marshal(part.Data.Value)
				b.Write(raw)
			}
		case llm.PartFile:
			if part.File != nil {
				if strings.HasPrefix(part.File.MimeType, "image/") {
					hasStructured = true
					flushText()
					items = appendImageContent(items, part.File.URI)
				} else {
					b.WriteString(part.File.URI)
				}
			}
		case llm.PartImage:
			if part.Image != nil {
				if url := mediaURL(part.Image); url != "" {
					hasStructured = true
					flushText()
					items = appendImageContent(items, url)
				}
			}
		}
	}
	if hasStructured {
		flushText()
		if len(items) == 0 {
			return nil, false
		}
		return &arkresponses.MessageContent{Union: &arkresponses.MessageContent_ListValue{
			ListValue: &arkresponses.ContentItemList{ListValue: items},
		}}, true
	}

	text := strings.TrimSpace(b.String())
	if text == "" {
		return nil, false
	}
	return &arkresponses.MessageContent{Union: &arkresponses.MessageContent_StringValue{StringValue: text}}, true
}

func appendImageContent(items []*arkresponses.ContentItem, url string) []*arkresponses.ContentItem {
	if url == "" {
		return items
	}
	return append(items, &arkresponses.ContentItem{Union: &arkresponses.ContentItem_Image{
		Image: &arkresponses.ContentItemImage{
			Type:     arkresponses.ContentItemType_input_image,
			ImageUrl: &url,
		},
	}})
}

func mediaURL(media *llm.MediaRef) string {
	if media.URL != "" {
		return media.URL
	}
	if media.Base64 == "" {
		return ""
	}
	mime := media.MediaType
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + media.Base64
}

type webSearchConfig struct {
	Enabled    bool
	MaxKeyword int
	Limit      int
}

func (c webSearchConfig) tool() *arkresponses.ResponsesTool {
	tool := &arkresponses.ToolWebSearch{Type: arkresponses.ToolType_web_search}
	if c.Limit > 0 {
		v := int64(c.Limit)
		tool.Limit = &v
	}
	if c.MaxKeyword > 0 {
		v := int32(c.MaxKeyword)
		tool.MaxKeyword = &v
	}
	return &arkresponses.ResponsesTool{Union: &arkresponses.ResponsesTool_ToolWebSearch{ToolWebSearch: tool}}
}

func parseWebSearchConfig(v any) webSearchConfig {
	switch x := v.(type) {
	case bool:
		return webSearchConfig{Enabled: x}
	case map[string]any:
		return webSearchConfig{
			Enabled:    boolConfig(x["enabled"]),
			MaxKeyword: intConfig(x["max_keyword"]),
			Limit:      intConfig(x["limit"]),
		}
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			if s, ok := k.(string); ok {
				m[s] = v
			}
		}
		return parseWebSearchConfig(m)
	default:
		return webSearchConfig{}
	}
}

func responseMessage(resp *arkresponses.ResponseObject) llm.Message {
	var parts []llm.Part
	for _, item := range resp.GetOutput() {
		if msg := item.GetOutputMessage(); msg != nil {
			for _, content := range msg.GetContent() {
				if text := content.GetText(); text != nil && text.GetText() != "" {
					parts = append(parts, llm.Part{Type: llm.PartText, Text: text.GetText()})
				}
			}
			continue
		}
		if call := item.GetFunctionToolCall(); call != nil {
			parts = append(parts, llm.Part{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				ID:        call.GetCallId(),
				Name:      call.GetName(),
				Arguments: call.GetArguments(),
			}})
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

func responseText(resp *arkresponses.ResponseObject) string {
	return responseMessage(resp).Content()
}

func responseUsage(resp *arkresponses.ResponseObject) llm.TokenUsage {
	usage := resp.GetUsage()
	out := llm.TokenUsage{
		InputTokens:       usage.GetInputTokens(),
		OutputTokens:      usage.GetOutputTokens(),
		TotalTokens:       usage.GetTotalTokens(),
		CachedInputTokens: usage.GetInputTokensDetails().GetCachedTokens(),
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

type responsesStreamMessage struct {
	baseCtx context.Context
	span    trace.Span
	model   string
	stream  *utils.ResponsesStreamReader
	start   time.Time

	mu        sync.Mutex
	content   string
	pending   []byte
	msg       llm.Message
	toolCalls map[int]llm.ToolCall
	usage     llm.Usage
	cur       llm.StreamChunk
	err       error
	closeOnce sync.Once
	spanEnded bool
}

func newResponsesStreamMessage(ctx context.Context, span trace.Span, modelName string, stream *utils.ResponsesStreamReader) llm.StreamMessage {
	return &responsesStreamMessage{
		baseCtx: ctx,
		span:    span,
		model:   modelName,
		stream:  stream,
		start:   time.Now(),
	}
}

func (s *responsesStreamMessage) Next() bool {
	s.mu.Lock()
	if s.err != nil || s.stream == nil {
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
		event, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.mu.Lock()
				s.stream = nil
				s.ensureMessageLocked()
				s.mu.Unlock()
				s.finish(nil)
				return false
			}
			err = classifyAPIError(err)
			s.mu.Lock()
			s.stream = nil
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}
		if event == nil {
			continue
		}
		if err := s.applyEvent(event); err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.finish(err)
			return false
		}
		if text := eventDeltaText(event); text != "" {
			s.mu.Lock()
			text = s.appendDeltaTextLocked(text)
			s.mu.Unlock()
			if text != "" {
				return true
			}
		}
	}
}

func (s *responsesStreamMessage) appendDeltaTextLocked(text string) string {
	s.pending = append(s.pending, text...)
	text, pending := drainValidUTF8Text(s.pending)
	s.pending = pending
	if text == "" {
		return ""
	}
	s.content += text
	s.cur = llm.StreamChunk{Role: llm.RoleAssistant, Content: text}
	return text
}

func drainValidUTF8Text(buf []byte) (string, []byte) {
	var out strings.Builder
	i := 0
	for i < len(buf) {
		r, size := utf8.DecodeRune(buf[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(buf[i:]) {
				break
			}
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		out.WriteRune(r)
		i += size
	}
	pending := append([]byte(nil), buf[i:]...)
	return out.String(), pending
}

func (s *responsesStreamMessage) applyEvent(event *arkresponses.Event) error {
	if e := event.GetError(); e != nil {
		return errdefs.NotAvailablef("bytedance: stream error %s: %s", e.GetCode(), e.GetMessage())
	}
	if e := event.GetResponseFailed(); e != nil {
		resp := e.GetResponse()
		if apiErr := resp.GetError(); apiErr != nil {
			return errdefs.NotAvailablef("bytedance: response failed %s: %s", apiErr.GetCode(), apiErr.GetMessage())
		}
		return errdefs.NotAvailablef("bytedance: response failed")
	}
	if e := event.GetResponseIncomplete(); e != nil {
		return errdefs.NotAvailablef("bytedance: response incomplete: %s", e.GetResponse().GetIncompleteDetails().GetReason())
	}
	if e := event.GetResponseCompleted(); e != nil {
		msg := responseMessage(e.GetResponse())
		usage := responseUsage(e.GetResponse())
		s.mu.Lock()
		s.msg = msg
		s.usage.InputTokens = usage.InputTokens
		s.usage.CachedInputTokens = usage.CachedInputTokens
		s.usage.OutputTokens = usage.OutputTokens
		s.mu.Unlock()
	}
	if e := event.GetItem(); e != nil {
		s.accumulateOutputItem(e.GetOutputIndex(), e.GetItem())
	}
	if e := event.GetItemDone(); e != nil {
		s.accumulateOutputItem(e.GetOutputIndex(), e.GetItem())
	}
	if e := event.GetFunctionCallArguments(); e != nil {
		s.accumulateFunctionArguments(e.GetOutputIndex(), e.GetDelta(), e.GetArguments())
	}
	if e := event.GetFunctionCallArgumentsDone(); e != nil {
		s.accumulateFunctionArguments(e.GetOutputIndex(), e.GetDelta(), e.GetArguments())
	}
	return nil
}

func (s *responsesStreamMessage) accumulateOutputItem(outputIndex int64, item *arkresponses.OutputItem) {
	if item == nil {
		return
	}
	call := item.GetFunctionToolCall()
	if call == nil {
		return
	}
	idx := int(outputIndex)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolCalls == nil {
		s.toolCalls = make(map[int]llm.ToolCall)
	}
	existing := s.toolCalls[idx]
	if call.GetCallId() != "" {
		existing.ID = call.GetCallId()
	}
	if call.GetName() != "" {
		existing.Name = call.GetName()
	}
	if call.GetArguments() != "" {
		existing.Arguments = call.GetArguments()
	}
	s.toolCalls[idx] = existing
}

func (s *responsesStreamMessage) accumulateFunctionArguments(outputIndex int64, delta, arguments string) {
	if delta == "" && arguments == "" {
		return
	}
	idx := int(outputIndex)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolCalls == nil {
		s.toolCalls = make(map[int]llm.ToolCall)
	}
	existing := s.toolCalls[idx]
	if arguments != "" {
		existing.Arguments = arguments
	} else {
		existing.Arguments += delta
	}
	s.toolCalls[idx] = existing
}

func eventDeltaText(event *arkresponses.Event) string {
	if e := event.GetText(); e != nil {
		return e.GetDelta()
	}
	if e := event.GetResponseDoubaoAppCallOutputTextDelta(); e != nil {
		return e.GetDelta()
	}
	return ""
}

func (s *responsesStreamMessage) Current() llm.StreamChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *responsesStreamMessage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *responsesStreamMessage) Usage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *responsesStreamMessage) Message() llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMessageLocked()
	return s.msg
}

func (s *responsesStreamMessage) Close() error {
	var cerr error
	s.closeOnce.Do(func() {
		if s.stream != nil {
			cerr = s.stream.Close()
			s.stream = nil
		}
		s.finish(cerr)
	})
	return cerr
}

func (s *responsesStreamMessage) ensureMessageLocked() {
	if len(s.msg.Parts) == 0 && s.content != "" {
		s.msg.Parts = append(s.msg.Parts, llm.Part{Type: llm.PartText, Text: s.content})
	}
	if !s.msg.HasToolCalls() {
		for _, tc := range s.sortedToolCallsLocked() {
			tc := tc
			s.msg.Parts = append(s.msg.Parts, llm.Part{Type: llm.PartToolCall, ToolCall: &tc})
		}
	}
	if len(s.msg.Parts) > 0 {
		s.msg.Role = llm.RoleAssistant
	}
}

func (s *responsesStreamMessage) sortedToolCallsLocked() []llm.ToolCall {
	if len(s.toolCalls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(s.toolCalls))
	for idx := range s.toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	calls := make([]llm.ToolCall, 0, len(indices))
	for _, idx := range indices {
		calls = append(calls, s.toolCalls[idx])
	}
	return calls
}

func (s *responsesStreamMessage) finish(err error) {
	s.mu.Lock()
	if s.spanEnded {
		s.mu.Unlock()
		return
	}
	s.spanEnded = true
	usage := s.usage
	s.mu.Unlock()

	dur := time.Since(s.start)
	tokens := llm.TokenUsage{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.InputTokens + usage.OutputTokens,
	}
	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
		llm.RecordLLMMetrics(s.baseCtx, "bytedance", s.model, "error", dur, tokens)
	} else {
		s.span.SetAttributes(llm.UsageSpanAttrs(tokens)...)
		s.span.SetStatus(codes.Ok, "OK")
		llm.RecordLLMMetrics(s.baseCtx, "bytedance", s.model, "success", dur, tokens)
	}
	s.span.End()
}

func stringPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intConfig(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

func boolConfig(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	default:
		return false
	}
}
