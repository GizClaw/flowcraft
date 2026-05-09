package dataset

import "bytes"

// newLineReader replaces all line breaks with whitespace + comma so a sequence
// of standalone JSON objects becomes a JSON stream that json.Decoder can read.
func newLineReader(b []byte) *bytes.Reader {
	out := make([]byte, 0, len(b))
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 {
			continue
		}
		out = append(out, t...)
		out = append(out, ' ')
	}
	return bytes.NewReader(out)
}
