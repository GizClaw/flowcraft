package graph

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// Kind names the type a state slot holds.
//
// The set is deliberately small: a slot holds text, structured data, a
// session, a boolean or one media payload. A node that needs more can
// encode it as JSON.
type Kind string

const (
	// KindText is a string.
	KindText Kind = "text"
	// KindJSON is a JSON document.
	KindJSON Kind = "json"
	// KindMessages is a session, carried as an immutable sequence.
	KindMessages Kind = "messages"
	// KindBool is a boolean.
	KindBool Kind = "bool"
	// KindImage is an image: a media type plus inline bytes or a blob reference.
	KindImage Kind = "image"
	// KindAudio is audio, carried the same way as an image.
	KindAudio Kind = "audio"
	// KindFile is an opaque file, carried the same way as an image.
	KindFile Kind = "file"
	// KindVideo is video, carried the same way as an image.
	KindVideo Kind = "video"
)

// Valid reports whether k is one of the declared kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindText, KindJSON, KindMessages, KindBool,
		KindImage, KindAudio, KindFile, KindVideo:
		return true
	}
	return false
}

// Value is an immutable value stored in the graph's state.
//
// The representation is unexported and every constructor copies what it is given:
// text is a string, JSON is stored as a string, media bytes are copied or replaced
// by a blob reference. A value therefore cannot change after it is produced, which
// is what makes sharing it between concurrent nodes safe without a copy.
//
// Sessions follow the same rule one level down: message parts are immutable
// by construction (a media source keeps its bytes unexported and copies them on
// read; tool-call arguments are an opaque type that copies on construction), so
// appending a message shares its parts instead of cloning them.
type Value struct {
	kind Kind
	text string // KindText, the encoded form of KindJSON, and inline media bytes
	flag bool   // KindBool
	msgs *Messages
	// mediaType and blob belong to the media kinds. Inline bytes and a blob
	// reference are mutually exclusive: a host that has a store passes the
	// reference, one that does not passes the bytes.
	mediaType string
	blob      string
}

// Text returns a text value.
func Text(s string) Value { return Value{kind: KindText, text: s} }

// JSON returns a JSON value. The bytes are copied, so mutating b afterwards does
// not change the value.
func JSON(b []byte) Value { return Value{kind: KindJSON, text: string(b)} }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{kind: KindBool, flag: b} }

// MessagesValue returns a session value.
func MessagesValue(m Messages) Value { return Value{kind: KindMessages, msgs: &m} }

// Image returns an image value holding inline bytes. The bytes are copied, so
// mutating b afterwards does not change the value.
func Image(mediaType string, b []byte) Value { return inlineMedia(KindImage, mediaType, b) }

// Audio returns an audio value holding inline bytes.
func Audio(mediaType string, b []byte) Value { return inlineMedia(KindAudio, mediaType, b) }

// File returns a file value holding inline bytes.
func File(mediaType string, b []byte) Value { return inlineMedia(KindFile, mediaType, b) }

// Video returns a video value holding inline bytes.
func Video(mediaType string, b []byte) Value { return inlineMedia(KindVideo, mediaType, b) }

// ImageRef returns an image that names a stored blob instead of carrying it.
func ImageRef(mediaType, blob string) Value { return mediaRef(KindImage, mediaType, blob) }

// AudioRef returns audio that names a stored blob.
func AudioRef(mediaType, blob string) Value { return mediaRef(KindAudio, mediaType, blob) }

// FileRef returns a file that names a stored blob.
func FileRef(mediaType, blob string) Value { return mediaRef(KindFile, mediaType, blob) }

// VideoRef returns video that names a stored blob.
func VideoRef(mediaType, blob string) Value { return mediaRef(KindVideo, mediaType, blob) }

func mediaRef(kind Kind, mediaType, blob string) Value {
	return Value{kind: kind, mediaType: mediaType, blob: blob}
}

func inlineMedia(kind Kind, mediaType string, b []byte) Value {
	return Value{kind: kind, mediaType: mediaType, text: string(b)}
}

// Media is a media value's payload: inline bytes or a blob reference.
type Media struct {
	MediaType string
	// Data is nil when Blob is set.
	Data []byte
	Blob string
}

// Media returns a media value's payload.
func (v Value) Media() (Media, error) {
	switch v.kind {
	case KindImage, KindAudio, KindFile, KindVideo:
	default:
		return Media{}, fmt.Errorf("graph: value is %q, not media", v.kind)
	}
	media := Media{MediaType: v.mediaType, Blob: v.blob}
	if v.blob == "" {
		media.Data = []byte(v.text)
	}
	return media, nil
}

// Kind reports the value's kind.
func (v Value) Kind() Kind { return v.kind }

// IsZero reports whether the value was never set.
func (v Value) IsZero() bool { return v.kind == "" }

// Text returns the string of a text value.
func (v Value) Text() (string, error) { return v.decodeText(KindText) }

// JSON returns the JSON document of a JSON value.
func (v Value) JSON() (json.RawMessage, error) {
	text, err := v.decodeText(KindJSON)
	return json.RawMessage(text), err
}

// Bool returns the boolean of a boolean value.
func (v Value) Bool() (bool, error) {
	if v.kind != KindBool {
		return false, fmt.Errorf("graph: value is %q, not bool", v.kind)
	}
	return v.flag, nil
}

// Messages returns the session of a messages value.
func (v Value) Messages() (Messages, error) {
	if v.kind != KindMessages || v.msgs == nil {
		return Messages{}, fmt.Errorf("graph: value is %q, not messages", v.kind)
	}
	return *v.msgs, nil
}

