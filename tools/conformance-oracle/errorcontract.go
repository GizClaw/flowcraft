package main

// The error contract: what core/errdefs answers about classification, dumped
// so the port can be held to it.
//
// The decode dump in main.go is driven by a corpus of documents. This one is
// driven by tables of *questions*: a kind and the HTTP status it answers with,
// a chain of kinds and the status core picks out of it, a provider error's
// text and the category core reads into it. Every answer below is computed by
// calling errdefs, never restated -- regenerating after a change to core is
// how the committed file moves, and that movement is the drift the gate is
// meant to show.
//
// The rows are deliberately longer than the happy path. Two of the samples
// pin differences between the forward mapping (an error that knows its kind →
// HTTPStatus) and the reverse ones (a status or a provider error → a kind):
// 409 is a Conflict going forward but has no provider category coming back,
// and 408/504 are Timeouts going forward but no categories coming back. The
// other rows pin the parts of the classifiers that are easy to get almost
// right: the priority order HTTPStatus scans in (which is by category, not by
// depth in the chain), the keyword table's order (context overflow before the
// plain "http 400" it sits next to), and the shape of the "\b(?:http|status)
// \s*(\d{3})\b" scan over the lowercased chain text.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// contractNote is part of the committed file, so it is written for whoever
// opens that file rather than for whoever runs this command.
const contractNote = "The error contract core/errdefs answers, dumped for the port. " +
	"Names identify rows: core spells neither these kinds nor these categories on the wire today. " +
	"Regenerate with: (cd tools/conformance-oracle && GOWORK=off go run . -error-contract) > rust/conformance/error-contract.json"

// errorContract is the document -error-contract prints. The field order is
// the reading order and does not change: the file is committed and diffed.
type errorContract struct {
	Note                         string                  `json:"note"`
	Kinds                        []kindRow               `json:"kinds"`
	HTTPStatusPriority           []priorityRow           `json:"http_status_priority"`
	ProviderCategoryFromHTTPCode []codeRow               `json:"provider_category_from_http_code"`
	ClassifyStatus               []classifyStatusRow     `json:"classify_status"`
	ClassifyHTTPStatus           []classifyHTTPStatusRow `json:"classify_http_status"`
	ClassifyProviderError        []classifyProviderRow   `json:"classify_provider_error"`
	ClassifyProvider             []providerRow           `json:"classify_provider"`
}

type kindRow struct {
	Name       string `json:"name"`
	HTTPStatus int    `json:"http_status"`
}

type priorityRow struct {
	Label  string   `json:"label"`
	Chain  []string `json:"chain"`
	Status int      `json:"status"`
}

type codeRow struct {
	Code     int    `json:"code"`
	Category string `json:"category"`
	Mapped   bool   `json:"mapped"`
}

