package server

import "encoding/json"

// settingsJSON turns a Go literal into the JSON a settings block now is.
func settingsJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
