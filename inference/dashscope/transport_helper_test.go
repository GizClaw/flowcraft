package dashscope

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newJSONServer answers every request with one canned JSON body.
func newJSONServer(t *testing.T, write func(io.Writer)) string {
	t.Helper()
	return newJSONServerStatus(t, http.StatusOK, write)
}

func newJSONServerStatus(t *testing.T, status int, write func(io.Writer)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		write(w)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// newSSEServer answers with one event stream whose payloads are the given data
// lines.
func newSSEServer(t *testing.T, lines []string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range lines {
			_, _ = io.WriteString(w, "data: "+line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// newTaskServer answers requests through a handler that decides per path, so one
// server can play the create endpoint, the task endpoint and the artifact host.
func newTaskServer(t *testing.T, handle func(w io.Writer, path string) bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !handle(w, r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}
