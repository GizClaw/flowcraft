package dashscope

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The platform answers request-level failures with an HTTP status plus a
// top-level {code, message, request_id} envelope, and task-level failures
// with a terminal task_status plus output.code/output.message. The code
// vocabulary is documented per product; the mapping below covers the
// families the driver can meet and lets everything else fall back to the
// HTTP status, so an unknown code degrades to the transport truth rather
// than to a wrong classification. A model the platform does not know is not
// a code of its own: it is refused as InvalidParameter with "Model not
// exist.", which the invalid-parameter case covers.
const (
	codeInvalidKey      = "InvalidApiKey"
	codeArrearage       = "Arrearage"
	codeAccessDenied    = "AccessDenied"
	codeThrottling      = "Throttling"
	codeInvalidParam    = "InvalidParameter"
	codeDataInspection  = "DataInspectionFailed"
	codeIPInfringement  = "IPInfringementSuspect"
	codeInvalidTask     = "InvalidTaskId"
	codeInternalError   = "InternalError"
	codeServiceUnready  = "ServiceUnavailable"
	codeUnsupportedMode = "UnsupportedModel"
)

// errorBody is the platform's error envelope. The fields are shared by
// request-level failures, task-creation failures, and terminal task
// failures; which of them is populated depends on where the failure landed.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// failure renders the envelope as an error, classified through errdefs. The
// HTTP status is the fallback classification, so a code this driver does not
// know still lands in the right family when the status is expressive.
func (b errorBody) failure(operation string, status int) error {
	detail := strings.TrimSpace(b.Message)
	if detail == "" {
		detail = "no message"
	}
	base := fmt.Errorf(
		"dashscope: %s failed: %s: %s",
		operation,
		nonEmpty(b.Code, fmt.Sprintf("HTTP %d", status)),
		detail,
	)
	classified := classifyCode(b.Code, base)
	if b.RequestID != "" {
		classified = errdefs.WithRequestID(classified, b.RequestID)
	}
	return classified
}

// classifyCode maps a platform code onto an errdefs class.
func classifyCode(code string, err error) error {
	switch {
	case strings.HasPrefix(code, codeInvalidKey):
		return errdefs.Unauthorized(err)
	case strings.HasPrefix(code, codeArrearage):
		return errdefs.Forbidden(err)
	case strings.HasPrefix(code, codeAccessDenied):
		// AccessDenied covers an account that has not opened a model and a
		// call the endpoint refuses on protocol grounds — a synchronous call
		// on a task surface answers AccessDenied even though the request was
		// well formed. Both are "this caller may not do that".
		return errdefs.Forbidden(err)
	case strings.HasPrefix(code, codeThrottling):
		return errdefs.RateLimit(err)
	case strings.HasPrefix(code, codeInvalidParam),
		strings.HasPrefix(code, codeDataInspection),
		strings.HasPrefix(code, codeIPInfringement),
		strings.HasPrefix(code, codeInvalidTask),
		strings.HasPrefix(code, codeUnsupportedMode):
		return errdefs.Validation(err)
	case strings.HasPrefix(code, codeInternalError),
		strings.HasPrefix(code, codeServiceUnready):
		return errdefs.NotAvailable(err)
	default:
		return err
	}
}

// classifyStatus classifies a transport failure by HTTP status, then refines
// it with the envelope's code when one is present.
func classifyStatus(status int, code string, err error) error {
	classified := errdefs.ClassifyStatus(status, err)
	refined := classifyCode(code, classified)
	return refined
}

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
