package memorytest_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/memory/memorytest"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// memImpl is a minimal in-memory impl that satisfies the full
// AppendOp / LoadOp / RecallOp / ImportOp / CompactOp / ArchiveOp
// surface. It is the reference impl memorytest is run against
// to prove the suites are correct.
type memImpl struct {
	mu       sync.Mutex
	messages map[memKey][]memRecord
	hits     map[memKey][]memHit
	docs     map[memKey][]memDoc
}

type memKey struct {
	rt, user, conv string
}

type memRecord struct {
	id    string
	seq   uint64
	msg   message.Message
	idKey string
}

type memHit struct {
	id, source, query string
	score             float64
	parts             []message.Part
}

type memDoc struct {
	id         string
	chunkCount int
}

func newMemImpl() *memImpl {
	return &memImpl{
		messages: map[memKey][]memRecord{},
		hits:     map[memKey][]memHit{},
		docs:     map[memKey][]memDoc{},
	}
}

func keyOf(s memory.Scope, conv string) memKey {
	return memKey{rt: s.RuntimeID, user: s.UserID, conv: conv}
}

func seqOf(idKey string, m *memImpl, k memKey) uint64 {
	var max uint64
	for _, r := range m.messages[k] {
		if r.idKey == idKey || r.id == idKey {
			return r.seq
		}
		if r.seq > max {
			max = r.seq
		}
	}
	return max
}

func (m *memImpl) nextSeq(k memKey) uint64 {
	var max uint64
	for _, r := range m.messages[k] {
		if r.seq > max {
			max = r.seq
		}
	}
	return max + 1
}

// --- AppendOp ---

