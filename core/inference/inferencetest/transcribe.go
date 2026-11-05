package inferencetest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// TranscriberFixture is what a transcription driver hands the suite: a way to
// build the transcriber against a base URL, and canned answers to assert on.
type TranscriberFixture struct {
	// ProviderID is the driver's identity.
	ProviderID string
	// NewTranscriber builds a transcriber pointed at baseURL. It must not
	// perform I/O during construction.
	NewTranscriber func(t *testing.T, baseURL string) (inference.Transcriber, error)
	// Request builds the request used for every sample.
	Request func(t *testing.T) inference.TranscribeRequest
	// Samples are the canned provider answers.
	Samples []TranscribeSample
}

// TranscribeSample is one canned answer and what it must decode to.
type TranscribeSample struct {
	Name string
	// Status is the HTTP status to serve. Zero means 200.
	Status int
	// Body is the raw provider payload.
	Body string
	// WantKind, when set, asserts the call fails with that classified kind
	// instead of producing a response.
	WantKind inference.ErrorKind
	// WantText and WantSegments describe the successful answer.
	WantText     string
	WantSegments int
}

// RunTranscriber runs the transcription conformance suite against a real HTTP
// endpoint, so the assertions cover the driver's own decoding and status
// mapping rather than a mock of them.
func RunTranscriber(t *testing.T, fixture TranscriberFixture) {
	t.Helper()
	if fixture.NewTranscriber == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: transcriber fixture needs NewTranscriber and Request")
	}
	if len(fixture.Samples) == 0 {
		t.Fatalf("inferencetest: transcriber fixture needs at least one sample")
	}

	for _, sample := range fixture.Samples {
		t.Run(sample.Name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				status := sample.Status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(sample.Body))
			}))
			t.Cleanup(server.Close)

			transcriber, err := fixture.NewTranscriber(t, server.URL)
			if err != nil {
				t.Fatalf("NewTranscriber: %v", err)
			}
			response, err := transcriber.Transcribe(context.Background(), fixture.Request(t))

			if sample.WantKind != "" {
				if err == nil {
					t.Fatalf("Transcribe = nil error, want kind %q", sample.WantKind)
				}
				if !inference.IsKind(err, sample.WantKind) {
					t.Fatalf("Transcribe kind = %v, want %q", err, sample.WantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if response.Text != sample.WantText {
				t.Errorf("Text = %q, want %q", response.Text, sample.WantText)
			}
			if len(response.Segments) != sample.WantSegments {
				t.Errorf("Segments = %d, want %d", len(response.Segments), sample.WantSegments)
			}
		})
	}
}
