package workspace

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// segmentBuild is the result of writing one segment to disk:
// the manifest entry that will be appended to the next manifest,
// and the size in bytes that the size-tiered compactor will use to
// pick merge candidates. The caller (namespace flush) appends ref
// to a fresh manifest then performs the atomic manifest swap.
type segmentBuild struct {
	ref segmentRef
}

// writeSegment emits one segment from snap under <ns>/segments/<id>/.
// The publication contract is: every sibling file is written first;
// meta.json is written LAST. A directory whose meta.json is missing
// is therefore an abandoned partial flush and is GC'd on next open.
//
// This commit emits the doc tier (docs.jsonl + docs.offsets.bin),
// the tombstones list, and the meta.json commit marker. The bm25
// and vector sidecars are populated in the read-path commit;
// segments built by this commit advertise zero-dim vectors and an
// empty BM25 index, which the search code treats as "no contribution
// from this segment" rather than as an error.
func writeSegment(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	id uint64,
	snap *memtableSnapshot,
	now time.Time,
) (*segmentBuild, error) {
	upserts := snap.upserts()
	tombs := snap.tombstones()
	if len(upserts) == 0 && len(tombs) == 0 {
		// Nothing to flush; caller should skip.
		return nil, nil
	}

	docsBytes, offsetsBytes, totalLen, err := encodeDocs(upserts)
	if err != nil {
		return nil, err
	}
	tombBytes := encodeTombstones(tombs)

	checksums := map[string]uint32{
		"docs.jsonl":       crc32.ChecksumIEEE(docsBytes),
		"docs.offsets.bin": crc32.ChecksumIEEE(offsetsBytes),
		"tombstones.bin":   crc32.ChecksumIEEE(tombBytes),
	}

	avgDocLen := 0.0
	if len(upserts) > 0 {
		avgDocLen = float64(totalLen) / float64(len(upserts))
	}

	meta := segmentMeta{
		Version:       segmentMetaVersion,
		ID:            id,
		DocCount:      len(upserts),
		VectorDim:     vectorDim(upserts),
		AvgDocLength:  avgDocLen,
		BuildAt:       now,
		FileChecksums: checksums,
	}

	if err := ws.Write(ctx, paths.segmentDocsPath(id), docsBytes); err != nil {
		return nil, fmt.Errorf("writeSegment: docs: %w", err)
	}
	if err := ws.Write(ctx, paths.segmentOffsetsPath(id), offsetsBytes); err != nil {
		return nil, fmt.Errorf("writeSegment: offsets: %w", err)
	}
	if err := ws.Write(ctx, paths.segmentTombstonesPath(id), tombBytes); err != nil {
		return nil, fmt.Errorf("writeSegment: tombstones: %w", err)
	}

	// meta.json is the segment-ready commit marker; if any earlier
	// Write fails the dir exists without it, and the next open's GC
	// pass deletes the half-written segment.
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("writeSegment: marshal meta: %w", err)
	}
	if err := ws.Write(ctx, paths.segmentMetaPath(id), metaBytes); err != nil {
		return nil, fmt.Errorf("writeSegment: meta: %w", err)
	}

	size := int64(len(docsBytes)) + int64(len(offsetsBytes)) +
		int64(len(tombBytes)) + int64(len(metaBytes))

	return &segmentBuild{
		ref: segmentRef{
			ID:        id,
			DocCount:  meta.DocCount,
			VectorDim: meta.VectorDim,
			SizeBytes: size,
			BuildAt:   now,
		},
	}, nil
}

// encodeDocs packs upserts into the docs.jsonl + offsets formats.
// The offsets file is a sequence of uint64 big-endian entries: one
// per doc, naming the byte offset (within docs.jsonl) where the
// doc starts. A trailing offset equal to len(docs.jsonl) is appended
// so [start, end) ranges can be derived without a special case for
// the last doc.
//
// totalLen is the cumulative count of marshaled doc bytes (used by
// segmentMeta.AvgDocLength).
func encodeDocs(docs []retrieval.Doc) (docsBytes, offsetsBytes []byte, totalLen int, err error) {
	if len(docs) == 0 {
		return nil, nil, 0, nil
	}
	// Stable iteration order: sort by ID so segments are
	// content-deterministic, which makes byte-level diffs in tests
	// meaningful and makes future cross-segment merges cheaper.
	sorted := make([]retrieval.Doc, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var docsBuf bytes.Buffer
	offsetsBuf := bytes.NewBuffer(make([]byte, 0, (len(sorted)+1)*8))
	for _, d := range sorted {
		off := uint64(docsBuf.Len())
		buf := offsetsBuf.Bytes()
		offsetsBuf.Reset()
		offsetsBuf.Write(buf)
		_ = binary.Write(offsetsBuf, binary.BigEndian, off)

		raw, err := json.Marshal(d)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("encodeDocs: marshal %s: %w", d.ID, err)
		}
		docsBuf.Write(raw)
		docsBuf.WriteByte('\n')
		totalLen += len(raw)
	}
	// Trailing sentinel offset == file length.
	tailBuf := offsetsBuf.Bytes()
	offsetsBuf.Reset()
	offsetsBuf.Write(tailBuf)
	_ = binary.Write(offsetsBuf, binary.BigEndian, uint64(docsBuf.Len()))

	return docsBuf.Bytes(), offsetsBuf.Bytes(), totalLen, nil
}

// encodeTombstones produces a length-prefixed sequence of UTF-8 IDs:
//
//	[uint32 count][uint16 idLen][idBytes]...repeat...
//
// The count prefix lets readers preallocate the result slice; the
// per-ID uint16 length keeps the format compact for typical UUID-
// length IDs while still allowing up to 64 KiB per ID.
func encodeTombstones(ids []string) []byte {
	if len(ids) == 0 {
		// We still emit an empty file so the segment-meta crc32
		// of the file is well-defined.
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, 0)
		return buf
	}
	sort.Strings(ids)
	var buf bytes.Buffer
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(ids)))
	buf.Write(hdr)
	for _, id := range ids {
		idLen := make([]byte, 2)
		binary.BigEndian.PutUint16(idLen, uint16(len(id)))
		buf.Write(idLen)
		buf.WriteString(id)
	}
	return buf.Bytes()
}

// vectorDim returns the dimension of the first non-empty vector
// in docs, or 0 when no doc carries a vector. Mismatched dims
// across docs are caught at flush time elsewhere; this helper just
// records what the segment claims to hold.
func vectorDim(docs []retrieval.Doc) int {
	for _, d := range docs {
		if len(d.Vector) > 0 {
			return len(d.Vector)
		}
	}
	return 0
}
