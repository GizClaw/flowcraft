package nodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/outcome"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// projection is what a model node needs beyond show-and-call: it must be able to pin a tool the
// document names, and to advance the discovery round after the call that used it.
//
// It is defined here, at the consumer, so the projection package implements it without this package
// depending on that one. A view that cannot do either — a stub in a test — still works: the two
// methods are advisory, and a node that cannot pin simply offers whatever the projection shows.
type projection interface {
	tool.View
	// Require makes names visible for this round.
	Require(names ...string)
	// AdvanceRound expires what the round discovered.
	AdvanceRound()
}

// project narrows a view to the wider interface a model node needs, or nil.
func project(view tool.View) projection {
	p, _ := view.(projection)
	return p
}

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
	decoders map[string]inference.ExtensionDecoder
}

// DecodeConfig parses the node's config at Build, so a run does not decode it
// again for every call. The extension list is resolved here for the same reason:
// `provider`/`id`/`fields` is config, and its typed form is what a call needs.
func (m *modelKind) DecodeConfig(raw json.RawMessage) (any, error) {
	config, err := decodeModelConfig(raw)
	if err != nil {
		return nil, err
	}
	// A node built without decoders refuses an extension entry here, at Build,
	// instead of failing the first call that runs it.
	resolved, err := inference.DecodeExtensions(
		config.Request.Extensions, m.decoders, "request.extensions")
	if err != nil {
		return nil, err
	}
	config.Request.decoded = resolved
	return config, nil
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

// Model builds the model kind.
//
// It takes nothing: the session's authority, the tool surface and the model access arrive with each
// invocation, so one instance serves every session of a deployment.
func Model(opts ...ModelOption) graph.NodeKind {
	m := &modelKind{}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

func (*modelKind) Name() string { return "model" }

// FailureKind reports a model call as the turn's diagnosis: the input or the answer, not the
// step that carried it.
func (*modelKind) FailureKind() agent.FailureKind { return agent.FailureModel }

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
	// Stream says how the call reaches the provider: the incremental events a
	// spoken turn wants, or one finished response. Nil streams, so an existing
	// deployment keeps the live text a reader watches; a call whose answer is data
	// (a scheduler, a judge) sets false and pays for no deltas.
	Stream     *bool                      `json:"stream"`
	Model      *modelRef                  `json:"model"`
	Hint       string                     `json:"hint"`
	Output     string                     `json:"output"`
	System     string                     `json:"system"`
	Tools      *toolsConfig               `json:"tools"`
	Extensions []inference.ExtensionEntry `json:"extensions"`
	// decoded is the resolved form of Extensions. It is built when the node's
	// config is decoded, because the config is static: resolving it per call paid
	// a JSON parse for a value that cannot change between calls.
	decoded inference.Extensions `json:"-"`
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

// DeclaredModels reports the model this node names, or nothing when it leaves the choice to the
// deployment's default. It is what a host checks a session's authority against before a turn.
func (c modelConfig) DeclaredModels() []model.ModelRef {
	if c.Request.Model == nil {
		return nil
	}
	ref := model.ModelRef{
		ID:      model.ModelID{Provider: c.Request.Model.Provider, Name: c.Request.Model.Name},
		Profile: c.Request.Model.Profile,
	}
	if ref.ID.Name == "" {
		return nil
	}
	return []model.ModelRef{ref}
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
	view := viewOf(in)
	request, err := m.request(ctx, in, view, config.Request, transcript)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	record := in.Trace.AtLeast(telemetry.TraceNode) || in.Replay
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
			replay: &call,
			// The recorded identity is what this round answers as, so the
			// opening names the model the original round was answered by.
			metadata:    call.Metadata,
			view:        view,
			transcript:  transcript,
			messagesKey: config.Messages,
			pendingKey:  config.Pending,
			usageKey:    config.Usage,
			outputKey:   config.Output,
			model:       request.Model,
			requestHash: requestHash,
			step:        in.Step,
		}, nil
	}
	started := time.Now()
	var (
		stream   inference.Stream
		finished *inference.GenerateResponse
	)
	if config.Request.Stream != nil && !*config.Request.Stream {
		response, err := generate(ctx, in, request)
		if err != nil {
			return nil, err
		}
		finished = &response
	} else {
		stream, err = open(ctx, in, request)
		if err != nil {
			return nil, err
		}
	}
	return &modelStream{
		stream:      stream,
		finished:    finished,
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
		step:        in.Step,
	}, nil
}

