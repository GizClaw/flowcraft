package httpx

import (
	"bufio"
	"fmt"
	"io"
	"iter"
	"strings"
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
func Frames(r io.Reader) iter.Seq2[Frame, error] {
	return func(yield func(Frame, error) bool) {
		reader := bufio.NewReaderSize(r, 64<<10)
		var (
			frame Frame
			data  strings.Builder
			size  int
			seen  bool
		)
		flush := func() bool {
			if !seen {
				return true
			}
			frame.Data = []byte(data.String())
			emitted := frame
			frame, data, size, seen = Frame{}, strings.Builder{}, 0, false
			return yield(emitted, nil)
		}
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				switch {
				case trimmed == "":
					if !flush() {
						return
					}
				case strings.HasPrefix(trimmed, ":"):
					// A comment, used as a keep-alive.
				default:
					field, value, _ := strings.Cut(trimmed, ":")
					value = strings.TrimPrefix(value, " ")
					switch field {
					case "event":
						frame.Event = value
						seen = true
					case "id":
						frame.ID = value
						seen = true
					case "data":
						size += len(value)
						if size > frameLimit {
							yield(Frame{}, fmt.Errorf("event stream frame exceeds %d bytes", frameLimit))
							return
						}
						if data.Len() > 0 {
							data.WriteByte('\n')
						}
						data.WriteString(value)
						seen = true
					}
				}
			}
			if err != nil {
				// A frame the stream ended inside is still delivered: the
				// alternative is losing the terminal event of a short answer.
				if !flush() {
					return
				}
				if err != io.EOF {
					yield(Frame{}, err)
				}
				return
			}
		}
	}
}
