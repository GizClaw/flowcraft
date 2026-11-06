package inferencetest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve answers every request with one canned body and removes itself when the
// test ends.
//
// Every suite needs exactly this, and it is the part that is easy to get subtly
// wrong — a missing content type or a status left at zero turns a decode
// assertion into a puzzling failure. What each suite asserts about the decoded
// answer stays in that suite: an embedding answer and a judgment answer share no
// assertions.
func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}
