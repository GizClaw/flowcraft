package recall

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// MessageBuffer maintains a per-Scope ring buffer of recent conversation
// messages so the extractor can resolve pronouns, anaphora, and entity
// references that were established in earlier turns but not yet
// promoted into a stored fact.
//
// Without this buffer, the extractor sees only the messages of the
// current Save call. In multi-batch ingestion (chat applications,
// session-batched eval pipelines, streaming transcripts) that means
// every cross-batch reference — "the gym I mentioned", "she came
// too", "yesterday's call" — is unresolvable, and the resulting
// fact body falls back to a vague paraphrase that no downstream
// retrieval can ground.
//
// Save reads up to K previous messages BEFORE extraction (handed to
// the extractor via [WithRecentMessages]) and appends the current
// batch AFTER persistence completes. Append is therefore never
// circular within a single Save: the messages a Save sees in Recent
// are strictly the messages of PRIOR Saves on the same scope.
//
// Implementations must be safe for concurrent use; Recent and Append
// may be invoked from different goroutines for the same scope.
//
// The default in-memory implementation ([NewMemoryBuffer]) is volatile
// and intended for single-process workloads. Production deployments
// that survive process restarts should provide a persistent backend
// (Redis ring, SQLite circular log, etc.) implementing this
// interface and inject it via [WithMessageBuffer].
type MessageBuffer interface {
	// Recent returns the most recent up-to-k messages stored for the
	// given scope, in chronological order (oldest first). An empty
	// scope must return an empty slice, not an error.
	Recent(ctx context.Context, scope Scope, k int) ([]llm.Message, error)

	// Append adds msgs to the tail of the scope's buffer. If the
	// buffer is bounded and the append would exceed capacity, the
	// oldest entries are evicted to make room.
	Append(ctx context.Context, scope Scope, msgs []llm.Message) error
}

// NewMemoryBuffer returns an in-process MessageBuffer that keeps at
// most cap messages per scope. The buffer is volatile — it lives only
// for the lifetime of the parent process — and intended for
// single-process deployments and tests. cap must be positive;
// non-positive values default to 64.
func NewMemoryBuffer(cap int) MessageBuffer {
	if cap <= 0 {
		cap = 64
	}
	return &memBuffer{cap: cap, scopes: make(map[string][]llm.Message)}
}

type memBuffer struct {
	cap int
	mu  sync.Mutex
	// scopes holds at most `cap` messages per canonical scope key.
	// The slice is treated as a ring; appends evict from the head
	// (oldest end) when the capacity is exceeded so Recent always
	// reads the freshest K from the tail.
	scopes map[string][]llm.Message
}

func (b *memBuffer) Recent(_ context.Context, scope Scope, k int) ([]llm.Message, error) {
	if k <= 0 {
		return nil, nil
	}
	key := messageBufferKey(scope)
	if key == "" {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := b.scopes[key]
	if len(buf) == 0 {
		return nil, nil
	}
	if k >= len(buf) {
		out := make([]llm.Message, len(buf))
		copy(out, buf)
		return out, nil
	}
	out := make([]llm.Message, k)
	copy(out, buf[len(buf)-k:])
	return out, nil
}

func (b *memBuffer) Append(_ context.Context, scope Scope, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	key := messageBufferKey(scope)
	if key == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	buf := append(b.scopes[key], msgs...)
	if len(buf) > b.cap {
		// Drop oldest entries; copy the tail into a fresh slice so the
		// underlying array does not retain references to evicted
		// messages indefinitely.
		tail := buf[len(buf)-b.cap:]
		fresh := make([]llm.Message, len(tail))
		copy(fresh, tail)
		buf = fresh
	}
	b.scopes[key] = buf
	return nil
}

// messageBufferKey reduces a Scope to a stable string key for buffer
// indexing. Uses the same namespace function the storage path uses so
// the buffer scopes align with the recall namespaces.
func messageBufferKey(s Scope) string {
	if s.RuntimeID == "" {
		return ""
	}
	return NamespaceFor(s)
}
