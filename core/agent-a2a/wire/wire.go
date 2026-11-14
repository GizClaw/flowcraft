// Package wire converts between the core message model and the A2A wire types.
//
// It exists because both halves of the protocol need both directions: the client
// renders a message for the peer and reads the peer's messages back, and the
// server does the same the other way round. One implementation keeps a mapping
// decision (a media part, a metadata key) from being made twice and diverging.
package wire

import (
	"encoding/gob"
	"mime"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/goccy/go-json"
)

func init() {
	// A data part's value is an interface, and the SDK's in-memory task store
	// gob-encodes tasks: the concrete map an artifact reference falls back to
	// must be registered before it can cross.
	gob.Register(map[string]string{})
}

// Message renders one core message for the wire, metadata included.
func Message(msg message.Message) (*a2a.Message, error) {
	parts, err := Parts(msg.Content.Parts)
	if err != nil {
		return nil, err
	}
	wire := &a2a.Message{
		ID:    a2a.NewMessageID(),
		Role:  roleOf(msg.Role),
		Parts: parts,
	}
	metadata := msg.Metadata
	if metadata == nil {
		return wire, nil
	}
	for _, task := range metadata.ReferenceTasks {
		wire.ReferenceTasks = append(wire.ReferenceTasks, a2a.TaskID(task))
	}
	wire.Extensions = append([]string(nil), metadata.Extensions...)
	for key, raw := range metadata.Attributes {
		if index, ok := PartAttributeIndex(key); ok {
			if index < len(parts) {
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, errdefs.Validationf("a2a: part %d metadata: %w", index, err)
				}
				parts[index].Metadata = value
			}
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errdefs.Validationf("a2a: metadata %q: %w", key, err)
		}
		if wire.Metadata == nil {
			wire.Metadata = make(map[string]any, len(metadata.Attributes))
		}
		wire.Metadata[key] = value
	}
	return wire, nil
}

// FromMessage converts an incoming wire message into the core shape.
func FromMessage(msg *a2a.Message) (message.Message, error) {
	role := message.RoleAssistant
	if msg.Role == a2a.MessageRoleUser {
		role = message.RoleUser
	}
	parts := make([]message.Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		converted, err := FromPart(part)
		if err != nil {
			return message.Message{}, err
		}
		parts = append(parts, converted)
	}
	metadata, err := metadataOf(msg)
	if err != nil {
		return message.Message{}, err
	}
	return message.Message{Role: role, Content: message.Content{Parts: parts}, Metadata: metadata}, nil
}

