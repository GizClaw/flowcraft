package openai

import (
	"encoding/json"

	"github.com/openai/openai-go/v3/option"
)

// bodyOptions turns the request's injected raw fields into a request-body
// override. Without them the typed request goes out untouched, exactly as the SDK
// built it, so the escape hatch costs nothing on the common path.
func bodyOptions(params any, extra map[string]json.RawMessage) ([]option.RequestOption, error) {
	if len(extra) == 0 {
		return nil, nil
	}
	base, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(base, &object); err != nil {
		return nil, err
	}
	for key, value := range extra {
		object[key] = value
	}
	merged, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return []option.RequestOption{option.WithRequestBody("application/json", merged)}, nil
}
