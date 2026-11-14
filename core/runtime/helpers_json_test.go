package runtime

import "encoding/json"

// settingsJSON turns a Go literal into the JSON a settings block now is.
func settingsJSON(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	// A typed nil (a nil map inside an any) marshals to null; a settings block
	// that was never written is nil, not the word null.
	if string(body) == "null" {
		return nil
	}
	return body
}
