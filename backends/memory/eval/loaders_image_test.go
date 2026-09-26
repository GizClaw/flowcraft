package eval

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// onePixelPNG is a real 1x1 PNG, used to test the magic-byte check.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestImageFetcherValidatesPayloadsAndBoundsItsCache covers the three paths
// that had no test while being the ones that already mattered in practice: a
// lying content type, a failed fetch, and the cache bound.
func TestImageFetcherValidatesPayloadsAndBoundsItsCache(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	var flaky atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/real"):
			// A host that mislabels the response must still work: the bytes win.
			writer.Header().Set("Content-Type", "text/html")
			_, _ = writer.Write(png)
		case strings.HasPrefix(request.URL.Path, "/fake"):
			// The inverse: an HTML block page advertised as an image must be
			// rejected, which is exactly what stalled derivation before.
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write([]byte("<html>hotlink blocked</html>"))
		case strings.HasPrefix(request.URL.Path, "/flaky"):
			if flaky.Add(1) == 1 {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write(png)
		default:
			writer.Header().Set("Content-Type", "image/gif")
			_, _ = writer.Write(png)
		}
	}))
	defer server.Close()

	fetcher := newImageFetcher()

	parts, ok := fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/real.png"}})
	if !ok || len(parts) != 1 {
		t.Fatalf("a mislabelled but real image was rejected: ok=%v parts=%d", ok, len(parts))
	}
	if _, ok := parts[0].(coremessage.ImagePart); !ok {
		t.Fatalf("expected an image part, got %T", parts[0])
	}

	if _, ok := fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/fake.png"}}); ok {
		t.Fatal("an HTML block page served as image/png was accepted")
	}

	flakyTurn := loCoMoRawTurn{ImgURL: []string{server.URL + "/flaky.png"}}
	if _, ok := fetcher.parts(flakyTurn); ok {
		t.Fatal("a 500 response was accepted")
	}
	// A failure is remembered (so a batch of runs sees the same image set)
	// rather than re-fetched on every attempt.
	if _, ok := fetcher.parts(flakyTurn); ok {
		t.Fatal("a freshly cached failure was retried anyway")
	}
	if got := flaky.Load(); got != 1 {
		t.Fatalf("the server was hit %d times for one cached failure, want 1", got)
	}
	// Once the marker is stale the url is retried and can recover.
	// Clearing the marker (the TTL path) lets the url recover.
	if err := os.Remove(fetcher.cachedPath(flakyTurn.ImgURL[0])); err != nil {
		t.Fatal(err)
	}
	if _, ok := fetcher.parts(flakyTurn); !ok {
		t.Fatal("a cleared failure marker still blocked the retry")
	}

	// The cache is bounded: an unbounded one held gigabytes for a full run.
	for index := 0; index < maxCachedImages+8; index++ {
		fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/bulk-" + string(rune('a'+index%26)) + string(rune('a'+index/26)) + ".gif"}})
	}
	if len(fetcher.cache) > maxCachedImages {
		t.Fatalf("cache grew to %d entries, want at most %d", len(fetcher.cache), maxCachedImages)
	}
}
