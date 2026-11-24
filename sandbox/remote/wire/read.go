package wire

import (
	"encoding/binary"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Read responses are the one large payload on this wire, and they are sent as raw
// bytes rather than as JSON with base64 inside it: measured on a 1 MiB payload,
// JSON+base64 costs 594 µs to encode and 754 µs to decode, against 16 µs for a
// memcpy of the same size. The framing below is the difference between those
// numbers and the copy.
//
// Layout, little-endian, one header then the chunks back to back:
//
//	magic     4   "FSR1"
//	flags     1   bit0 EOF, bit1 idle
//	count     4   number of chunks
//	next_seq  8
//	per chunk:
//	  seq     8
//	  stream  1
//	  pad     1
//	  len     4
//	  data    len
//
// The magic is not a negotiation (both ends always ship together): it makes "this
// body is not one of ours" detectable — a proxy's HTML error page, a truncated
// response — instead of being parsed into nonsense chunk lengths.
const (
	readMagic     = "FSR1"
	readFlagEOF   = 1 << 0
	readFlagIdle  = 1 << 1
	readHeadSize  = 4 + 1 + 4 + 8
	readChunkHead = 8 + 1 + 1 + 4
)

// ReadResponseSize is the encoded size of one read response, which the server
// needs as Content-Length so the client can size its buffer once.
func ReadResponseSize(out sandbox.SessionOutput, idle bool) int {
	size := readHeadSize
	if idle {
		return size
	}
	for _, chunk := range out.Chunks {
		size += readChunkHead + len(chunk.Data)
	}
	return size
}

// AppendReadHeader appends the response header to dst: the part that precedes the
// chunks, so a server can write headers first and stream each chunk's bytes
// through without assembling a copy of the whole response.
func AppendReadHeader(dst []byte, out sandbox.SessionOutput, idle bool) []byte {
	var flags byte
	if out.EOF {
		flags |= readFlagEOF
	}
	count := len(out.Chunks)
	if idle {
		flags |= readFlagIdle
		count = 0
	}
	dst = append(dst, readMagic...)
	dst = append(dst, flags)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(count))
	return binary.LittleEndian.AppendUint64(dst, uint64(out.NextSeq))
}

// AppendChunkHeader appends one chunk's header, to be followed by its data.
func AppendChunkHeader(dst []byte, chunk sandbox.OutputChunk) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(chunk.Seq))
	dst = append(dst, byte(chunk.Stream), 0)
	return binary.LittleEndian.AppendUint32(dst, uint32(len(chunk.Data)))
}

// AppendReadResponse appends one whole read response to dst. The server uses the
// header/chunk helpers instead when it writes straight to the connection.
func AppendReadResponse(dst []byte, out sandbox.SessionOutput, idle bool) []byte {
	dst = AppendReadHeader(dst, out, idle)
	if idle {
		return dst
	}
	for _, chunk := range out.Chunks {
		dst = AppendChunkHeader(dst, chunk)
		dst = append(dst, chunk.Data...)
	}
	return dst
}

// DecodeReadResponse parses one read response.
//
// The returned chunks' Data slices point into body, so a read costs one allocation
// (the body) rather than one per chunk. body stays alive through those slices, and
// the client never rewrites it.
func DecodeReadResponse(body []byte) (sandbox.SessionOutput, bool, error) {
	if len(body) < readHeadSize {
		return sandbox.SessionOutput{}, false, errdefs.Internalf(
			"wire: read response is %d bytes, shorter than its %d-byte header", len(body), readHeadSize)
	}
	if string(body[:4]) != readMagic {
		return sandbox.SessionOutput{}, false, errdefs.Internalf(
			"wire: read response does not start with %q", readMagic)
	}
	flags := body[4]
	count := int(binary.LittleEndian.Uint32(body[5:9]))
	nextSeq := int64(binary.LittleEndian.Uint64(body[9:17]))

	if flags&readFlagIdle != 0 {
		return sandbox.SessionOutput{NextSeq: nextSeq}, true, nil
	}
	// A count that cannot fit in the body is a malformed frame, not an empty one:
	// checking it first turns a corrupt length into one clear error.
	if count > (len(body)-readHeadSize)/readChunkHead {
		return sandbox.SessionOutput{}, false, errdefs.Internalf(
			"wire: read response claims %d chunks, which cannot fit in %d bytes", count, len(body))
	}

	chunks := make([]sandbox.OutputChunk, 0, count)
	rest := body[readHeadSize:]
	for i := range count {
		if len(rest) < readChunkHead {
			return sandbox.SessionOutput{}, false, errdefs.Internalf(
				"wire: truncated chunk header at index %d", i)
		}
		seq := int64(binary.LittleEndian.Uint64(rest[:8]))
		stream := sandbox.SessionStream(rest[8])
		length := int(binary.LittleEndian.Uint32(rest[10:14]))
		rest = rest[readChunkHead:]
		if length > len(rest) {
			return sandbox.SessionOutput{}, false, errdefs.Internalf(
				"wire: chunk %d claims %d bytes, %d left", i, length, len(rest))
		}
		chunks = append(chunks, sandbox.OutputChunk{Seq: seq, Stream: stream, Data: rest[:length]})
		rest = rest[length:]
	}
	if len(rest) != 0 {
		return sandbox.SessionOutput{}, false, errdefs.Internalf(
			"wire: read response has %d trailing bytes", len(rest))
	}
	return sandbox.SessionOutput{
		NextSeq: nextSeq,
		Chunks:  chunks,
		EOF:     flags&readFlagEOF != 0,
	}, false, nil
}