func (v Value) decodeText(want Kind) (string, error) {
	if v.kind != want {
		return "", fmt.Errorf("graph: value is %q, not %s", v.kind, want)
	}
	return v.text, nil
}

// messagesChunkSize is how many messages a chunk holds. Chunks are the unit
// Slice copies and the point where the tail is folded into the chain: a small
// chunk keeps the fold rare, a large one keeps the walk short.
const messagesChunkSize = 64

// messageChunk is an immutable run of messages. Every chunk in the chain
// holds exactly messagesChunkSize messages, and parent points at the older one.
type messageChunk struct {
	parent   *messageChunk
	messages [messagesChunkSize]message.Message
}

// messageTail is the newest, still short end of a session: a linked list of at
// most messagesChunkSize-1 messages, newest first, waiting to be folded into a
// chunk. n is the length of the list that starts here.
type messageTail struct {
	parent *messageTail
	msg    message.Message
	n      int
}

// Messages is an immutable session with O(1) append.
//
// A message list is the value a fan-out would otherwise copy once per branch:
// a thousand messages is forty kilobytes of headers before any part is touched.
// As a persistent sequence, Append allocates one tail node and shares every
// chunk and node before it, so two branches append independently without
// copying the shared prefix and without a lock. Each messagesChunkSize appends
// the tail is folded into one immutable chunk, which is what makes Slice a
// per-chunk copy instead of a walk over every message.
type Messages struct {
	chunks *messageChunk
	tail   *messageTail
	n      int
}

// EmptyMessages returns an empty session.
func EmptyMessages() Messages { return Messages{} }

// MessagesFromSlice builds a session from a slice. It copies the headers, so
// later appends to the caller's slice do not change this value; the messages
// themselves are shared, which is safe because a message does not change after it
// is built.
//
// A restored transcript is built with one allocation for its chunks and one for
// its tail, rather than one node per message.
func MessagesFromSlice(msgs []message.Message) Messages {
	n := len(msgs)
	if n == 0 {
		return Messages{}
	}
	full := n / messagesChunkSize
	tailLen := n % messagesChunkSize

	var chunks *messageChunk
	if full > 0 {
		nodes := make([]messageChunk, full)
		for i := range nodes {
			copy(nodes[i].messages[:], msgs[i*messagesChunkSize:(i+1)*messagesChunkSize])
			if i > 0 {
				nodes[i].parent = &nodes[i-1]
			}
		}
		chunks = &nodes[full-1]
	}

	var tail *messageTail
	if tailLen > 0 {
		nodes := make([]messageTail, tailLen)
		start := full * messagesChunkSize
		for i := range nodes {
			nodes[i].n = i + 1
			nodes[i].msg = msgs[start+i]
			if i > 0 {
				nodes[i].parent = &nodes[i-1]
			}
		}
		tail = &nodes[tailLen-1]
	}
	return Messages{chunks: chunks, tail: tail, n: n}
}

// Append returns a new session with msg at the end. The receiver is
// unchanged, and the two share every message before msg.
func (m Messages) Append(msg message.Message) Messages {
	tail := &messageTail{parent: m.tail, msg: msg, n: 1}
	if m.tail != nil {
		tail.n = m.tail.n + 1
	}
	if tail.n < messagesChunkSize {
		return Messages{chunks: m.chunks, tail: tail, n: m.n + 1}
	}
	return m.fold(tail)
}

// fold moves a full tail into the chunk chain and returns the session that
// starts a fresh tail. The tail's messages are copied once, in chunk order.
func (m Messages) fold(tail *messageTail) Messages {
	chunk := &messageChunk{parent: m.chunks}
	for node, i := tail, messagesChunkSize-1; node != nil; node, i = node.parent, i-1 {
		chunk.messages[i] = node.msg
	}
	return Messages{chunks: chunk, n: m.n + 1}
}

// Len reports how many messages the session holds.
func (m Messages) Len() int { return m.n }

// Slice materializes the session. It is the one O(n) operation, paid only
// where a node actually needs the list (a model request, for example): one
// allocation, one copy per chunk and one step per tail message.
func (m Messages) Slice() []message.Message {
	out := make([]message.Message, m.n)
	at := m.n
	for node := m.tail; node != nil; node = node.parent {
		at--
		out[at] = node.msg
	}
	for chunk := m.chunks; chunk != nil; chunk = chunk.parent {
		at -= messagesChunkSize
		copy(out[at:], chunk.messages[:])
	}
	return out
}

// Concat returns a new session holding this one's messages followed by
// other's. It walks other, not the shared prefix, so merging two branches costs
// what the branches appended.
func (m Messages) Concat(other Messages) Messages {
	out := m
	for _, msg := range other.Slice() {
		out = out.Append(msg)
	}
	return out
}

// Each walks the session newest first without materializing it. The
// callback returns false to stop. A caller that needs oldest-first order can
// collect and reverse what it keeps, which is cheaper than materializing a
// slice of every message.
func (m Messages) Each(fn func(message.Message) bool) {
	for node := m.tail; node != nil; node = node.parent {
		if !fn(node.msg) {
			return
		}
	}
	for chunk := m.chunks; chunk != nil; chunk = chunk.parent {
		for i := messagesChunkSize - 1; i >= 0; i-- {
			if !fn(chunk.messages[i]) {
				return
			}
		}
	}
}
