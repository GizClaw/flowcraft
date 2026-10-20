package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/httpkit"
)

// recordedCall is one wire call the fake route saw.
type recordedCall struct {
	method  string
	path    string
	auth    string
	ctype   string
	headers map[string]string
	body    map[string]any
}

// fakeRoute is a scripted System One route: it answers the same status, body,
// and headers to every call and records each request, so a test can assert the
// path, the credential, and the payload the driver sent. There is no live lane
// for this provider — no api_key is available — so this is the only place the
// wire shape is exercised.
type fakeRoute struct {
	mu      sync.Mutex
	calls   []recordedCall
	status  int
	body    string
	headers map[string]string
}

// server starts the fake behind an httptest server and closes it with the test.
func (f *fakeRoute) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return server
}

func (f *fakeRoute) serve(w http.ResponseWriter, r *http.Request) {
	call := recordedCall{
		method:  r.Method,
		path:    r.URL.Path,
		auth:    r.Header.Get(headerAuthorization),
		ctype:   r.Header.Get(headerContentType),
		headers: make(map[string]string, len(r.Header)),
	}
	for name := range r.Header {
		call.headers[name] = r.Header.Get(name)
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&call.body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	status, body, headers := f.status, f.body, f.headers
	f.mu.Unlock()

	for name, value := range headers {
		w.Header().Set(name, value)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeRoute) lastCall(t *testing.T) recordedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the fake route saw no call")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeRoute) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// testClients roots one profile's clients at the fake route's version root,
// with the credential a deployment would resolve. Some spec is inserted so a
// test that cares about deployment facts — static headers, retry budget —
// exercises the path that reads them.
func testClients(t *testing.T, route *fakeRoute, spec Spec) *clients {
	t.Helper()
	spec.Endpoint.BaseURL = route.server(t).URL + "/v1"
	material := profileMaterial{apiKey: resource.LiteralSecret("sk-test")}
	cls, err := material.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	return cls
}

// countingTransport wraps a transport and counts its calls, so the shared
// unary suite can assert that Explain never reaches the provider and Execute
// reaches it exactly once.
func countingTransport[Wire, Raw any](
	calls *atomic.Int64,
	inner inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Add(1)
		return inner(ctx, wire)
	}
}

// TestTransportSystemOneRoute pins the request the route receives: one POST to
// the version root's systemone path, authenticated with the profile's key, and
// carrying the compiled body.
func TestTransportSystemOneRoute(t *testing.T) {
	route := &fakeRoute{body: wireResponseFixture()}
	cls := testClients(t, route, Spec{})

	raw, err := transportDecision(cls)(
		context.Background(), compileForTest(t, decisionRequestFixture()))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.truncated || len(raw.placement) != 3 ||
		!strings.Contains(string(raw.body), `"jev-1.13.0"`) {
		t.Fatalf("raw = %+v", raw)
	}

	call := route.lastCall(t)
	if call.method != http.MethodPost {
		t.Errorf("method = %q, want POST", call.method)
	}
	if call.path != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", call.path)
	}
	if call.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the profile's key", call.auth)
	}
	if call.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", call.ctype)
	}
	if call.body["model"] != DefaultModel || call.body["state"] != evidenceFixture {
		t.Errorf("body = %+v", call.body)
	}
}

// TestTransportStaticHeaders checks the deployment's static headers ride every
// request without displacing the credential the profile owns.
func TestTransportStaticHeaders(t *testing.T) {
	route := &fakeRoute{body: wireResponseFixture()}
	cls := testClients(t, route, Spec{Endpoint: EndpointSpec{
		Headers: map[string]string{"X-Tenant": "acme"},
	}})

	if _, err := transportDecision(cls)(
		context.Background(), compileForTest(t, decisionRequestFixture())); err != nil {
		t.Fatalf("transport: %v", err)
	}
	call := route.lastCall(t)
	if call.headers["X-Tenant"] != "acme" {
		t.Errorf("X-Tenant = %q, want acme", call.headers["X-Tenant"])
	}
	if call.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the profile's key", call.auth)
	}
}

