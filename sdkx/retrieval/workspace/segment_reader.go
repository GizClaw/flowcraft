package workspace

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// segmentReader is the read-side handle for one on-disk segment.
//
// The segment is stored as a small set of files (docs.jsonl +
// docs.offsets.bin + tombstones.bin + meta.json). BM25 corpus
// statistics and per-doc tokens are NOT persisted: they are
// derivatives of docs.jsonl that the reader rebuilds in memory on
// first Search via [textsearch]. This keeps BM25 logic in exactly
// one place (sdk/textsearch) at the cost of ~tens of milliseconds
// per cold segment load — within the few-thousand-doc segment
// budget the default flush threshold imposes.
//
// Concurrency: meta + tombstones load in [openSegmentReader] (so
// the reader can answer "is X tombstoned" with no I/O). docs and
// the BM25 corpus load lazily, each guarded by a [sync.Once]. After
// loading completes the populated fields are immutable for the
// reader's life so concurrent searches read them lock-free.
type segmentReader struct {
	ws    sdkworkspace.Workspace
	paths pathHelper
	id    uint64

	meta segmentMeta

	// tombSet is the set of doc IDs deleted within this segment's
	// lifetime. Loaded eagerly because every Search consults it.
	tombSet map[string]struct{}

	docsOnce sync.Once
	docsErr  error
	// docs is the segment's doc list in segment order (sorted by
	// ID; same order as docs.jsonl lines). Empty for tombstone-
	// only segments.
	docs []retrieval.Doc
	// idIndex maps doc id -> position in docs, enabling O(1)
	// membership tests for [Get] and tombstone filtering.
	idIndex map[string]int

	bm25Once sync.Once
	bm25Err  error
	// docTokens parallels docs: docTokens[i] is the tokenizer
	// output for docs[i].Content. Held so [Index.Search] can both
	// fold this segment's docs into a per-Search global corpus and
	// score against that corpus without re-tokenizing.
	//
	// Note: BM25 corpus stats themselves are NOT cached on the
	// segment. A segment-local corpus would let segments answer
	// scoring requests in isolation, but BM25 IDF is corpus-
	// relative and merging per-segment scores at the top of Search
	// produces ranks that depend on which segment a doc landed in
	// rather than its global frequency. Search rebuilds the corpus
	// across all live (non-tombstoned) segment + memtable docs
	// each call; tokens are the only segment-cacheable piece.
	docTokens [][]string
}

// openSegmentReader loads meta + tombstones for the segment named
// by ref. Returns [ErrCorrupt] (wrapped with context) on checksum
// mismatch, version mismatch, or framing errors.
func openSegmentReader(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	ref segmentRef,
) (*segmentReader, error) {
	r := &segmentReader{ws: ws, paths: paths, id: ref.ID}

	metaBytes, err := ws.Read(ctx, paths.segmentMetaPath(ref.ID))
	if err != nil {
		return nil, fmt.Errorf("openSegment[%016x]: meta read: %w", ref.ID, err)
	}
	if err := json.Unmarshal(metaBytes, &r.meta); err != nil {
		return nil, fmt.Errorf("%w: segment %016x meta unmarshal: %v",
			ErrCorrupt, ref.ID, err)
	}
	if r.meta.Version != segmentMetaVersion {
		return nil, fmt.Errorf("%w: segment %016x meta version=%d want=%d",
			ErrCorrupt, ref.ID, r.meta.Version, segmentMetaVersion)
	}

	tombBytes, err := ws.Read(ctx, paths.segmentTombstonesPath(ref.ID))
	if err != nil {
		return nil, fmt.Errorf("openSegment[%016x]: tombstones read: %w", ref.ID, err)
	}
	if want, ok := r.meta.FileChecksums["tombstones.bin"]; ok {
		if got := crc32.ChecksumIEEE(tombBytes); got != want {
			return nil, fmt.Errorf("%w: segment %016x tombstones crc=%x want=%x",
				ErrCorrupt, ref.ID, got, want)
		}
	}
	tombs, err := decodeTombstones(tombBytes)
	if err != nil {
		return nil, fmt.Errorf("openSegment[%016x]: %w", ref.ID, err)
	}
	r.tombSet = make(map[string]struct{}, len(tombs))
	for _, id := range tombs {
		r.tombSet[id] = struct{}{}
	}
	return r, nil
}

