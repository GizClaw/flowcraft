package anthropic

import anthropicgo "github.com/anthropics/anthropic-sdk-go"

// client is the SDK client for one deployment. It is a value, not an
// interface: the SDK is transport-injectable through option.WithHTTPClient,
// which is how tests point it at a local server.
type client = anthropicgo.Client

func newClient(settings Settings) client {
	return anthropicgo.NewClient(settings.options()...)
}