// TestTransportErrorClassification pins the mapping from the vendor's four
// failure statuses onto errdefs, and the wire facts the router and a trace read
// back off the failure: the Retry-After the vendor's rate-limit guidance asks
// callers to honour, and the provider request id when the response carries one.
//
// It drives the bound driver rather than the bare transport, so what it asserts
// is the failure a caller actually sees: the runtime wraps a classified
// transport error into ProviderFailure, and that wrap is what the router's
// retry and circuit-breaker policies read. Retries are disabled here so each
// case costs one send, which also keeps the Retry-After header from being slept
// on; which failures the transport retries, and the attempt count it stamps, is
// pinned separately in TestTransportRetryGrading.
func TestTransportErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{
			name:   "401 unauthorized",
			status: http.StatusUnauthorized,
			check:  errdefs.IsUnauthorized,
		},
		{
			// The route answers a *missing* key with 403 where the vendor's
			// status table names 401 for "missing or invalid API key"; the
			// wrong-key 401 above is what the table describes. Both are
			// permanent, and the vendor's own message rides along either
			// way.
			name:   "403 forbidden",
			status: http.StatusForbidden,
			check:  errdefs.IsForbidden,
		},
		{
			name:   "422 unprocessable",
			status: http.StatusUnprocessableEntity,
			check:  errdefs.IsValidation,
		},
		{
			name:   "429 too many requests",
			status: http.StatusTooManyRequests,
			check:  errdefs.IsRateLimit,
		},
		{
			// 529 is not a status errdefs names, so it lands in the
			// not-available default — which is the retryable class the
			// vendor's own "back off and retry" advice asks for.
			name:   "529 overloaded",
			status: 529,
			check:  errdefs.IsNotAvailable,
		},
		{
			name:   "503 service unavailable",
			status: http.StatusServiceUnavailable,
			check:  errdefs.IsNotAvailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := &fakeRoute{
				status: tc.status,
				body:   `{"error": "the request body failed validation"}`,
				headers: map[string]string{
					"Retry-After":   "2",
					requestIDHeader: "req-7",
				},
			}
			// The driver the runtime binds, not the transport: the runtime
			// is what turns a classified transport error into the
			// ProviderFailure the assertion below reads.
			retries := resource.Int(0)
			cls := testClients(t, route, Spec{HTTPRetries: &retries})
			ref := typesafeModel(DefaultModel)
			driver, err := openDecision(cls, ref.ID)
			if err != nil {
				t.Fatalf("openDecision: %v", err)
			}

			_, err = driver.Execute(
				context.Background(), ref, decisionRequestFixture())
			if err == nil {
				t.Fatal("transport succeeded, want a classified failure")
			}
			if !inference.IsKind(err, inference.ProviderFailure) {
				t.Fatalf("error = %v, want a ProviderFailure", err)
			}
			if !tc.check(err) {
				t.Fatalf("error = %v, want the %s class", err, tc.name)
			}
			// The runtime's own message names the class; the provider's
			// detail rides the cause its classification wrapped, which is
			// where a failed-call log and a trace read it.
			var failure *inference.Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %T %v, want the runtime's *inference.Error", err, err)
			}
			if cause := failure.Unwrap(); cause == nil ||
				!strings.Contains(cause.Error(), "the request body failed validation") {
				t.Errorf("cause = %v, want the provider body in the message", cause)
			}
			if after, ok := errdefs.RetryAfter(err); !ok || after != 2*time.Second {
				t.Errorf("Retry-After = %v (%v), want 2s", after, ok)
			}
			if id, ok := errdefs.RequestID(err); !ok || id != "req-7" {
				t.Errorf("request id = %q (%v), want req-7", id, ok)
			}
		})
	}
}

// TestTransportRetryGrading pins which failures the transport retries, and the
// attempt count it stamps on the response: a permanent failure costs one send,
// while a throttled or unavailable route is retried up to the budget httpkit
// defaults to. The two go together — the count is what a trace reports as
// wire_attempts, so it has to agree with the sends the route saw.
func TestTransportRetryGrading(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantSends int
	}{
		{"401 unauthorized is permanent", http.StatusUnauthorized, 1},
		{"422 unprocessable is permanent", http.StatusUnprocessableEntity, 1},
		{"429 too many requests is retried", http.StatusTooManyRequests, 3},
		// 529 is the vendor's own overload status and not one errdefs names:
		// it reaches the retry transport as a 5xx, which is what its
		// "back off and retry" guidance asks for.
		{"529 overloaded is retried", 529, 3},
		{"503 service unavailable is retried", http.StatusServiceUnavailable, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No Retry-After header: the backoff falls back to the
			// transport's jittered exponential, which is what keeps this
			// test from sleeping on a server-supplied hint.
			route := &fakeRoute{status: tc.status, body: "{}"}
			cls := testClients(t, route, Spec{})

			_, err := transportDecision(cls)(
				context.Background(), compileForTest(t, decisionRequestFixture()))
			if err == nil {
				t.Fatal("transport succeeded, want a failure")
			}
			if sends := route.callCount(); sends != tc.wantSends {
				t.Errorf("route saw %d sends, want %d", sends, tc.wantSends)
			}
			if count := errdefs.RetryCount(err); count != tc.wantSends {
				t.Errorf("wire attempts = %d, want the %d sends the route saw",
					count, tc.wantSends)
			}
		})
	}
}

// TestTransportRetryBudget checks the deployment's http_retries reaches the
// transport: zero disables wire retries, so a thrice-failing route is called
// exactly once.
func TestTransportRetryBudget(t *testing.T) {
	route := &fakeRoute{status: http.StatusServiceUnavailable, body: "{}"}
	retries := resource.Int(0)
	cls := testClients(t, route, Spec{HTTPRetries: &retries})

	if _, err := transportDecision(cls)(
		context.Background(), compileForTest(t, decisionRequestFixture())); err == nil {
		t.Fatal("transport succeeded, want a failure")
	}
	if calls := route.callCount(); calls != 1 {
		t.Errorf("route saw %d calls, want 1 with retries disabled", calls)
	}
}

