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
			var b strings.Builder
			for _, part := range msg.Parts {
				if part.Type != llm.PartText {
					return errdefs.Validationf("bytedance: system message supports text parts only, got %s", part.Type)
				}
				b.WriteString(part.Text)
			}
			text := strings.TrimSpace(b.String())
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
			assistantText, err := responsesTextContent(msg.Role, msg.Parts)
			if err != nil {
				return err
			}
			if text := strings.TrimSpace(assistantText); text != "" {
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

		content, ok, err := messageContentForResponses(msg)
		if err != nil {
			return err
		}
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

func messageContentForResponses(msg llm.Message) (*arkresponses.MessageContent, bool, error) {
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
				raw, err := json.Marshal(part.Data.Value)
				if err != nil {
					return nil, false, errdefs.Validationf("bytedance: marshal data part: %w", err)
				}
				hasStructured = true
				flushText()
				items = appendResponsesTextContent(items, responsesDataText(part.Data.MimeType, raw))
			}
		case llm.PartFile:
			if part.File != nil {
				hasStructured = true
				flushText()
				if strings.HasPrefix(part.File.MimeType, "image/") {
					items = appendImageContent(items, part.File.URI)
				} else {
					items = appendFileContent(items, part.File)
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
			return nil, false, nil
		}
		return &arkresponses.MessageContent{Union: &arkresponses.MessageContent_ListValue{
			ListValue: &arkresponses.ContentItemList{ListValue: items},
		}}, true, nil
	}

	text := strings.TrimSpace(b.String())
	if text == "" {
		return nil, false, nil
	}
	return &arkresponses.MessageContent{Union: &arkresponses.MessageContent_StringValue{StringValue: text}}, true, nil
}

func appendResponsesTextContent(items []*arkresponses.ContentItem, text string) []*arkresponses.ContentItem {
	if strings.TrimSpace(text) == "" {
		return items
	}
	return append(items, &arkresponses.ContentItem{Union: &arkresponses.ContentItem_Text{
		Text: &arkresponses.ContentItemText{
			Type: arkresponses.ContentItemType_input_text,
			Text: text,
		},
	}})
}

func responsesTextContent(role llm.Role, parts []llm.Part) (string, error) {
	var b strings.Builder
	needsBoundary := false
	for _, part := range parts {
		switch part.Type {
		case llm.PartText:
			if needsBoundary && part.Text != "" {
				ensureResponsesTextBoundary(&b)
			}
			b.WriteString(part.Text)
			needsBoundary = false
		case llm.PartData:
			if part.Data == nil {
				continue
			}
			raw, err := json.Marshal(part.Data.Value)
			if err != nil {
				return "", errdefs.Validationf("bytedance: marshal data part in %s message: %w", role, err)
			}
			ensureResponsesTextBoundary(&b)
			b.WriteString(responsesDataText(part.Data.MimeType, raw))
			needsBoundary = true
		}
	}
	return b.String(), nil
}

func responsesDataText(mime string, raw []byte) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/json"
	}
	return "ByteDance input data\nMIME type: " + mime + "\nJSON:\n" + string(raw)
}

func ensureResponsesTextBoundary(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	s := b.String()
	switch {
	case strings.HasSuffix(s, "\n\n"):
		return
	case strings.HasSuffix(s, "\n"):
		b.WriteByte('\n')
	default:
		b.WriteString("\n\n")
	}
}

func appendImageContent(items []*arkresponses.ContentItem, ref string) []*arkresponses.ContentItem {
	if ref == "" {
		return items
	}
	img := &arkresponses.ContentItemImage{Type: arkresponses.ContentItemType_input_image}
	// The Responses API splits image-by-file-id from image-by-URL into
	// separate fields; a file_id:// reference must not be stuffed into
	// image_url or the server treats "file_id://..." as a (broken) URL.
	if after, ok := strings.CutPrefix(ref, "file_id://"); ok {
		img.FileId = stringPtrIfNotEmpty(after)
	} else {
		url := ref
		img.ImageUrl = &url
	}
	return append(items, &arkresponses.ContentItem{Union: &arkresponses.ContentItem_Image{Image: img}})
}

func appendFileContent(items []*arkresponses.ContentItem, fileRef *llm.FileRef) []*arkresponses.ContentItem {
	if fileRef == nil || fileRef.URI == "" {
		return items
	}
	file := &arkresponses.ContentItemFile{
		Type: arkresponses.ContentItemType_input_file,
	}
	switch {
	case strings.HasPrefix(fileRef.URI, "file_id://"):
		id := strings.TrimPrefix(fileRef.URI, "file_id://")
		file.FileId = stringPtrIfNotEmpty(id)
	case strings.HasPrefix(fileRef.URI, "data:"):
		file.FileData = &fileRef.URI
	default:
		file.FileUrl = &fileRef.URI
	}
	if fileRef.Name != "" {
		name := fileRef.Name
		file.Filename = &name
	} else if file.FileData != nil {
		// Spec: filename is required when using file_data. Derive a
		// usable name from the MIME type when the caller didn't give one.
		if name := filenameFromMime(fileRef.MimeType); name != "" {
			file.Filename = &name
		}
	}
	return append(items, &arkresponses.ContentItem{Union: &arkresponses.ContentItem_File{
		File: file,
	}})
}

