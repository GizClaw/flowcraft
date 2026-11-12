package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// Model returns the "model" node kind: one model call per invocation.
//
// config:
//
//	messages: state key holding the transcript (type messages), required
//	request:  the call itself, mapped one-to-one onto inference.GenerateRequest
//	pending:  state key for the tool calls the model asked for (type json)
//	usage:    state key for the call's accounting (type json)
//	output:   state key for the assistant's text (type text)
//
// request:
//
//	model:      {provider, name, profile}; default is the runtime's model
//	hint:       per-call preference for the selection layer
//	output:     text | image | audio | video; default text
//	system:     prepended only when the context does not already start with one
//	tools:      {enabled, names, require, choice}
//	extensions: provider knobs, decoded through the decoders the host supplied
//
// It streams: text and reasoning deltas become agent.Text and agent.Reasoning
// events while the call runs, and the assembled assistant turn is written to the
// transcript when it ends.
type modelKind struct {
	rt       *runtime.Runtime
	decoders map[string]inference.ExtensionDecoder
}

// DecodeConfig parses the node's config at Build, so a run does not decode it
// again for every call.
func (*modelKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return decodeModelConfig(raw)
}

func decodeModelConfig(raw json.RawMessage) (modelConfig, error) {
	var config modelConfig
	if err := graph.DecodeConfig(raw, &config); err != nil {
		return modelConfig{}, err
	}
	return config, nil
}

// ModelOption configures the model kind.
type ModelOption func(*modelKind)

// WithExtensionDecoders supplies the provider extension decoders a config's
// request.extensions resolves against. A node that is given none refuses an
// extension entry instead of silently ignoring it.
func WithExtensionDecoders(decoders map[string]inference.ExtensionDecoder) ModelOption {
	return func(m *modelKind) { m.decoders = decoders }
}

// Model builds the model kind over a runtime.
func Model(rt *runtime.Runtime, opts ...ModelOption) graph.NodeKind {
	m := &modelKind{rt: rt}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

func (*modelKind) Name() string { return "model" }

// modelConfig is the node's config. It is decoded strictly: a misspelled field
// is a load error, not a silently defaulted setting.
type modelConfig struct {
	Messages string        `json:"messages"`
	Pending  string        `json:"pending"`
	Usage    string        `json:"usage"`
	Output   string        `json:"output"`
	Request  requestConfig `json:"request"`
}

type requestConfig struct {
	Model      *modelRef                  `json:"model"`
	Hint       string                     `json:"hint"`
	Output     string                     `json:"output"`
	System     string                     `json:"system"`
	Tools      *toolsConfig               `json:"tools"`
	Extensions []inference.ExtensionEntry `json:"extensions"`
}

type modelRef struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Profile  string `json:"profile"`
}

type toolsConfig struct {
	// Enabled false offers no tools at all.
	Enabled *bool `json:"enabled"`
	// Names restricts the offer to these catalog tools.
	Names []string `json:"names"`
	// Require forces these tools visible in a dynamic catalog view.
	Require []string `json:"require"`
	// Choice constrains when and which tool is called.
	Choice *inference.ToolChoice `json:"choice"`
}

