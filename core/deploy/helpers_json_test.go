package deploy

import "encoding/json"

// rawJSON turns a Go literal into the JSON a settings block now is.
func rawJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
