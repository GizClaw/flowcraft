package typesafe

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
)

// requestIDHeader is the response header the driver reads the provider's own
// request id from: every failure the route answers carries one
// (x-typesafe-request-id: req_…) and the vendor's SDK exposes it as the error's
// request id. gatewayRequestIDHeader is the conventional name a gateway in
// front of the route sets instead, read only when the vendor's is absent — a
// deployment behind a proxy keeps its correlation without the driver
// inventing an envelope. When neither header is present the failure carries no
// id, which is the truth about that response.
const (
	requestIDHeader        = "x-typesafe-request-id"
	gatewayRequestIDHeader = "x-request-id"
)

// failureBodyLimit bounds how much of a rejected body reaches the error message:
// enough to name the offending field, never enough to buffer a response the
// driver is discarding anyway.
const failureBodyLimit = 4096

// classifyFailure renders one non-2xx response as an errdefs-classified error
// and attaches the wire facts the runtime reads back off it.
//
// The four statuses the vendor publishes all land through the shared status
// mapping: 401 unauthorized, 422 validation, 429 rate limit, and 529 overloaded
// (docs.typesafe.ai/api). 529 is not a status errdefs names, so it falls
// through to the not-available default — which is the retryable class the
// vendor's own "back off and retry" advice asks for, and the one its SDK
// retries (408, 429, and every 5xx). Retry-After rides along because the
// vendor's rate-limit guidance is to honour it, and the attempt count comes
// from the httpkit retry transport so a trace can see how many sends a failure
// cost.
//
// The route answers a wrong key with 401 and a *missing* one with 403, where
// the vendor's status table names 401 for "missing or invalid API key". The
// wire's own behavior is what the mapping follows, so a wrong key classifies as
// unauthorized and the 403 a deployment cannot reach through this driver — a
// profile with no key fails when it opens — would classify as forbidden. Both
// are permanent, and both carry the vendor's message.
//
// The body rides along as it arrived, bounded, rather than being unpacked: the
// route's error envelope is two shapes — {"detail": {"error_type", "message"}}
// for API errors and {"detail": "Not Found"} for a route that does not exist —
// so a driver that parsed one shape would drop the message of the other.
// A caller reads the vendor's own words either way.
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
	if id := requestIDOf(response); id != "" {
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

// readResponseBody reads the body of an accepted response, at most limit bytes.
// It reports whether the route sent more than that: the surplus is not read, so
// the caller holds an incomplete body rather than an unbounded one. A read that
// fails outright is an error — the caller distinguishes a body that did not
// arrive whole (transient) from one that is only larger than this driver reads
// (a response that cannot be read as this contract).
func readResponseBody(body io.Reader, limit int64) ([]byte, bool, error) {
	payload, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(payload)) > limit {
		return payload[:limit], true, nil
	}
	return payload, false, nil
}

// requestIDOf reads the provider request id a response carries, if any: the
// vendor's own header first, then the conventional one a gateway sets.
func requestIDOf(response *http.Response) string {
	if id := response.Header.Get(requestIDHeader); id != "" {
		return id
	}
	return response.Header.Get(gatewayRequestIDHeader)
}