func (m *memImpl) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpAppend,
		Decisions: []memory.Decision{
			{Field: memory.FieldAppendScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendIdempotencyKey, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendRecords, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendMetadata, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteAppend(_ context.Context, req memory.AppendRequest) (memory.AppendResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(req.Scope, req.ConversationID)
	// IdempotencyKey dedup: if the same key has been seen
	// under this scope, return the original response without
	// writing again. We tag the records with the key.
	if req.IdempotencyKey != "" {
		for _, r := range m.messages[k] {
			if r.idKey == req.IdempotencyKey {
				return memory.AppendResponse{Appended: 0, LastSeq: r.seq}, nil
			}
		}
	}
	var maxSeq uint64
	appended := 0
	for _, rec := range req.Records {
		seq := m.nextSeq(k)
		m.messages[k] = append(m.messages[k], memRecord{
			id:    rec.ID,
			seq:   seq,
			msg:   rec.Message,
			idKey: req.IdempotencyKey,
		})
		if seq > maxSeq {
			maxSeq = seq
		}
		appended++
	}
	return memory.AppendResponse{Appended: appended, LastSeq: maxSeq}, nil
}

// --- LoadOp ---

func (m *memImpl) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpLoad,
		Decisions: []memory.Decision{
			{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadCursor, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadLimit, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadReverse, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteLoad(_ context.Context, req memory.LoadRequest) (memory.LoadResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(req.Scope, req.ConversationID)
	src := append([]memRecord(nil), m.messages[k]...)
	if req.Reverse {
		sort.SliceStable(src, func(i, j int) bool { return src[i].seq > src[j].seq })
	}
	// Cursor is the Seq after which to continue; empty Cursor
	// means "from the start (or end, when Reverse)".
	var start int
	if req.Cursor != "" {
		for i, r := range src {
			if r.seq > mustParseSeq(req.Cursor) {
				start = i
				break
			}
		}
	}
	end := start + req.Limit
	if req.Limit <= 0 || end > len(src) {
		end = len(src)
	}
	window := src[start:end]
	out := make([]memory.Record, len(window))
	for i, r := range window {
		out[i] = memory.Record{ID: r.id, Seq: r.seq, Message: r.msg}
	}
	var next string
	if end < len(src) {
		next = formatSeq(src[end-1].seq)
	}
	return memory.LoadResponse{Records: out, NextCursor: next}, nil
}

// --- RecallOp ---

func (m *memImpl) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpRecall,
		Decisions: []memory.Decision{
			{Field: memory.FieldRecallScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallQuery, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallTopK, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallFilters, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallMinScore, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteRecall(_ context.Context, req memory.RecallRequest) (memory.RecallResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(req.Scope, req.ConversationID)
	src := m.hits[k]
	// Filter by MinScore.
	var filtered []memHit
	for _, h := range src {
		if h.query == req.Query && h.score >= req.MinScore {
			filtered = append(filtered, h)
		}
	}
	// Sort by score descending.
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].score > filtered[j].score })
	// Truncate to TopK.
	if req.TopK > 0 && len(filtered) > req.TopK {
		filtered = filtered[:req.TopK]
	}
	out := make([]memory.Hit, len(filtered))
	for i, h := range filtered {
		out[i] = memory.Hit{
			ID:     h.id,
			Score:  h.score,
			Source: h.source,
			Parts:  h.parts,
		}
	}
	return memory.RecallResponse{Hits: out}, nil
}

// seedHit is a test-only helper that puts a hit into the store
// keyed by query, so a Recall call can find it.
func (m *memImpl) seedHit(scope memory.Scope, conv, query, id, source string, score float64, parts []message.Part) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(scope, conv)
	m.hits[k] = append(m.hits[k], memHit{
		id: id, source: source, query: query, score: score, parts: parts,
	})
}

// --- ImportOp ---

func (m *memImpl) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpImport,
		Decisions: []memory.Decision{
			{Field: memory.FieldImportScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportDatasetID, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportSource, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportTags, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportChunkPolicy, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteImport(_ context.Context, req memory.ImportRequest) (memory.ImportResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(req.Scope, req.DatasetID)
	docID := "doc-" + req.Source
	m.docs[k] = append(m.docs[k], memDoc{id: docID, chunkCount: 1})
	return memory.ImportResponse{DocumentID: docID, ChunkCount: 1}, nil
}

// --- CompactOp / ArchiveOp ---

func (m *memImpl) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpCompact,
		Decisions: []memory.Decision{
			{Field: memory.FieldCompactScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldCompactOlderThan, Disposition: memory.DispositionNative},
			{Field: memory.FieldCompactKeep, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{Compacted: 0, Bytes: 0}, nil
}

func (m *memImpl) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpArchive,
		Decisions: []memory.Decision{
			{Field: memory.FieldArchiveScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldArchiveOlderThan, Disposition: memory.DispositionNative},
			{Field: memory.FieldArchiveDestination, Disposition: memory.DispositionNative},
		},
	}
}

func (m *memImpl) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{Archived: 0, Bytes: 0}, nil
}

// --- helpers ---

func mustParseSeq(s string) uint64 {
	// minimal uint64 parser; the cursor format is "seq=<N>".
	if len(s) < 5 || s[:4] != "seq=" {
		return 0
	}
	var n uint64
	for i := 4; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

func formatSeq(n uint64) string { return "seq=" + uintToString(n) }

func uintToString(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestMemImplPassesContract wires memImpl into every Run*
// suite. The test serves two purposes:
//
//  1. prove memorytest's contracts are reachable from a real
//     impl (the in-memory store here is a faithful reference
//     impl for the kernel's contract);
//  2. give subsequent impls a known-good pattern to copy from.
func TestMemImplPassesContract(t *testing.T) {
	spec := memory.Spec{RuntimeID: "test"}
	scope := memory.Scope{RuntimeID: spec.RuntimeID, UserID: "u"}
	conv := "conv-1"

	// freshRT builds a runtime with a fresh memImpl so each
	// sub-test starts from an empty store. A shared instance
	// would let state leak across sub-tests.
	freshRT := func(t *testing.T) *memory.Runtime {
		t.Helper()
		impl := newMemImpl()
		rt, err := memory.New(spec, memory.Impls{
			Append:  impl,
			Load:    impl,
			Recall:  impl,
			Import:  impl,
			Compact: impl,
			Archive: impl,
		})
		if err != nil {
			t.Fatalf("memory.New: %v", err)
		}
		t.Cleanup(func() { _ = rt.Close() })

		// Seed long-term memory once for the Recall suite.
		impl.seedHit(scope, "", "seed", "h-1", "transcript:c/seq-1", 0.9,
			[]message.Part{message.TextPart{Text: "alpha body"}})
		impl.seedHit(scope, "", "seed", "h-2", "transcript:c/seq-2", 0.7,
			[]message.Part{message.TextPart{Text: "alpha more"}})
		impl.seedHit(scope, "", "seed", "h-3", "transcript:c/seq-3", 0.3,
			[]message.Part{message.TextPart{Text: "alpha weak"}})

		return rt
	}

	memorytest.RunScope(t, memorytest.ScopeSuite{})

	memorytest.RunAppend(t, memorytest.AppendSuite{
		Spec:           spec,
		BuildRuntime:   freshRT,
		SampleScope:    scope,
		ConversationID: conv,
	})

	memorytest.RunLoad(t, memorytest.LoadSuite{
		Spec:           spec,
		BuildRuntime:   freshRT,
		SampleScope:    scope,
		ConversationID: conv,
	})

	memorytest.RunRecall(t, memorytest.RecallSuite{
		Spec:         spec,
		BuildRuntime: freshRT,
		SampleScope:  scope,
	})

	memorytest.RunImport(t, memorytest.ImportSuite{
		Spec:         spec,
		BuildRuntime: freshRT,
		SampleScope:  scope,
	})

	memorytest.RunCompact(t, memorytest.CompactSuite{
		Spec:           spec,
		BuildRuntime:   freshRT,
		SampleScope:    scope,
		ConversationID: conv,
	})

	memorytest.RunArchive(t, memorytest.ArchiveSuite{
		Spec:         spec,
		BuildRuntime: freshRT,
		SampleScope:  scope,
	})
}

// TestNoopRuntimePassesContract runs the slimmed-down noop
// suite to confirm the kernel handles every op against a
// real (NoopRuntime) implementation.
func TestNoopRuntimePassesContract(t *testing.T) {
	memorytest.RunNoop(t)
}

// silence unused import warning when the helper isn't called.
var _ = time.Second
