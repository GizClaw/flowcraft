package openai

import "github.com/openai/openai-go/v3"

// client is the SDK client for one deployment. It is a value, not an interface:
// the SDK is already transport-injectable through option.WithHTTPClient, which is
// how tests point it at a local server.
type openaiClient = openai.Client

// newClient builds the SDK client from the deployment settings.
func newClient(settings Settings) openaiClient {
	return openai.NewClient(settings.options()...)
}
