package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPI_RunList_Pagination drives several Submits, then walks
// /v1/runs with a small page_size and asserts every run is yielded
// exactly once across the cursor chain. Without this contract,
// downstream operators (k8s controllers, dashboards) would
// double-count or drop entries between pages.
func TestAPI_RunList_Pagination(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	const total = 5
	for i := 0; i < total; i++ {
		body := strings.NewReader(fmt.Sprintf(`{"agent":"helper","query":"q%d"}`, i))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/vessels/support/call", body))
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status %d body=%s", i, w.Code, w.Body.String())
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		target := "/v1/runs?page_size=2"
		if cursor != "" {
			target += "&page_token=" + cursor
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("list page %d status=%d body=%s", pages, w.Code, w.Body.String())
		}
		var got struct {
			Runs []struct {
				RunID  string `json:"run_id"`
				Vessel string `json:"vessel"`
			} `json:"runs"`
			NextPageToken string `json:"next_page_token"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Runs) > 2 {
			t.Fatalf("page_size=2 violated: got %d", len(got.Runs))
		}
		for _, r := range got.Runs {
			if seen[r.RunID] {
				t.Fatalf("duplicate run_id %q across pages", r.RunID)
			}
			seen[r.RunID] = true
			if r.Vessel != "support" {
				t.Fatalf("unexpected vessel %q", r.Vessel)
			}
		}
		if got.NextPageToken == "" {
			break
		}
		cursor = got.NextPageToken
	}
	if len(seen) != total {
		t.Fatalf("walked %d runs, expected %d", len(seen), total)
	}
}

// TestAPI_RunList_VesselFilter asserts ?vessel=<unknown> returns
// an empty page rather than 404 — pagination contract requires the
// filter to be a no-op on no-match, not a hard error.
func TestAPI_RunList_VesselFilter(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)

	body := strings.NewReader(`{"agent":"helper","query":"hi"}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/vessels/support/call", body))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/runs?vessel=missing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Runs []any `json:"runs"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 0 {
		t.Fatalf("expected empty, got %d entries", len(got.Runs))
	}
}