// TestTransportReadsTheRequestIDTheRouteSets pins where a failure's request id
// comes from now that the vendor's own header name is known: the route sets
// x-typesafe-request-id on every failure it answers, and a deployment behind a
// proxy may see the conventional x-request-id instead. Reading only the
// conventional name leaves the failures the route itself answered without the
// id that correlates them.
func TestTransportReadsTheRequestIDTheRouteSets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "the route's own header",
			headers: map[string]string{requestIDHeader: "req_01a134f6"},
			want:    "req_01a134f6",
		},
		{
			name:    "a gateway's header",
			headers: map[string]string{gatewayRequestIDHeader: "gw-7"},
			want:    "gw-7",
		},
		{
			name: "both, and the route's wins",
			headers: map[string]string{
				requestIDHeader:        "req_01a134f6",
				gatewayRequestIDHeader: "gw-7",
			},
			want: "req_01a134f6",
		},
		{
			name:    "neither, and the failure carries none",
			headers: nil,
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := &fakeRoute{
				status:  http.StatusUnauthorized,
				body:    `{"detail":{"error_type":"authentication_error"}}`,
				headers: tc.headers,
			}
			cls := testClients(t, route, Spec{})
			_, err := transportDecision(cls)(
				context.Background(), compileForTest(t, decisionRequestFixture()))
			if err == nil {
				t.Fatal("transport succeeded, want the route's failure")
			}
			id, ok := errdefs.RequestID(err)
			if tc.want == "" {
				if ok {
					t.Errorf("request id = %q, want none", id)
				}
				return
			}
			if !ok || id != tc.want {
				t.Errorf("request id = %q (%v), want %q", id, ok, tc.want)
			}
		})
	}
}

// TestTransportFailedExchangeIsRetryable pins the classification of a failure
// that never reached the route: the transport saw no answer, so the failure is
// transient by construction and the router may retry it.
func TestTransportFailedExchangeIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	dead := server.URL
	server.Close()

	cls := &clients{
		http: httpkit.NewHTTPClient(),
		key:  "sk-test",
		base: dead + "/v1",
	}
	_, err := transportDecision(cls)(
		context.Background(), compileForTest(t, decisionRequestFixture()))
	if err == nil {
		t.Fatal("transport succeeded against a closed route")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("error = %v, want a not-available failure", err)
	}
}

// TestTransportLeavesUnreadableAnswersToTheDecoder pins which stage classifies
// what, because the class is what decides whether a paid call is sent twice.
//
// The transport keeps the failures a retry can fix: an exchange that failed and
// a body that did not arrive whole. A 200 whose body cannot be read as this
// contract is the decoder's, and core classifies it as an invalid provider
// response — the class the runtime does not retry, since the retry would buy
// the same body again. The vendor's own SDK draws the line in the same place:
// it retries on status, connection, and timeout, and reports a successful
// response whose body is structurally invalid as a response-validation error.
func TestTransportLeavesUnreadableAnswersToTheDecoder(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "a body that is not JSON",
			body: `{"model": `,
		},
		{
			name: "an answer to a question the request never asked",
			body: `{"model":"jev-1.13.0","answers":` +
				`{"sentiment":{"type":"noul","noul":0.5}},` +
				`"usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			name: "an answer with no body at all",
			body: ``,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := &fakeRoute{body: tc.body}
			cls := testClients(t, route, Spec{})
			ref := typesafeModel(DefaultModel)
			driver, err := openDecision(cls, ref.ID)
			if err != nil {
				t.Fatalf("openDecision: %v", err)
			}
			_, err = driver.Execute(
				context.Background(), ref, decisionRequestFixture())
			if !inference.IsKind(err, inference.InvalidProviderResponse) {
				t.Fatalf("error = %v, want an invalid provider response", err)
			}
			if errdefs.IsNotAvailable(err) {
				t.Errorf("error = %v, want a failure the router does not retry", err)
			}
			// The vendor's own reading of the failure reaches the caller:
			// the message says which answer could not be read.
			var failure *inference.Error
			if !errors.As(err, &failure) || failure.Unwrap() == nil {
				t.Fatalf("error = %v, want the decoder's cause attached", err)
			}
		})
	}
}

// TestTransportKeepsAnIncompleteBodyRetryable pins the other half of that
// split: a body that did not arrive whole never reached the driver as an
// answer, so it is transient by construction and the router may retry it.
func TestTransportKeepsAnIncompleteBodyRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			// A length the handler never delivers: the read fails.
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model": "jev`))
		}))
	t.Cleanup(server.Close)

	cls := &clients{
		http: httpkit.NewHTTPClient(),
		key:  "sk-test",
		base: server.URL + "/v1",
	}
	_, err := transportDecision(cls)(
		context.Background(), compileForTest(t, decisionRequestFixture()))
	if err == nil {
		t.Fatal("transport succeeded on a body that did not arrive whole")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("error = %v, want a not-available failure", err)
	}
}
