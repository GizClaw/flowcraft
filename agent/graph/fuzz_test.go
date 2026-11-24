package graph

import (
	"testing"

	"github.com/goccy/go-json"
)

// FuzzStateEvent: a checkpoint event is bytes from disk; decoding one must
// never panic, whatever the payload claims.
func FuzzStateEvent(f *testing.F) {
	f.Add([]byte(`{"step":1,"key":"x","value":{"kind":"text","text":"hi"}}`))
	f.Add([]byte(`{"step":2,"key":"m","value":{"kind":"messages","messages":[]}}`))
	f.Add([]byte(`{"step":0,"key":"","value":{"kind":"unknown"}}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		var event stateEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return
		}
		if _, err := decodeValue(event.Value); err != nil {
			return
		}
	})
}
