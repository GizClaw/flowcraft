package memory

import "github.com/GizClaw/flowcraft/sdk/inference"

// Record is one entry in a transcript. It wraps the canonical
// inference.Message with a runtime-assigned Seq and a caller-stable
// ID so transcripts can implement (Seq, ID) last-write-wins, idempotent
// retry, and opaque cursor pagination.
//
// Callers that only care about messages can iterate Records and
// pull .Message; callers that need to page or checkpoint carry
// .Seq and .ID alongside the message.
type Record struct {
	// ID is the caller's stable identifier for the record. The
	// runtime treats (Seq, ID) as the (Seq, ID) last-write-wins
	// pair: re-appending a Record whose ID was already accepted
	// is a no-op that still returns the original Seq. An empty
	// ID means "the runtime should assign one" — the runtime
	// back-fills ID with a unique value before persisting.
	ID string
	// Seq is the runtime-assigned monotonic sequence number
	// within (Scope.HardPartitionKey, ConversationID). Callers
	// leave it zero on Append; the runtime fills it in and
	// returns the final value in AppendResponse.LastSeq. Load
	// returns the Seq alongside every Record so callers can
	// pass it back as the next Load's Cursor.
	Seq uint64
	// Message is the canonical message content, reused from
	// sdk/inference so the kernel does not own a second
	// content union.
	Message inference.Message
}

// Hit is one result returned by Recall. The shape is intentionally
// minimal and only references kernel-defined types: Parts reuses
// the inference Part union so that downstream prompt builders and
// inspectors do not need a second content model.
type Hit struct {
	// ID is stable for the lifetime of the underlying record.
	// Two recalls returning the same ID refer to the same
	// physical entry.
	ID string
	// Parts is the content of the hit. Reuses inference.Part so
	// callers can render hits through the same machinery they
	// use for messages.
	Parts []inference.Part
	// Score is the relevance score in the implementation's
	// native range. Callers that want a normalized score use
	// RecallRequest.MinScore to define a comparable bound.
	Score float64
	// Source is an opaque locator that names where the hit
	// came from. The kernel does not enumerate its values;
	// implementations write what makes sense for their storage
	// model ("transcript:abc/seq-42", "chunk:docs/foo.md#3"),
	// and the agent decides how to render it. Switch / prefix
	// matching on Source is the documented usage.
	Source string
	// Metadata is opaque key/value annotations from the
	// implementation. Callers should treat it as advisory and
	// not depend on the presence of any key.
	Metadata map[string]string
}

// ChunkPolicy controls how Import splits a source document into
// chunks. The fields here are the ones a tool factory decides
// per call; the embedder binding and tokenizer implementation are
// configured in the runtime spec, not on every Import call.
type ChunkPolicy struct {
	// Target is the desired number of tokens per chunk.
	Target int `json:"target,omitempty" yaml:"target,omitempty"`
	// MinChunkSize is the lower bound. Chunks smaller than this
	// are merged into their neighbour.
	MinChunkSize int `json:"min_chunk_size,omitempty" yaml:"min_chunk_size,omitempty"`
	// MaxChunkSize is the upper bound. Chunks larger than this
	// are split further, even if it crosses a logical
	// boundary.
	MaxChunkSize int `json:"max_chunk_size,omitempty" yaml:"max_chunk_size,omitempty"`
	// Tokenizer names the tokenizer that defines the unit
	// "token" for Target / MinChunkSize / MaxChunkSize. The
	// value is opaque to the kernel; impls map it to a
	// concrete tokenizer.
	Tokenizer string `json:"tokenizer,omitempty" yaml:"tokenizer,omitempty"`
	// Overlap is the number of tokens shared between adjacent
	// chunks. 0 means no overlap.
	Overlap int `json:"overlap,omitempty" yaml:"overlap,omitempty"`
	// Splitter is the chunking strategy. "fixed",
	// "sentence", "paragraph" are documented values; the
	// kernel does not enumerate the set.
	Splitter string `json:"splitter,omitempty" yaml:"splitter,omitempty"`
	// RespectCode prevents the splitter from cutting inside a
	// fenced code block when true.
	RespectCode bool `json:"respect_code,omitempty" yaml:"respect_code,omitempty"`
}

// IsZero reports whether the ChunkPolicy is the zero value. A zero
// ChunkPolicy means "use the runtime's default"; callers usually
// leave it zero in the common case.
func (p ChunkPolicy) IsZero() bool {
	return p == ChunkPolicy{}
}