func (m *modelKind) Invoke(ctx context.Context, in graph.Invoke) (graph.NodeStream, error) {
	config, err := graph.ConfigOf(in, decodeModelConfig)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	if strings.TrimSpace(config.Messages) == "" {
		return nil, fmt.Errorf("model: config.messages is required")
	}
	transcript, err := in.State(config.Messages).Messages()
	if err != nil {
		return nil, fmt.Errorf("model: state %q: %w", config.Messages, err)
	}
	if transcript.Len() == 0 {
		return nil, fmt.Errorf("model: state %q holds no messages", config.Messages)
	}
	view, err := m.view(ctx)
	if err != nil {
		return nil, err
	}
	request, err := m.request(ctx, view, config.Request, transcript)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	record := in.Trace.Allows(telemetry.TraceNode) || in.Replay
	bodies := in.Bodies || in.Replay
	var digests []agent.MessageDigest
	var segments []agent.PromptSegment
	var requestHash string
	if record {
		digests, segments = describePrompt(request)
		hashes := make([]string, 0, len(request.Context))
		for i := 0; i < len(request.Context) && i < len(digests); i++ {
			hashes = append(hashes, digests[i].Hash)
		}
		requestHash = inference.RequestHashWith(request, hashes)
	}
	if in.Replay {
		call, ok := in.Lookup(requestHash)
		if !ok {
			return nil, fmt.Errorf("model: replay miss for %s", requestHash)
		}
		return &modelStream{
			replay:      &call,
			view:        view,
			transcript:  transcript,
			messagesKey: config.Messages,
			pendingKey:  config.Pending,
			usageKey:    config.Usage,
			outputKey:   config.Output,
			model:       request.Model,
			requestHash: requestHash,
		}, nil
	}
	started := time.Now()
	stream, err := m.open(ctx, request)
	if err != nil {
		return nil, err
	}
	return &modelStream{
		stream:      stream,
		view:        view,
		transcript:  transcript,
		messagesKey: config.Messages,
		pendingKey:  config.Pending,
		usageKey:    config.Usage,
		outputKey:   config.Output,
		model:       request.Model,
		started:     started,
		record:      record,
		bodies:      bodies,
		digests:     digests,
		segments:    segments,
		requestHash: requestHash,
	}, nil
}

// request maps the node's request block onto the inference contract.
func (m *modelKind) request(
	ctx context.Context,
	view engine.CatalogView,
	config requestConfig,
	transcript graph.Messages,
) (inference.GenerateRequest, error) {
	turns := transcript.Slice()
	last := turns[len(turns)-1]
	role, err := inputRole(last.Role)
	if err != nil {
		return inference.GenerateRequest{}, err
	}
	request := inference.GenerateRequest{
		Model:     m.modelRef(config.Model),
		ModelHint: config.Hint,
		Context:   turns[:len(turns)-1],
		Input: inference.GenerateInput{
			Role:    role,
			Content: last.Content,
		},
	}
	output := inference.Output(config.Output)
	if err := output.Validate(); err != nil {
		return inference.GenerateRequest{}, fmt.Errorf("request.output: %w", err)
	}
	request.Output = output

	// A system prompt the deployment names is framing, not a turn: it is added
	// only when the transcript does not already open with one, so a graph that
	// also stores its system turn is not framed twice.
	if system := strings.TrimSpace(config.System); system != "" {
		if len(request.Context) == 0 || request.Context[0].Role != message.RoleSystem {
			// One allocation: copy the turns behind the system message instead
			// of appending to a literal, which would copy the context twice.
			withSystem := make([]message.Message, len(request.Context)+1)
			withSystem[0] = message.NewTextMessage(message.RoleSystem, system)
			copy(withSystem[1:], request.Context)
			request.Context = withSystem
		}
	}
	// Tools apply to text output only; the request contract rejects the
	// combination, so it must not be built in the first place.
	if output == "" || output == inference.OutputText {
		if config.Tools == nil || config.Tools.Enabled == nil || *config.Tools.Enabled {
			tools, err := m.toolset(view, transcript, config.Tools)
			if err != nil {
				return inference.GenerateRequest{}, err
			}
			request.Tools = tools
		}
	}
	extensions, err := inference.DecodeExtensions(config.Extensions, m.decoders, "request.extensions")
	if err != nil {
		return inference.GenerateRequest{}, err
	}
	request.Extensions = extensions
	// Validate what this node built, not the transcript: every history message
	// was checked when it entered state, and re-checking ten thousand of them per
	// call costs more than the request it guards.
	for _, check := range []struct {
		field string
		err   error
	}{
		{"model", request.Model.Validate()},
		{"input", request.Input.Validate()},
		{"output", request.Output.Validate()},
		{"tools", request.Tools.Validate()},
	} {
		if check.err != nil {
			return inference.GenerateRequest{}, fmt.Errorf("request.%s: %w", check.field, check.err)
		}
	}
	return request, nil
}

