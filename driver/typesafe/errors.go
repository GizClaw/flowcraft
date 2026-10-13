package typesafe

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
)

// requestIDHeader is the response header the driver reads a provider request id
// from. The vendor's reference documents four failure statuses and no id header,
// so the driver reads the conventional name a gateway in front of the route
// sets rather than inventing an envelope: when it is absent the failure simply
// carries no id, which is the truth about that response.
const requestIDHeader = "x-request-id"

// failureBodyLimit bounds how much of a rejected body reaches the error message:
// enough to name the offending field, never enough to buffer a response the
// driver is discarding anyway.
const failureBodyLimit = 4096

// classifyFailure renders one non-2xx response as an errdefs-classified error
// and attaches the wire facts the runtime reads back off it.
//
// The four statuses the vendor publishes all land through the shared status
// mapping: 401 unauthorized, 422 validation, 429 rate limit, and 529 overloaded.
// 529 is not a status errdefs names, so it falls through to the not-available
// default — which is the retryable class the vendor's own "back off and retry"
// advice asks for. Retry-After rides along because the vendor's rate-limit
// guidance is to honour it, and the attempt count comes from the httpkit retry
// transport so a trace can see how many sends a failure cost.
func classifyFailure(response *http.Response, body []byte) error {
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = "no body"
	}
	classified := errdefs.ClassifyStatus(
		response.StatusCode,
		fmt.Errorf(
			"typesafe: system one failed: HTTP %d: %s",
			response.StatusCode,
			detail,
		),
	)
	if id := response.Header.Get(requestIDHeader); id != "" {
		classified = errdefs.WithRequestID(classified, id)
	}
	classified = errdefs.WithRetryAfter(
		classified,
		errdefs.ParseRetryAfter(response.Header.Get("Retry-After")),
	)
	return errdefs.WithRetryCount(classified, utils.RetryCountOf(response))
}

// readFailureBody reads a bounded slice of a rejected body.
func readFailureBody(body io.Reader) []byte {
	snippet, _ := io.ReadAll(io.LimitReader(body, failureBodyLimit))
	return snippet
}

// requestIDOf reads the provider request id a response carries, if any.
func requestIDOf(response *http.Response) string {
	return response.Header.Get(requestIDHeader)
}