// request maps the node's request block onto the inference contract.
func (m *modelKind) request(
	ctx context.Context,
	in graph.Invoke,
	view tool.View,
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
		Model:     modelRefOf(config.Model, in),
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
	extensions := config.decoded
	if extensions == nil && len(config.Extensions) > 0 {
		// The engine supplies the pre-decoded config; this path is the fallback
		// for a caller that hands the node a raw config (a test, or a host that
		// invokes the kind directly), and it must keep working.
		resolved, err := inference.DecodeExtensions(
			config.Extensions, m.decoders, "request.extensions")
		if err != nil {
			return inference.GenerateRequest{}, err
		}
		extensions = resolved
	}
	// Clone because a request owns its extensions: the decoded value is shared by
	// every call this node makes.
	request.Extensions = extensions.Clone()
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
func (m *modelKind) toolset(view tool.View, transcript graph.Messages, config *toolsConfig) (inference.Toolset, error) {
	if config != nil {
		if p := project(view); p != nil {
			if len(config.Names) > 0 {
				p.Require(config.Names...)
			}
			if len(config.Require) > 0 {
				p.Require(config.Require...)
			}
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

// viewOf is the session's projection of the tool table, as the invocation carries it.
//
// An unbound run has no view, and a model node then offers the model no tools at all: a node cannot
// show what the caller never held authority for.
func viewOf(in graph.Invoke) tool.View {
	if in.Binding.View == nil {
		return emptyView{}
	}
	return in.Binding.View
}

// emptyView is a projection with nothing in it, for a run that carries no binding.
type emptyView struct{}

func (emptyView) Definitions() []message.ToolDefinition { return nil }
func (emptyView) Visible(string) bool                   { return false }
func (emptyView) RecordCall(message.ToolCall)           {}

// modelRef resolves the configured model, or the runtime's default.
func modelRefOf(configured *modelRef, in graph.Invoke) model.ModelRef {
	if configured == nil {
		return in.DefaultModel
	}
	ref := model.ModelRef{
		ID:      model.ModelID{Provider: configured.Provider, Name: configured.Name},
		Profile: configured.Profile,
	}
	if ref.ID.Name == "" {
		return in.DefaultModel
	}
	return ref
}

// modelStream forwards the model's deltas while it runs.
// advanceRound expires what the round's discovery exposed. A view that cannot do it — a stub, or one
// session's projection replaced by a narrower implementation — keeps showing what it showed.
func (s *modelStream) advanceRound() {
	if p := project(s.view); p != nil {
		p.AdvanceRound()
	}
}

type modelStream struct {
	stream      inference.Stream
	view        tool.View
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
	// step is the run's step ordinal when this node was invoked, so the stream's
	// start event can name the round it opened.
	step int
	// metadata is what the provider said answered: the model it resolved, and its
	// own request and response ids. It arrives on the stream's start event when
	// the provider announces one and on the response otherwise, so the last value
	// wins.
	metadata outcome.Metadata
	// announced says the round's opening has been forwarded. It is emitted once
	// per call, before any delta, which is what a live consumer resets on.
	announced bool
	// pending holds a delta that arrived before the opening went out.
	pending agent.Event
	// replay answers from a recorded call instead of the provider.
	replay *agent.LLMCallRecord
	// finished is a call that arrived whole: a non-streaming request results in one
	// response, not a sequence of deltas.
	finished *inference.GenerateResponse
	// reportedUsage says the round's accounting already went out on the stream. A
	// provider that sends usage deltas reports it before the terminal event; one that
	// sends the whole response at the end never sends one at all, and the turn would
	// then believe the call cost nothing.
	reportedUsage bool
	// pendingUsage carries the accounting the response held when the stream did not
	// report it. It goes out after the answer, so a consumer sees text before totals.
	pendingUsage *outcome.Usage
	result       graph.Result
	done         bool
}

func (s *modelStream) Next(ctx context.Context) (agent.Event, error) {
	// A delta that arrived before the round opened waits here: the opening goes
	// first, and the stream's contract is one event per call.
	if s.pending != nil {
		event := s.pending
		s.pending = nil
		return event, nil
	}
	if s.pendingUsage != nil {
		usage := *s.pendingUsage
		s.pendingUsage = nil
		return s.report(agent.UsageEvent{Usage: usage}), nil
	}
	if s.done {
		return nil, io.EOF
	}
	if s.finished != nil {
		if !s.announced {
			return s.open(), nil
		}
		// A call asked for a finished response still advances the discovery pool:
		// it is a live call, it just did not arrive in pieces.
		s.advanceRound()
		return s.answered(*s.finished)
	}
	if s.replay != nil {
		if !s.announced {
			// A replayed round has no wire to wait on: it opens at once.
			return s.open(), nil
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
		return s.answered(inference.GenerateResponse{
			Message:      msg,
			Usage:        s.replay.Usage,
			FinishReason: inference.FinishReason(s.replay.StopReason),
			// A replayed round reports the identity the original one reported, so
			// a log cannot tell the two apart by what the answer claims to be.
			Metadata: s.replay.Metadata,
		})
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
			s.advanceRound()
			s.done = true
			s.queueUsage(response.Usage)
			// An answer with no text still opened a round; the opening is all a
			// caller gets for it.
			if !s.announced {
				return s.open(), nil
			}
			if s.pendingUsage != nil {
				return s.Next(ctx)
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		switch event.Kind {
		case inference.StreamStart:
			// The provider names the model it resolved here, before any delta:
			// keep it for the record and for the opening that follows.
			if event.Metadata != nil {
				s.metadata = *event.Metadata
			}
		case inference.StreamText:
			return s.report(agent.Text{Text: event.Text}), nil
		case inference.StreamReasoning:
			return s.report(agent.Reasoning{Text: event.Text}), nil
		case inference.StreamUsage:
			if event.Usage != nil {
				s.reportedUsage = true
				return s.report(agent.UsageEvent{Usage: *event.Usage}), nil
			}
		}
	}
}

// report returns a delta, preceded by the round's opening the first time.
//
// The opening is emitted here rather than before the loop because the model it
// names is only known once the provider has said it: an announcement made up
// front could only repeat the request's model, which is the one fact the
// resolved name exists to correct.
func (s *modelStream) report(event agent.Event) agent.Event {
	if s.announced {
		return event
	}
	s.pending = event
	return s.open()
}

// open renders the round's opening and marks it emitted.
//
// The event exists in the vocabulary and the engine already resets its streamed
// text on it (graph.Run.runStep); nothing produced it until now, so a live
// consumer could not tell which model a round was answered by.
func (s *modelStream) open() agent.Event {
	s.announced = true
	name := s.model.ID.Name
	if s.metadata.Model.Name != "" {
		name = s.metadata.Model.Name
	}
	return agent.StreamStart{Round: s.step + 1, Model: name}
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
		// The response's metadata is authoritative when it has one: a driver that
		// only learns the resolved model at the end of the stream reports it
		// there, not on the opening event.
		metadata := s.metadata
		if response.Metadata.Model.Name != "" || response.Metadata.ResponseID != "" {
			metadata = response.Metadata
		}
		record := agent.LLMCallRecord{
			RequestHash: s.requestHash,
			Model:       s.model,
			Metadata:    metadata,
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

// answer finishes a call that arrived whole: one response, one text event.
func (s *modelStream) answer(response inference.GenerateResponse) (agent.Event, error) {
	result, err := s.finish(response)
	if err != nil {
		return nil, err
	}
	s.result = result
	s.done = true
	s.queueUsage(response.Usage)
	if text := response.Message.Content.Text(); text != "" {
		return agent.Text{Text: text}, nil
	}
	return nil, io.EOF
}

// answered forwards a response that arrived whole, and makes sure its accounting is not lost.
//
// A stream reports usage in its own delta; a response that arrives whole has no such event,
// so the totals exist only in the response. Returning io.EOF here would end the round without
// ever telling the caller what it cost, which is what made a non-streaming provider look free
// in every aggregate downstream.
func (s *modelStream) answered(response inference.GenerateResponse) (agent.Event, error) {
	event, err := s.answer(response)
	switch {
	case errors.Is(err, io.EOF):
		// The answer carried no text; the accounting may still be news.
		if s.pendingUsage != nil {
			usage := *s.pendingUsage
			s.pendingUsage = nil
			return s.report(agent.UsageEvent{Usage: usage}), nil
		}
		return nil, io.EOF
	case err != nil:
		return nil, err
	}
	return event, nil
}

// queueUsage keeps the response's accounting for the next event when the stream did not
// report it itself. Text goes first: a consumer that shows totals before the answer would be
// reporting a call it has not finished reading.
func (s *modelStream) queueUsage(usage outcome.Usage) {
	if s.reportedUsage || !usageWorthReporting(usage) {
		return
	}
	s.pendingUsage = &usage
}

// usageWorthReporting reports whether a response carried accounting a caller can use.
func usageWorthReporting(usage outcome.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.Billing != nil || len(usage.Work) > 0
}

// generate performs one non-streaming call through the process's model access.
func generate(
	ctx context.Context,
	in graph.Invoke,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	generator := generatorOf(in)
	if generator == nil {
		return inference.GenerateResponse{}, fmt.Errorf("model: the deployment declares no generator")
	}
	return generator.Generate(ctx, request)
}

// open starts a streaming call through the process's model access.
func open(ctx context.Context, in graph.Invoke, request inference.GenerateRequest) (inference.Stream, error) {
	generator := generatorOf(in)
	if generator == nil {
		return nil, fmt.Errorf("model: the deployment declares no generator")
	}
	return generator.GenerateStream(ctx, request)
}

// generatorOf is the model access the invocation carries.
func generatorOf(in graph.Invoke) inference.Generator {
	if in.Models == nil {
		return nil
	}
	return in.Models.Generator
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
		// Either T or *T satisfies message.Part, and a driver that builds the pointer
		// is as valid as one that builds the value. A raw assertion on the value form
		// finds nothing for the first of them, and "the model asked for no tools" is
		// indistinguishable from "the model asked for tools this cannot see" — so the
		// part is normalized first.
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		if call, ok := normalized.(message.ToolCallPart); ok {
			calls = append(calls, call.Call)
		}
	}
	return calls
}