// toolset builds what this call may offer.
//
// The projection is rebuilt from the transcript: the tools the model already
// used and the tools tool_search exposed are visible, whichever step produced
// them. Naming a tool in the config pins it visible, so an explicit choice never
// depends on a discovery that may have happened in another branch.
func (m *modelKind) toolset(view engine.CatalogView, transcript graph.Messages, config *toolsConfig) (inference.Toolset, error) {
	if config != nil {
		if len(config.Names) > 0 {
			view.Require(config.Names...)
		}
		if len(config.Require) > 0 {
			view.Require(config.Require...)
		}
	}
	definitions := view.Definitions()
	if config != nil && len(config.Names) > 0 {
		wanted := make(map[string]bool, len(config.Names))
		for _, name := range config.Names {
			wanted[name] = true
		}
		selected := make([]message.ToolDefinition, 0, len(definitions))
		found := make(map[string]bool, len(config.Names))
		for _, definition := range definitions {
			if wanted[definition.Name] {
				selected = append(selected, definition)
				found[definition.Name] = true
			}
		}
		for _, name := range config.Names {
			if !found[name] {
				return inference.Toolset{}, fmt.Errorf(
					"request.tools.names: %q is not wired by the runtime (tools.enable)", name)
			}
		}
		definitions = selected
	}
	// The view's definitions are a fresh slice over schemas that never change, so
	// taking ownership avoids a second deep clone of every schema.
	toolset, err := inference.NewToolsetOwned(definitions)
	if err != nil {
		return inference.Toolset{}, fmt.Errorf("request.tools: %w", err)
	}
	if config != nil && config.Choice != nil {
		toolset, err = toolset.Choosing(*config.Choice)
		if err != nil {
			return inference.Toolset{}, fmt.Errorf("request.tools.choice: %w", err)
		}
	}
	return toolset, nil
}

// view returns the run's catalog projection from the context, or a fresh one for
// a caller that did not attach any (a unit test, a bare engine use). A fresh view
// still offers every visible tool; it just cannot remember discoveries between
// steps.
func (m *modelKind) view(ctx context.Context) (engine.CatalogView, error) {
	if view, ok := runtime.CatalogViewFrom(ctx); ok {
		return view, nil
	}
	return m.rt.NewCatalogView(), nil
}

// modelRef resolves the configured model, or the runtime's default.
func (m *modelKind) modelRef(configured *modelRef) model.ModelRef {
	if configured == nil {
		return m.rt.DefaultModel()
	}
	ref := model.ModelRef{
		ID:      model.ModelID{Provider: configured.Provider, Name: configured.Name},
		Profile: configured.Profile,
	}
	if ref.ID.Name == "" {
		return m.rt.DefaultModel()
	}
	return ref
}

// modelStream forwards the model's deltas while it runs.
type modelStream struct {
	stream      inference.Stream
	view        engine.CatalogView
	transcript  graph.Messages
	messagesKey string
	pendingKey  string
	usageKey    string
	outputKey   string
	model       model.ModelRef
	started     time.Time
	// record says this call's identity is kept: the input digests and the
	// assembled response ride the step's checkpoint.
	record bool
	// bodies says the response's bytes are kept too, which is what a replay
	// answers from. It is narrower than record on purpose: a deployment may want
	// to attribute a prompt without paying for the answers.
	bodies      bool
	requestHash string
	digests     []agent.MessageDigest
	segments    []agent.PromptSegment
	// replay answers from a recorded call instead of the provider.
	replay *agent.LLMCallRecord
	result graph.Result
	done   bool
}

func (s *modelStream) Next(ctx context.Context) (agent.Event, error) {
	if s.replay != nil {
		if s.done {
			return nil, io.EOF
		}
		if len(s.replay.Response) == 0 {
			return nil, errdefs.NotAvailablef(
				"model: replay %s: this task recorded no response body (bodies is off)",
				s.requestHash)
		}
		var msg message.Message
		if err := json.Unmarshal(s.replay.Response, &msg); err != nil {
			return nil, fmt.Errorf("model: replay response: %w", err)
		}
		response := inference.GenerateResponse{
			Message:      msg,
			Usage:        s.replay.Usage,
			FinishReason: inference.FinishReason(s.replay.StopReason),
		}
		result, err := s.finish(response)
		if err != nil {
			return nil, err
		}
		s.result = result
		s.done = true
		if text := msg.Content.Text(); text != "" {
			return agent.Text{Text: text}, nil
		}
		return nil, io.EOF
	}
	for {
		event, err := s.stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			response, resultErr := s.stream.Result()
			if resultErr != nil {
				return nil, resultErr
			}
			s.result, err = s.finish(response)
			if err != nil {
				return nil, err
			}
			// One model call is one round: the discovery pool advances after the
			// call that used it.
			s.view.AdvanceRound()
			s.done = true
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		switch event.Kind {
		case inference.StreamText:
			return agent.Text{Text: event.Text}, nil
		case inference.StreamReasoning:
			return agent.Reasoning{Text: event.Text}, nil
		case inference.StreamUsage:
			if event.Usage != nil {
				return agent.UsageEvent{Usage: *event.Usage}, nil
			}
		}
	}
}

