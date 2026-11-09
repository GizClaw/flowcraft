package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// accumulator assembles a stream's events into the response a unary call would
// have returned. It lives in the pipeline rather than in every driver: turning
// deltas into parts is provider-neutral work, and a driver that reimplemented it
// would be reimplementing the same assembly and drifting from it.
//
// Events are keyed by content part index, which is the single index space
// [inference.StreamEvent.Index] defines. Parts come out ordered by that index.
type accumulator struct {
	text      map[int]*strings.Builder
	reasoning map[int]*reasoningBlock
	tools     map[int]*toolCallBlock
	delivered map[int]message.Part
	chunks    map[int]*chunkBuild

	usage    usage.Usage
	metadata inference.Metadata
	finish   inference.FinishReason
	done     bool
}

type reasoningBlock struct {
	text      strings.Builder
	signature string
	id        string
	source    string
}

// chunkBuild accumulates the bytes of one streamed media part. The template is the
// first chunk, which carries the media type and format the finished part needs.
type chunkBuild struct {
	template message.Part
	data     []byte
}

type toolCallBlock struct {
	id   string
	name string
	args strings.Builder
}

func newAccumulator() *accumulator {
	return &accumulator{
		text:      make(map[int]*strings.Builder),
		reasoning: make(map[int]*reasoningBlock),
		tools:     make(map[int]*toolCallBlock),
		delivered: make(map[int]message.Part),
		chunks:    make(map[int]*chunkBuild),
	}
}

// add consumes one decoded batch.
func (a *accumulator) add(events []inference.StreamEvent) {
	for _, event := range events {
		switch event.Kind {
		case inference.StreamStart:
			if event.Metadata != nil {
				a.metadata = *event.Metadata
			}
		case inference.StreamText:
			a.textBuilder(event.Index).WriteString(event.Text)
		case inference.StreamReasoning:
			block := a.reasoningBlock(event.Index)
			block.text.WriteString(event.Text)
			if event.Signature != "" {
				block.signature = event.Signature
			}
			if event.ID != "" {
				block.id = event.ID
			}
			if event.Source != "" {
				block.source = event.Source
			}
		case inference.StreamToolCall:
			block := a.toolBlock(event.Index)
			if event.Tool.ID != "" {
				block.id = event.Tool.ID
			}
			if event.Tool.Name != "" {
				block.name = event.Tool.Name
			}
			block.args.WriteString(event.Tool.Arguments)
		case inference.StreamPart:
			a.delivered[event.Index] = event.Part
		case inference.StreamChunk:
			build := a.chunkBuild(event.Index)
			if build.template == nil {
				build.template = event.Part
			}
			build.data = append(build.data, partBytes(event.Part)...)
		case inference.StreamUsage:
			a.usage = a.usage.Add(*event.Usage)
		case inference.StreamEnd:
			a.finish = event.Finish
			a.done = true
		}
	}
}

// finished reports whether a terminal event was seen.
func (a *accumulator) finished() bool { return a.done }

// response builds the canonical response. The caller must have seen a terminal
// event; assembly of a half-finished stream is meaningless and reported as
// truncation by the pipeline rather than built here.
func (a *accumulator) response() (inference.GenerateResponse, error) {
	indexes := make([]int, 0,
		len(a.delivered)+len(a.text)+len(a.reasoning)+len(a.tools)+len(a.chunks))
	seen := make(map[int]struct{}, cap(indexes))
	collect := func(index int) {
		if _, ok := seen[index]; ok {
			return
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	for index := range a.delivered {
		collect(index)
	}
	for index := range a.text {
		collect(index)
	}
	for index := range a.reasoning {
		collect(index)
	}
	for index := range a.tools {
		collect(index)
	}
	for index := range a.chunks {
		collect(index)
	}
	sort.Ints(indexes)

	parts := make([]message.Part, 0, len(indexes))
	for _, index := range indexes {
		if part, ok := a.delivered[index]; ok {
			parts = append(parts, part)
			continue
		}
		if builder, ok := a.text[index]; ok {
			parts = append(parts, message.TextPart{Text: builder.String()})
			continue
		}
		if block, ok := a.reasoning[index]; ok {
			parts = append(parts, message.ReasoningPart{
				Text:      block.text.String(),
				Signature: block.signature,
				ID:        block.id,
				Source:    block.source,
			})
			continue
		}
		if block, ok := a.tools[index]; ok {
			parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
				ID:        block.id,
				Name:      block.name,
				Arguments: message.NewArguments(toolArguments(block.args.String())),
			}})
			continue
		}
		if build, ok := a.chunks[index]; ok {
			part, err := build.part()
			if err != nil {
				return inference.GenerateResponse{}, err
			}
			parts = append(parts, part)
		}
	}

	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: a.finish,
		Usage:        a.usage,
		Metadata:     a.metadata,
	}, nil
}

// part rebuilds the finished media part from the template and the accumulated
// bytes, keeping the template's own fields.
func (b *chunkBuild) part() (message.Part, error) {
	data := b.data
	switch template := b.template.(type) {
	case message.AudioPart:
		source, err := media.NewAudioBytes(data, template.Source.MediaType())
		if err != nil {
			return nil, err
		}
		template.Source = source
		return template, nil
	case message.VideoPart:
		source, err := media.NewVideoBytes(data, template.Source.MediaType())
		if err != nil {
			return nil, err
		}
		template.Source = source
		return template, nil
	case message.ImagePart:
		source, err := media.NewImageBytes(data, template.Source.MediaType())
		if err != nil {
			return nil, err
		}
		template.Source = source
		return template, nil
	default:
		return nil, fmt.Errorf("openai: cannot rebuild a chunked %q part", b.template.Kind())
	}
}

// partBytes is the payload a chunked part contributes.
func partBytes(part message.Part) []byte {
	switch typed := part.(type) {
	case message.AudioPart:
		return typed.Source.Bytes()
	case message.VideoPart:
		return typed.Source.Bytes()
	case message.ImagePart:
		return typed.Source.Bytes()
	default:
		return nil
	}
}

// chunkBuild returns the accumulator for one chunked index.
func (a *accumulator) chunkBuild(index int) *chunkBuild {
	build, ok := a.chunks[index]
	if !ok {
		build = &chunkBuild{}
		a.chunks[index] = build
	}
	return build
}

func (a *accumulator) textBuilder(index int) *strings.Builder {
	builder, ok := a.text[index]
	if !ok {
		builder = &strings.Builder{}
		a.text[index] = builder
	}
	return builder
}

func (a *accumulator) reasoningBlock(index int) *reasoningBlock {
	block, ok := a.reasoning[index]
	if !ok {
		block = &reasoningBlock{}
		a.reasoning[index] = block
	}
	return block
}

func (a *accumulator) toolBlock(index int) *toolCallBlock {
	block, ok := a.tools[index]
	if !ok {
		block = &toolCallBlock{}
		a.tools[index] = block
	}
	return block
}

// toolArguments normalizes accumulated fragments into a JSON object. A call that
// carried no arguments at all is the common case and means "empty object", not
// "invalid JSON".
func toolArguments(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}