// Parts renders core parts for the wire.
func Parts(parts []message.Part) ([]*a2a.Part, error) {
	out := make([]*a2a.Part, 0, len(parts))
	for _, part := range parts {
		converted, err := Part(part)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

// Part renders one core part for the wire.
func Part(part message.Part) (*a2a.Part, error) {
	switch typed := part.(type) {
	case message.TextPart:
		return a2a.NewTextPart(typed.Text), nil
	case message.DataPart:
		return a2a.NewDataPart(typed.Value), nil
	case message.ImagePart:
		return sourcePart(typed.Source.Kind(), typed.Source.URL(), typed.Source.Bytes(), typed.Source.MediaType(), ""), nil
	case message.AudioPart:
		return sourcePart(typed.Source.Kind(), typed.Source.URL(), typed.Source.Bytes(), typed.Source.MediaType(), ""), nil
	case message.VideoPart:
		return sourcePart(typed.Source.Kind(), typed.Source.URL(), typed.Source.Bytes(), typed.Source.MediaType(), ""), nil
	case message.FilePart:
		return sourcePart(typed.Source.Kind(), typed.Source.URL(), typed.Source.Bytes(), typed.Source.MediaType(), typed.Name), nil
	default:
		return nil, errdefs.Validationf("a2a: part kind %q has no wire form", part.Kind())
	}
}

// FromPart converts one wire part. A content type this adapter cannot carry is
// an error rather than a dropped part: a client cannot tell an ignored
// attachment from one that was never sent.
func FromPart(part *a2a.Part) (message.Part, error) {
	if part == nil {
		return nil, errdefs.Validationf("a2a: nil part")
	}
	switch content := part.Content.(type) {
	case a2a.Text:
		return message.TextPart{Text: string(content)}, nil
	case a2a.URL:
		source, err := media.NewFileURL(string(content), part.MediaType)
		if err != nil {
			return nil, errdefs.Validationf("a2a: file part: %w", err)
		}
		return message.FilePart{Source: source, Name: part.Filename}, nil
	case a2a.Raw:
		return fromRaw(content, part.MediaType, part.Filename)
	case a2a.Data:
		body, err := json.Marshal(content)
		if err != nil {
			return nil, errdefs.Validationf("a2a: encode data part: %w", err)
		}
		return message.DataPart{MediaType: part.MediaType, Value: body}, nil
	default:
		return nil, errdefs.Validationf(
			"a2a: part content %T has no core representation", part.Content)
	}
}

// Text concatenates the text parts of a wire message.
func Text(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	text := ""
	for _, part := range msg.Parts {
		if typed, ok := part.Content.(a2a.Text); ok {
			text += string(typed)
		}
	}
	return text
}

// State translates the internal lifecycle into the protocol's spelling. The
// internal names are the protocol's words in lower case; the SDK uses the
// proto-style upper-case constants.
func State(state agent.TaskState) a2a.TaskState {
	switch state {
	case agent.StateSubmitted:
		return a2a.TaskStateSubmitted
	case agent.StateWorking:
		return a2a.TaskStateWorking
	case agent.StateInputRequired:
		return a2a.TaskStateInputRequired
	case agent.StateCompleted:
		return a2a.TaskStateCompleted
	case agent.StateCanceled:
		return a2a.TaskStateCanceled
	case agent.StateRejected:
		return a2a.TaskStateRejected
	case agent.StateAuthRequired:
		return a2a.TaskStateAuthRequired
	default:
		return a2a.TaskStateFailed
	}
}

// FromState translates the protocol's spelling into the internal lifecycle.
func FromState(state a2a.TaskState) agent.TaskState {
	switch state {
	case a2a.TaskStateSubmitted:
		return agent.StateSubmitted
	case a2a.TaskStateWorking:
		return agent.StateWorking
	case a2a.TaskStateInputRequired:
		return agent.StateInputRequired
	case a2a.TaskStateCompleted:
		return agent.StateCompleted
	case a2a.TaskStateCanceled:
		return agent.StateCanceled
	case a2a.TaskStateRejected:
		return agent.StateRejected
	case a2a.TaskStateAuthRequired:
		return agent.StateAuthRequired
	default:
		return agent.StateFailed
	}
}

// PartAttributeKey is where a part's own metadata is kept on the message.
func PartAttributeKey(index int) string { return "part." + strconv.Itoa(index) }

// PartAttributeIndex reads that key back.
func PartAttributeIndex(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "part.")
	if !ok {
		return 0, false
	}
	index, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return index, true
}

func roleOf(role message.Role) a2a.MessageRole {
	if role == message.RoleUser {
		return a2a.MessageRoleUser
	}
	return a2a.MessageRoleAgent
}

// sourcePart renders one media/file source: inline bytes go as raw, a URL goes
// as a file URL part.
func sourcePart(kind media.SourceKind, url string, data []byte, mediaType, name string) *a2a.Part {
	if kind == media.SourceInline {
		return &a2a.Part{Content: a2a.Raw(data), MediaType: mediaType, Filename: name}
	}
	return &a2a.Part{Content: a2a.URL(url), MediaType: mediaType, Filename: name}
}

// fromRaw carries inline bytes into the core part that matches them: media by
// prefix, text as text, anything else as a file.
func fromRaw(data a2a.Raw, mediaType, name string) (message.Part, error) {
	raw := []byte(data)
	base, _, _ := mime.ParseMediaType(mediaType)
	switch {
	case strings.HasPrefix(base, "image/"):
		source, err := media.NewImageBytes(raw, mediaType)
		if err != nil {
			return nil, errdefs.Validationf("a2a: image part: %w", err)
		}
		return message.ImagePart{Source: source}, nil
	case strings.HasPrefix(base, "audio/"):
		source, err := media.NewAudioBytes(raw, mediaType)
		if err != nil {
			return nil, errdefs.Validationf("a2a: audio part: %w", err)
		}
		return message.AudioPart{Source: source}, nil
	case strings.HasPrefix(base, "video/"):
		source, err := media.NewVideoBytes(raw, mediaType)
		if err != nil {
			return nil, errdefs.Validationf("a2a: video part: %w", err)
		}
		return message.VideoPart{Source: source}, nil
	case strings.HasPrefix(base, "text/"):
		return message.TextPart{Text: string(raw)}, nil
	}
	source, err := media.NewFileBytes(raw, mediaType)
	if err != nil {
		return nil, errdefs.Validationf("a2a: inline part: %w", err)
	}
	return message.FilePart{Source: source, Name: name}, nil
}

// metadataOf carries the transport's annotations into the core message: the
// cited tasks, the extensions, the free-form metadata, and each part's own
// metadata keyed by its position so the association survives.
func metadataOf(msg *a2a.Message) (*message.Metadata, error) {
	if msg == nil {
		return nil, nil
	}
	metadata := &message.Metadata{Extensions: append([]string(nil), msg.Extensions...)}
	for _, task := range msg.ReferenceTasks {
		metadata.ReferenceTasks = append(metadata.ReferenceTasks, string(task))
	}
	attributes := make(map[string]json.RawMessage, len(msg.Metadata))
	for key, value := range msg.Metadata {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, errdefs.Validationf("a2a: encode metadata %q: %w", key, err)
		}
		attributes[key] = raw
	}
	for index, part := range msg.Parts {
		if part == nil || len(part.Metadata) == 0 {
			continue
		}
		raw, err := json.Marshal(part.Metadata)
		if err != nil {
			return nil, errdefs.Validationf("a2a: encode part %d metadata: %w", index, err)
		}
		attributes[PartAttributeKey(index)] = raw
	}
	if len(attributes) > 0 {
		metadata.Attributes = attributes
	}
	if metadata.Empty() {
		return nil, nil
	}
	return metadata, nil
}