func (s *modelStream) Result() (graph.Result, error) {
	if !s.done {
		return graph.Result{}, fmt.Errorf("model: result before the stream ended")
	}
	return s.result, nil
}

func (s *modelStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return s.stream.Close()
}

// finish turns the assembled response into the step's writes.
func (s *modelStream) finish(response inference.GenerateResponse) (graph.Result, error) {
	// The transcript key must merge as append: this write is the delta of this
	// step, not the whole transcript.
	updates := []graph.StateUpdate{{
		Key:   s.messagesKey,
		Value: graph.MessagesValue(graph.MessagesFromSlice([]message.Message{response.Message})),
	}}
	if s.pendingKey != "" {
		calls := toolCallsOf(response.Message)
		if calls == nil {
			calls = []message.ToolCall{}
		}
		encoded, err := json.Marshal(calls)
		if err != nil {
			return graph.Result{}, fmt.Errorf("model: encode tool calls: %w", err)
		}
		updates = append(updates, graph.StateUpdate{Key: s.pendingKey, Value: graph.JSON(encoded)})
	}
	if s.usageKey != "" {
		encoded, err := json.Marshal(response.Usage)
		if err != nil {
			return graph.Result{}, fmt.Errorf("model: encode usage: %w", err)
		}
		updates = append(updates, graph.StateUpdate{Key: s.usageKey, Value: graph.JSON(encoded)})
	}
	if s.outputKey != "" {
		if text := response.Message.Content.Text(); text != "" {
			updates = append(updates, graph.StateUpdate{Key: s.outputKey, Value: graph.Text(text)})
		}
	}
	result := graph.Result{State: updates}
	if s.record && s.replay == nil {
		record := agent.LLMCallRecord{
			RequestHash: s.requestHash,
			Model:       s.model,
			Usage:       response.Usage,
			StopReason:  string(response.FinishReason),
			LatencyMs:   time.Since(s.started).Milliseconds(),
			Messages:    s.digests,
			Segments:    s.segments,
		}
		if body, err := json.Marshal(response.Message); err == nil && s.bodies {
			record.Response = body
		}
		if payload, err := json.Marshal(record); err == nil {
			// Dated at the call's start: the record describes an interval, and
			// LatencyMs is what says how long it was. A reader that wants the end
			// adds the two.
			result.Records = []graph.Record{{
				Kind:    recordLLMCall,
				Payload: payload,
				At:      s.started,
			}}
		}
	}
	return result, nil
}

// open starts the call through the router when the runtime declared models, and
// through the single provider otherwise.
func (m *modelKind) open(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	if router := m.rt.Router(); router != nil {
		return router.GenerateStream(ctx, request)
	}
	generator := m.rt.Provider().Generator
	if generator == nil {
		return nil, fmt.Errorf("model: the runtime declares no generator")
	}
	return generator.GenerateStream(ctx, request)
}

// inputRole narrows a turn's role to what a generation may be fed. A transcript
// that ends with an assistant or system turn is a graph bug, not a user turn.
func inputRole(role message.Role) (inference.InputRole, error) {
	switch role {
	case message.RoleUser:
		return inference.InputRoleUser, nil
	case message.RoleTool:
		return inference.InputRoleTool, nil
	}
	return "", fmt.Errorf(
		"model: the transcript ends with a %s turn; only a user or tool turn can be answered",
		role)
}

// toolCallsOf extracts the calls the model asked for.
func toolCallsOf(msg message.Message) []message.ToolCall {
	var calls []message.ToolCall
	for _, part := range msg.Content.Parts {
		if call, ok := part.(message.ToolCallPart); ok {
			calls = append(calls, call.Call)
		}
	}
	return calls
}
