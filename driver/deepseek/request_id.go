package deepseek

import (
	"net/http"

	"github.com/openai/openai-go/v3/option"
)

// captureRequestID returns a per-call request option whose middleware
// records the provider's request-id response header. The middleware runs on
// every HTTP attempt, so the last attempt's header wins. DeepSeek echoes
// the request identifier as x-request-id; apim-request-id covers
// Azure-backed deployments of the same protocol.
func captureRequestID(target *string) option.RequestOption {
	return option.WithMiddleware(func(
		req *http.Request,
		next option.MiddlewareNext,
	) (*http.Response, error) {
		response, err := next(req)
		if err != nil || response == nil {
			return response, err
		}
		if id := response.Header.Get("x-request-id"); id != "" {
			*target = id
		} else if id := response.Header.Get("apim-request-id"); id != "" {
			*target = id
		}
		return response, err
	})
}