// classifyStatusRow is ClassifyStatus's answer for one code: the kind that
// decides and the text the classified error prints, which stays the cause's
// own -- the wrapper is transparent.
type classifyStatusRow struct {
	Label   string `json:"label"`
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type classifyHTTPStatusRow struct {
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Code     int    `json:"code"`
	Body     string `json:"body"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
}

// innerSpec describes the error handed to ClassifyProviderError: a text, an
// optional classification, and an optional wrapper around both. A pointer for
// the two optionals so the zero value stays "absent" in the JSON.
type innerSpec struct {
	Kind   *string `json:"kind"`
	Prefix *string `json:"prefix"`
	Text   string  `json:"text"`
}

type classifyProviderRow struct {
	Label    string    `json:"label"`
	Provider string    `json:"provider"`
	Inner    innerSpec `json:"inner"`
	Kind     string    `json:"kind"`
	Message  string    `json:"message"`
}

type providerRow struct {
	Label    string   `json:"label"`
	Chain    []string `json:"chain"`
	Category string   `json:"category"`
}

// kindSpec names a classification and says how to put it on an error and how
// to look for it. The order is core/errdefs.HTTPStatus's own: the first
// marker that matches decides the status, and the same order answers "which
// kind does this error carry" when a chain carries more than one.
type kindSpec struct {
	name string
	wrap func(error) error
	is   func(error) bool
}

var kindSpecs = []kindSpec{
	{"not_found", errdefs.NotFound, errdefs.IsNotFound},
	{"validation", errdefs.Validation, errdefs.IsValidation},
	{"unauthorized", errdefs.Unauthorized, errdefs.IsUnauthorized},
	{"forbidden", errdefs.Forbidden, errdefs.IsForbidden},
	{"conflict", errdefs.Conflict, errdefs.IsConflict},
	{"rate_limit", errdefs.RateLimit, errdefs.IsRateLimit},
	{"budget_exceeded", errdefs.BudgetExceeded, errdefs.IsBudgetExceeded},
	{"policy_denied", errdefs.PolicyDenied, errdefs.IsPolicyDenied},
	{"timeout", errdefs.Timeout, errdefs.IsTimeout},
	{"not_available", errdefs.NotAvailable, errdefs.IsNotAvailable},
	{"internal", errdefs.Internal, errdefs.IsInternal},
	{"interrupted", errdefs.Interrupted, errdefs.IsInterrupted},
	{"aborted", errdefs.Aborted, errdefs.IsAborted},
}

type categorySpec struct {
	name     string
	category errdefs.ProviderCategory
}

// categorySpecs is in ProviderCategory's declaration order (the iota values
// are part of the enum's shape, even where nothing renders them).
var categorySpecs = []categorySpec{
	{"transient", errdefs.ProviderTransient},
	{"rate_limit", errdefs.ProviderRateLimit},
	{"auth", errdefs.ProviderAuth},
	{"billing", errdefs.ProviderBilling},
	{"context_overflow", errdefs.ProviderContextOverflow},
	{"permanent", errdefs.ProviderPermanent},
}

func kindSpecOf(name string) (kindSpec, error) {
	for _, spec := range kindSpecs {
		if spec.name == name {
			return spec, nil
		}
	}
	return kindSpec{}, fmt.Errorf("unknown kind %q", name)
}

func categoryName(category errdefs.ProviderCategory) (string, error) {
	for _, spec := range categorySpecs {
		if spec.category == category {
			return spec.name, nil
		}
	}
	return "", fmt.Errorf("unknown category %d", int(category))
}

func categorySpecOf(name string) (categorySpec, error) {
	for _, spec := range categorySpecs {
		if spec.name == name {
			return spec, nil
		}
	}
	return categorySpec{}, fmt.Errorf("unknown category %q", name)
}

// kindNameOf names the classification kindNameOf finds first, in
// HTTPStatus's scan order. An error carrying no errdefs marker answers "".
func kindNameOf(err error) string {
	for _, spec := range kindSpecs {
		if spec.is(err) {
			return spec.name
		}
	}
	return ""
}

// nest builds the chain a row names: chain[0] is outermost, an empty chain is
// a plain error. The wrappers are transparent, so every text stays "probe".
func nest(chain []string) (error, error) {
	var err error = errors.New("probe")
	for i := len(chain) - 1; i >= 0; i-- {
		spec, specErr := kindSpecOf(chain[i])
		if specErr != nil {
			return nil, specErr
		}
		err = spec.wrap(err)
	}
	return err, nil
}

// chainError carries one chain element's text, so a sample can say what each
// element says without going through a formatter that would rewrite it.
type chainError struct {
	msg  string
	next error
}

func (e *chainError) Error() string { return e.msg }
func (e *chainError) Unwrap() error { return e.next }

// buildChain builds chain[0] as the outermost element.
func buildChain(chain []string) error {
	var err error
	for i := len(chain) - 1; i >= 0; i-- {
		err = &chainError{msg: chain[i], next: err}
	}
	return err
}

func buildInner(spec innerSpec) (error, error) {
	var err error = errors.New(spec.Text)
	if spec.Kind != nil {
		k, kindErr := kindSpecOf(*spec.Kind)
		if kindErr != nil {
			return nil, kindErr
		}
		err = k.wrap(err)
	}
	if spec.Prefix != nil {
		err = fmt.Errorf("%s: %w", *spec.Prefix, err)
	}
	return err, nil
}

func str(s string) *string { return &s }

// writeErrorContract prints the document. It returns an error rather than
// exiting so the tests can hold it to its own invariants.
func writeErrorContract(w io.Writer) error {
	doc := errorContract{Note: contractNote}

	for _, spec := range kindSpecs {
		doc.Kinds = append(doc.Kinds, kindRow{
			Name:       spec.name,
			HTTPStatus: errdefs.HTTPStatus(spec.wrap(errors.New("probe"))),
		})
	}

	for _, row := range priorityRows {
		nested, nestErr := nest(row.chain)
		if nestErr != nil {
			return fmt.Errorf("http_status_priority %q: %w", row.label, nestErr)
		}
		doc.HTTPStatusPriority = append(doc.HTTPStatusPriority, priorityRow{
			Label:  row.label,
			Chain:  row.chain,
			Status: errdefs.HTTPStatus(nested),
		})
	}

	for _, code := range codeTable {
		category, ok := errdefs.ProviderCategoryFromHTTPCode(code)
		name, nameErr := categoryName(category)
		if nameErr != nil {
			return fmt.Errorf("provider_category_from_http_code %d: %w", code, nameErr)
		}
		doc.ProviderCategoryFromHTTPCode = append(doc.ProviderCategoryFromHTTPCode,
			codeRow{Code: code, Category: name, Mapped: ok})
	}

	for _, code := range statusTable {
		classified := errdefs.ClassifyStatus(code, errors.New("provider said no"))
		doc.ClassifyStatus = append(doc.ClassifyStatus, classifyStatusRow{
			Label:   fmt.Sprintf("status %d", code),
			Code:    code,
			Kind:    kindNameOf(classified),
			Message: classified.Error(),
		})
	}

	for _, row := range httpStatusRows {
		classified := errdefs.ClassifyHTTPStatus(row.provider, row.code, row.body)
		doc.ClassifyHTTPStatus = append(doc.ClassifyHTTPStatus, classifyHTTPStatusRow{
			Label:    row.label,
			Provider: row.provider,
			Code:     row.code,
			Body:     row.body,
			Kind:     kindNameOf(classified),
			Message:  classified.Error(),
		})
	}

	for _, row := range providerErrorRows {
		inner, innerErr := buildInner(row.inner)
		if innerErr != nil {
			return fmt.Errorf("classify_provider_error %q: %w", row.label, innerErr)
		}
		classified := errdefs.ClassifyProviderError(row.provider, inner)
		doc.ClassifyProviderError = append(doc.ClassifyProviderError, classifyProviderRow{
			Label:    row.label,
			Provider: row.provider,
			Inner:    row.inner,
			Kind:     kindNameOf(classified),
			Message:  classified.Error(),
		})
	}

	for _, row := range providerRows {
		category, categoryErr := categoryName(errdefs.ClassifyProvider(buildChain(row.chain)))
		if categoryErr != nil {
			return fmt.Errorf("classify_provider %q: %w", row.label, categoryErr)
		}
		doc.ClassifyProvider = append(doc.ClassifyProvider, providerRow{
			Label:    row.label,
			Chain:    row.chain,
			Category: category,
		})
	}

	// HTML escaping off: the note carries the regeneration command, and a
	// committed file that says "\\u0026\\u0026" where it means "&&" is worse
	// documentation than JSON strictly requires.
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(doc)
}

// priorityRows are chains whose answer is decided by HTTPStatus's scan order
// rather than by the outermost kind. Every one of them names its expectation
// in the label so a diff reads without the table beside it.
var priorityRows = []struct {
	label string
	chain []string
}{
	{"plain error", []string{}},
	{"validation wraps not_found", []string{"validation", "not_found"}},
	{"not_found wraps validation", []string{"not_found", "validation"}},
	{"rate_limit wraps validation", []string{"rate_limit", "validation"}},
	{"aborted wraps rate_limit", []string{"aborted", "rate_limit"}},
	{"internal wraps budget_exceeded", []string{"internal", "budget_exceeded"}},
	{"policy_denied wraps conflict", []string{"policy_denied", "conflict"}},
	{"forbidden wraps policy_denied", []string{"forbidden", "policy_denied"}},
	{"timeout wraps not_available", []string{"timeout", "not_available"}},
	{"interrupted wraps aborted", []string{"interrupted", "aborted"}},
	{"aborted wraps internal wraps not_found", []string{"aborted", "internal", "not_found"}},
}

// codeTable is the reverse mapping's input domain: the codes it claims, the
// ones adjacent to those claims, and the two that only the forward direction
// knows (409 conflict, 408 timeout).
var codeTable = []int{200, 301, 400, 401, 402, 403, 404, 405, 408, 409, 418, 422, 429, 499, 500, 503, 504}

// statusTable is ClassifyStatus's own switch: the codes it names, plus the
// two that read differently from the same status through ClassifyHTTPStatus
// (409, 408/504), plus codes it has to answer something for anyway (200, 301,
// 418).
var statusTable = []int{200, 301, 400, 401, 402, 403, 404, 405, 408, 409, 418, 422, 429, 500, 503, 504}

var httpStatusRows = []struct {
	label    string
	provider string
	code     int
	body     string
}{
	{"429 bare", "openai", 429, ""},
	{"429 with body", "openai", 429, "slow down"},
	{"401", "ollama", 401, "body"},
	{"403", "ollama", 403, "body"},
	{"402", "ollama", 402, "body"},
	{"400", "ollama", 400, "body"},
	{"404", "ollama", 404, "body"},
	{"422", "ollama", 422, "body"},
	{"500", "ollama", 500, "body"},
	{"503", "ollama", 503, ""},
	{"301 is below 400", "ollama", 301, ""},
	{"200 is below 400", "ollama", 200, ""},
	{"418 is unknown", "ollama", 418, "body"},
	{"409 has no category", "ollama", 409, "body"},
	{"499 is unknown", "ollama", 499, ""},
	{"504 has no category", "ollama", 504, ""},
	{"408 has no category", "openai", 408, ""},
}

var providerErrorRows = []struct {
	label    string
	provider string
	inner    innerSpec
}{
	{"plain text", "openai", innerSpec{Text: "connection refused"}},
	{"auth keyword", "openai", innerSpec{Text: "invalid api key provided"}},
	{"billing keyword", "openai", innerSpec{Text: "insufficient credits"}},
	{"context overflow keyword", "openai", innerSpec{Text: "maximum context length exceeded"}},
	{"embedded code", "openai", innerSpec{Text: "got http 429"}},
	{"code with no category", "openai", innerSpec{Text: "status 503"}},
	{"already classified", "openai", innerSpec{Kind: str("timeout"), Text: "openai.generate: 30s"}},
	{"classified under a wrapper", "openai", innerSpec{Kind: str("rate_limit"), Prefix: str("wrapped"), Text: "rate limit hit"}},
	{"keyword beats code", "openai", innerSpec{Text: "http 400: maximum context length exceeded"}},
}

var providerRows = []struct {
	label string
	chain []string
}{
	{"auth keyword", []string{"invalid api key provided"}},
	{"auth keyword with underscore", []string{"invalid_api_key"}},
	{"auth keyword in caps", []string{"Invalid API Key"}},
	{"billing keyword", []string{"insufficient_quota"}},
	{"rate limit keyword", []string{"provider said: rate limit exceeded"}},
	{"resource_exhausted", []string{"resource_exhausted"}},
	{"overloaded", []string{"server is currently overloaded"}},
	{"context overflow keyword", []string{"maximum context length exceeded"}},
	{"max_tokens keyword", []string{"max_tokens must be at most 4096"}},
	{"overflow beats http 400", []string{"http 400: maximum context length exceeded"}},
	{"auth beats http 400", []string{"http 400: unauthorized access"}},
	{"embedded http code", []string{"got http 429"}},
	{"embedded status code", []string{"status 401 from upstream"}},
	{"uppercase is folded", []string{"GOT HTTP 429"}},
	{"http 400 alone is permanent", []string{"api error: http 400"}},
	{"httpx is not http", []string{"httpx 429"}},
	{"four digits are not a code", []string{"http 4291"}},
	{"estatus is not status", []string{"estatus 429"}},
	{"tab counts as space", []string{"http\t429"}},
	{"newline counts as space", []string{"http\n429"}},
	{"vertical tab is not space", []string{"http\v429"}},
	{"no space needed", []string{"http429"}},
	{"the join is a space", []string{"context length", "exceeded"}},
	{"chain texts are joined", []string{"outer", "rate limit exceeded"}},
	{"connection refused", []string{"connection refused"}},
	{"empty text", []string{""}},
}
