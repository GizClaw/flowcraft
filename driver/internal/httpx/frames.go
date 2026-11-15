package httpx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
)

// Frame is one event-stream message: the vendor's event name (empty when the
// stream carries none) and its data, with multi-line data joined by newlines as
// the format specifies.
type Frame struct {
	Event string
	ID    string
	Data  []byte
}

// frameLimit bounds one frame. A stream that never sends a blank line would
// otherwise let a broken server decide how much the driver buffers.
const frameLimit = 1 << 20

// readBuffer is the per-stream read buffer. A frame is usually well under a
// kilobyte and a line that runs past the buffer has its own path below, so this
// is sized for many concurrent streams rather than for one long line.
const readBuffer = 16 << 10

// The fields this parser acts on. Comparing bytes instead of building a string
// keeps the per-line cost at zero allocations; a stream spends most of its time
// here.
var (
	fieldData  = []byte("data")
	fieldEvent = []byte("event")
	fieldID    = []byte("id")
	colon      = []byte(":")
	space      = []byte(" ")
)

// Frames parses a text/event-stream body.
//
// The rules are the format's, not a vendor's: a line is a field and its value,
// ":" starts a comment, an empty line dispatches what was accumulated, and a
// final frame without a trailing blank line is still a frame — servers do omit
// it, and dropping the last event is the kind of bug that only shows up on a
// short answer.
//
// It does not interpret the data. `[DONE]` is a vendor convention, so the caller
// decides what a payload means; this returns it either way.
//
// The payload of a frame is copied out and belongs to the caller. Everything
// else is reused between frames, so a stream costs one allocation per frame
// rather than one per line.
func Frames(r io.Reader) iter.Seq2[Frame, error] {
	return func(yield func(Frame, error) bool) {
		reader := bufio.NewReaderSize(r, readBuffer)
		var (
			frame Frame
			data  []byte
			line  []byte
			size  int
			seen  bool
		)
		flush := func() bool {
			if !seen {
				return true
			}
			frame.Data = append([]byte(nil), data...)
			emitted := frame
			frame, data, size, seen = Frame{}, data[:0], 0, false
			return yield(emitted, nil)
		}
		// handle interprets one complete line and reports whether the stream
		// should continue.
		handle := func(raw []byte) (bool, error) {
			raw = bytes.TrimRight(raw, "\r\n")
			switch {
			case len(raw) == 0:
				return flush(), nil
			case raw[0] == ':':
				// A comment, used as a keep-alive.
				return true, nil
			}
			field, value, _ := bytes.Cut(raw, colon)
			value = bytes.TrimPrefix(value, space)
			switch {
			case bytes.Equal(field, fieldData):
				size += len(value)
				if size > frameLimit {
					return false, fmt.Errorf("event stream frame exceeds %d bytes", frameLimit)
				}
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
				seen = true
			case bytes.Equal(field, fieldEvent):
				frame.Event = string(value)
				seen = true
			case bytes.Equal(field, fieldID):
				frame.ID = string(value)
				seen = true
			}
			return true, nil
		}
		// finish delivers what is accumulated and reports the stream's end.
		finish := func(err error) bool {
			// A frame the stream ended inside is still delivered: the
			// alternative is losing the terminal event of a short answer.
			if !flush() {
				return false
			}
			if err != nil {
				yield(Frame{}, err)
			}
			return false
		}
		for {
			chunk, err := reader.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				// One line longer than the read buffer: accumulate it without
				// letting a server pick the size.
				line = append(line, chunk...)
				if len(line) > frameLimit {
					yield(Frame{}, fmt.Errorf("event stream line exceeds %d bytes", frameLimit))
					return
				}
				continue
			}
			raw := chunk
			if len(line) > 0 {
				line = append(line, chunk...)
				raw, line = line, line[:0]
			}
			if len(raw) > 0 {
				keep, handleErr := handle(raw)
				if handleErr != nil {
					yield(Frame{}, handleErr)
					return
				}
				if !keep {
					return
				}
			}
			if err != nil {
				var streamErr error
				if !errors.Is(err, io.EOF) {
					streamErr = err
				}
				finish(streamErr)
				return
			}
		}
	}
}