// loadDocs lazily reads docs.jsonl + docs.offsets.bin, verifies
// their CRCs, and decodes the segment's full doc list into r.docs.
// Tombstone-only segments have neither file (the writer omits both
// when there are no upserts); loadDocs short-circuits to an empty
// list in that case.
func (r *segmentReader) loadDocs(ctx context.Context) error {
	r.docsOnce.Do(func() {
		// Tombstone-only segments record no docs.jsonl checksum;
		// nothing to load and the empty doc list is the right
		// answer.
		if _, ok := r.meta.FileChecksums["docs.jsonl"]; !ok {
			r.docs = nil
			r.idIndex = map[string]int{}
			return
		}
		raw, err := r.ws.Read(ctx, r.paths.segmentDocsPath(r.id))
		if err != nil {
			r.docsErr = fmt.Errorf("loadDocs[%016x]: read docs: %w", r.id, err)
			return
		}
		if want := r.meta.FileChecksums["docs.jsonl"]; crc32.ChecksumIEEE(raw) != want {
			r.docsErr = fmt.Errorf("%w: segment %016x docs.jsonl crc mismatch",
				ErrCorrupt, r.id)
			return
		}
		offsBytes, err := r.ws.Read(ctx, r.paths.segmentOffsetsPath(r.id))
		if err != nil {
			r.docsErr = fmt.Errorf("loadDocs[%016x]: read offsets: %w", r.id, err)
			return
		}
		if want := r.meta.FileChecksums["docs.offsets.bin"]; crc32.ChecksumIEEE(offsBytes) != want {
			r.docsErr = fmt.Errorf("%w: segment %016x docs.offsets.bin crc mismatch",
				ErrCorrupt, r.id)
			return
		}
		if len(offsBytes)%8 != 0 {
			r.docsErr = fmt.Errorf("%w: segment %016x offsets size=%d not 8-aligned",
				ErrCorrupt, r.id, len(offsBytes))
			return
		}
		nOff := len(offsBytes) / 8
		// docCount entries + 1 sentinel.
		if nOff != r.meta.DocCount+1 {
			r.docsErr = fmt.Errorf("%w: segment %016x offsets entries=%d want=%d",
				ErrCorrupt, r.id, nOff, r.meta.DocCount+1)
			return
		}
		offs := make([]uint64, nOff)
		for i := range offs {
			offs[i] = binary.BigEndian.Uint64(offsBytes[i*8 : (i+1)*8])
		}
		// Decode docs by slicing docs.jsonl with consecutive offset
		// pairs; each slice is "<json>\n", strip the trailing
		// newline before json.Unmarshal.
		docs := make([]retrieval.Doc, r.meta.DocCount)
		idIndex := make(map[string]int, r.meta.DocCount)
		for i := 0; i < r.meta.DocCount; i++ {
			start, end := offs[i], offs[i+1]
			if end > uint64(len(raw)) || start > end {
				r.docsErr = fmt.Errorf("%w: segment %016x offsets[%d]=[%d,%d) out of range",
					ErrCorrupt, r.id, i, start, end)
				return
			}
			line := bytes.TrimSuffix(raw[start:end], []byte{'\n'})
			if err := json.Unmarshal(line, &docs[i]); err != nil {
				r.docsErr = fmt.Errorf("%w: segment %016x doc %d unmarshal: %v",
					ErrCorrupt, r.id, i, err)
				return
			}
			idIndex[docs[i].ID] = i
		}
		r.docs = docs
		r.idIndex = idIndex
	})
	return r.docsErr
}

// loadBM25 lazily tokenizes every doc into r.docTokens. It does NOT
// build a segment-local corpus: see the comment on docTokens for
// why. Search aggregates tokens across segments + memtable into a
// single per-call corpus before scoring.
//
// loadBM25 is a no-op for tombstone-only segments — docTokens stays
// nil, which the search loop treats as "no contribution".
func (r *segmentReader) loadBM25(ctx context.Context, tok textsearch.Tokenizer) error {
	if err := r.loadDocs(ctx); err != nil {
		return err
	}
	r.bm25Once.Do(func() {
		if len(r.docs) == 0 {
			return
		}
		docTokens := make([][]string, len(r.docs))
		for i, d := range r.docs {
			docTokens[i] = tok.Tokenize(d.Content)
		}
		r.docTokens = docTokens
	})
	return r.bm25Err
}

// isTombstoned reports whether id was deleted within this segment.
// Cheap (map lookup); used by the search merge to suppress hits
// produced by older segments.
func (r *segmentReader) isTombstoned(id string) bool {
	_, ok := r.tombSet[id]
	return ok
}

// decodeTombstones is the inverse of [encodeTombstones]. Returns
// [ErrCorrupt] on framing mismatch.
func decodeTombstones(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: tombstones header truncated", ErrCorrupt)
	}
	count := binary.BigEndian.Uint32(data[:4])
	off := 4
	out := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		if off+2 > len(data) {
			return nil, fmt.Errorf("%w: tombstones[%d] length truncated", ErrCorrupt, i)
		}
		ln := binary.BigEndian.Uint16(data[off : off+2])
		off += 2
		if off+int(ln) > len(data) {
			return nil, fmt.Errorf("%w: tombstones[%d] body truncated", ErrCorrupt, i)
		}
		out = append(out, string(data[off:off+int(ln)]))
		off += int(ln)
	}
	return out, nil
}
