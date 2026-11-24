package wire

import (
	"encoding/binary"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Write requests are framed for the same reason reads are: a large Stdin measured
// 1.35 ms/MiB of base64-in-JSON round trip, and the payload is bytes.
//
// Layout, little-endian:
//
//	magic  4   "FSW1"
//	id_len 2   length of the session id that follows
//	id     id_len
//	len    4   number of payload bytes
//	data   len
//
// A write carries exactly one payload, so there is no chunk list here: the
// request names one session and one buffer.
const (
	writeMagic    = "FSW1"
	writeHeadSize = 4 + 2
	writeTailSize = 4
)

// AppendWriteRequest appends one framed write body to dst.
func AppendWriteRequest(dst []byte, sessionID string, data []byte) []byte {
	dst = append(dst, writeMagic...)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(sessionID)))
	dst = append(dst, sessionID...)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(data)))
	return append(dst, data...)
}

// WriteRequestSize is the encoded size of one write body, which the client uses to
// size its buffer once.
func WriteRequestSize(sessionID string, data []byte) int {
	return writeHeadSize + len(sessionID) + writeTailSize + len(data)
}

// DecodeWriteRequest parses one framed write body. The returned data points into
// body: the server hands it to the session without a second copy.
func DecodeWriteRequest(body []byte) (sessionID string, data []byte, err error) {
	if len(body) < writeHeadSize+writeTailSize {
		return "", nil, errdefs.Internalf(
			"wire: write request is %d bytes, shorter than its header", len(body))
	}
	if string(body[:4]) != writeMagic {
		return "", nil, errdefs.Internalf(
			"wire: write request does not start with %q", writeMagic)
	}
	idLen := int(binary.LittleEndian.Uint16(body[4:6]))
	rest := body[writeHeadSize:]
	if idLen == 0 || idLen > len(rest)-writeTailSize {
		return "", nil, errdefs.Internalf("wire: write request names a %d-byte session id", idLen)
	}
	sessionID = string(rest[:idLen])
	rest = rest[idLen:]
	length := int(binary.LittleEndian.Uint32(rest[:writeTailSize]))
	rest = rest[writeTailSize:]
	if length > len(rest) {
		return "", nil, errdefs.Internalf(
			"wire: write request claims %d bytes, %d left", length, len(rest))
	}
	if length != len(rest) {
		return "", nil, errdefs.Internalf(
			"wire: write request has %d trailing bytes", len(rest)-length)
	}
	return sessionID, rest, nil
}
