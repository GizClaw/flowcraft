package decode_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// FuzzToJSON: whatever the input, a successful conversion must be valid JSON.
func FuzzToJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"version":1}`),
		[]byte("version: 1\nagents:\n  - name: a\n"),
		[]byte("- a\n- b\n"),
		[]byte(""),
		{0xff, 0xfe},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		body, err := decode.ToJSON(data)
		if err != nil {
			return
		}
		if !json.Valid(body) {
			t.Fatalf("ToJSON returned invalid JSON for %q: %q", data, body)
		}
	})
}