// filenameFromMime derives a "file.<ext>" name from a MIME type for
// the file_data path, where the Responses API requires a filename.
// Returns "" for empty/unknown MIME so the caller leaves the field unset.
func filenameFromMime(mime string) string {
	if mime == "" {
		return ""
	}
	i := strings.IndexByte(mime, '/')
	if i < 0 || i == len(mime)-1 {
		return ""
	}
	return "file." + mime[i+1:]
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

// applyResponsesExtra maps Responses API fields the provider-agnostic
// [llm.GenerateOptions] cannot express onto req. Honoured Extra keys
// are documented on the package. store defaults to false because the
// adapter never reads responses back via previous_response_id in the
// common case, so leaving the server default (true) only burns quota.
func applyResponsesExtra(req *arkresponses.ResponsesRequest, opts *llm.GenerateOptions) {
	store := false
	if v, ok := opts.Extra["store"].(bool); ok {
		store = v
	}
	req.Store = &store

	if v, ok := opts.Extra["previous_response_id"].(string); ok && v != "" {
		req.PreviousResponseId = &v
	}

	if thinkType, ok := parseThinkingExtra(opts.Extra["thinking"], opts.Thinking); ok {
		t := thinkType
		req.Thinking = &arkresponses.ResponsesThinking{Type: &t}
	}
	if effort, ok := parseReasoningEffortExtra(opts.Extra["reasoning_effort"]); ok {
		req.Reasoning = &arkresponses.ResponsesReasoning{Effort: effort}
	}
}

// parseThinkingExtra resolves the thinking mode, giving Extra
// ("auto"/"enabled"/"disabled") precedence over the bool
// [llm.WithThinking] so callers can reach the "auto" mode the bool
// cannot express. Returns ok=false when neither is set, so the field
// is omitted and the server applies its own default.
func parseThinkingExtra(v any, opt *bool) (arkresponses.ThinkingType_Enum, bool) {
	if s, ok := v.(string); ok {
		switch strings.ToLower(s) {
		case "auto":
			return arkresponses.ThinkingType_auto, true
		case "enabled":
			return arkresponses.ThinkingType_enabled, true
		case "disabled":
			return arkresponses.ThinkingType_disabled, true
		}
	}
	if opt != nil {
		if *opt {
			return arkresponses.ThinkingType_enabled, true
		}
		return arkresponses.ThinkingType_disabled, true
	}
	return 0, false
}

// parseReasoningEffortExtra maps an effort string to the Responses
// API reasoning.effort enum. Unknown values are ignored (field omitted).
func parseReasoningEffortExtra(v any) (arkresponses.ReasoningEffort_Enum, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	switch strings.ToLower(s) {
	case "minimal":
		return arkresponses.ReasoningEffort_minimal, true
	case "low":
		return arkresponses.ReasoningEffort_low, true
	case "medium":
		return arkresponses.ReasoningEffort_medium, true
	case "high":
		return arkresponses.ReasoningEffort_high, true
	}
	return 0, false
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
		return classifyResponseEventError("stream error", e.GetCode(), e.GetMessage())
	}
	if e := event.GetResponseFailed(); e != nil {
		resp := e.GetResponse()
		if apiErr := resp.GetError(); apiErr != nil {
			return classifyResponseEventError("response failed", apiErr.GetCode(), apiErr.GetMessage())
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

func classifyResponseEventError(prefix, code, message string) error {
	msg := strings.TrimSpace(prefix)
	if code != "" {
		msg += " " + code
	}
	if message != "" {
		msg += ": " + message
	}
	err := errdefs.Fmt("bytedance: %s", msg)
	// Prefer an exact (case-insensitive) match on the provider's error
	// code: providers reuse free-form messages that contain words like
	// "context" or "auth" for unrelated errors, so substring matching on
	// the message misclassifies. Fall back to substring only when the
	// code is not a recognised token.
	switch strings.ToLower(code) {
	case "rate_limit", "rate_limit_exceeded", "ratelimit", "ratelimitexceeded",
		"rate_limit_error", "429":
		return errdefs.RateLimit(err)
	case "unauthorized", "authentication_error", "auth_error",
		"permission_error", "permission_denied", "401", "403":
		return errdefs.Unauthorized(err)
	case "forbidden", "insufficient_quota_error", "402":
		return errdefs.Forbidden(err)
	case "invalid_request_error", "invalid_parameter", "invalid",
		"bad_request", "badrequest", "not_found", "not_found_error",
		"validation_error", "400", "404":
		return errdefs.Validation(err)
	}
	lower := strings.ToLower(code + " " + message)
	switch {
	case strings.Contains(lower, "rate"):
		return errdefs.RateLimit(err)
	case strings.Contains(lower, "permission denied"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "forbidden"):
		return errdefs.Forbidden(err)
	case strings.Contains(lower, "auth") || strings.Contains(lower, "unauthorized"):
		return errdefs.Unauthorized(err)
	case strings.Contains(lower, "invalid") || strings.Contains(lower, "badrequest") ||
		strings.Contains(lower, "notfound") || strings.Contains(lower, "context"):
		return errdefs.Validation(err)
	default:
		return errdefs.ClassifyProviderError("bytedance", err)
	}
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
