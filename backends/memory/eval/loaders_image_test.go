package eval

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// onePixelPNG is a real 1x1 PNG, used to test the magic-byte check.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// isolatedFetcher returns a fetcher whose disk cache is a fresh temporary
// directory, so one test cannot read another's markers.
func isolatedFetcher(t *testing.T) *imageFetcher {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	return newImageFetcher()
}

// TestImageFetcherKeepsTransientFailuresRetryable is the regression guard for
// the bug that made the benchmark's image set a function of the network's luck:
// every failure used to be written to the disk cache, so a timeout or a 500 was
// pinned for the next 24h and a later run silently inherited a smaller image
// set. Only a failure that describes the url may be remembered.
func TestImageFetcherKeepsTransientFailuresRetryable(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	var gone, busy atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/gone"):
			gone.Add(1)
			writer.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(request.URL.Path, "/busy"):
			busy.Add(1)
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write(png)
		}
	}))
	defer server.Close()
	t.Setenv("TMPDIR", t.TempDir())

	// A dead link is a property of the url: it is remembered, so a second run
	// reports the same reason without touching the network.
	goneTurn := loCoMoRawTurn{ImgURL: []string{server.URL + "/gone.png"}}
	first := newImageFetcher()
	_, goneErr := first.parts(goneTurn)
	if goneErr == nil {
		t.Fatal("a 404 was accepted")
	}
	if kind, detail := failureKind(goneErr); kind != failurePermanent || !strings.Contains(detail, "404") {
		t.Fatalf("404 classified as %q/%q, want permanent/status 404", kind, detail)
	}
	if got := gone.Load(); got != 1 {
		t.Fatalf("the server saw %d requests for one url used twice in one run, want 1", got)
	}
	second := newImageFetcher()
	_, cachedGoneErr := second.parts(goneTurn)
	if cachedGoneErr == nil {
		t.Fatal("a cached 404 was accepted")
	}
	if kind, _ := failureKind(cachedGoneErr); kind != failurePermanent {
		t.Fatalf("a cached 404 reports kind %q, want permanent", kind)
	}
	if got := gone.Load(); got != 1 {
		t.Fatalf("the server saw %d requests, want 1: a cached failure was re-fetched", got)
	}

	// A 5xx is a property of the moment: it is retried inside the run and not
	// remembered, so the next run asks again instead of inheriting the loss.
	busyTurn := loCoMoRawTurn{ImgURL: []string{server.URL + "/busy.png"}}
	_, busyErr := first.parts(busyTurn)
	if busyErr == nil {
		t.Fatal("a 500 was accepted")
	}
	if got := busy.Load(); got != imageAttempts {
		t.Fatalf("the server saw %d requests, want %d: a server-side transient must be retried", got, imageAttempts)
	}
	if kind, _ := failureKind(busyErr); kind != failureBusy {
		t.Fatalf("a 500 classified as %q, want busy", kind)
	}
	if _, err := second.parts(busyTurn); err == nil {
		t.Fatal("a 500 was accepted")
	}
	if got := busy.Load(); got != imageAttempts*2 {
		t.Fatalf("the server saw %d requests, want %d: a transient failure was cached", got, imageAttempts*2)
	}
}

// TestImageFetcherValidatesPayloadsAndBoundsItsCache covers the paths that
// matter in practice: a lying content type, a rejected payload, a retry that
// recovers, and the cache bound.
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

	fetcher := isolatedFetcher(t)

	parts, err := fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/real.png"}})
	if err != nil || len(parts) != 1 {
		t.Fatalf("a mislabelled but real image was rejected: err=%v parts=%d", err, len(parts))
	}
	if _, ok := parts[0].(coremessage.ImagePart); !ok {
		t.Fatalf("expected an image part, got %T", parts[0])
	}

	blockTurn := loCoMoRawTurn{ImgURL: []string{server.URL + "/fake.png"}}
	if _, err := fetcher.parts(blockTurn); err == nil {
		t.Fatal("an HTML block page served as image/png was accepted")
	}

	// A server-side hiccup recovers inside the run, without a second run.
	if _, err := fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/flaky.png"}}); err != nil {
		t.Fatalf("a transient 500 was not retried: %v", err)
	}

	// The cache is bounded: an unbounded one held gigabytes for a full run.
	for index := 0; index < maxCachedImages+8; index++ {
		_, _ = fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/bulk-" + string(rune('a'+index%26)) + string(rune('a'+index/26)) + ".gif"}})
	}
	if len(fetcher.cache) > maxCachedImages {
		t.Fatalf("cache grew to %d entries, want at most %d", len(fetcher.cache), maxCachedImages)
	}
}

// TestImageFetcherShrinksOversizedPayloads pins the second half of the fix: an
// image larger than the inline budget is re-encoded rather than dropped, because
// a turn's caption is not a stand-in for its picture -- the reference harness
// attaches the image, so an all-caption library understates what the dataset
// shows (and makes the native/annotation comparison meaningless).
func TestImageFetcherShrinksOversizedPayloads(t *testing.T) {
	oversized := noisePNG(t, 1100)
	if len(oversized) <= maxImageBytes {
		t.Skipf("the generated payload is %d bytes, not over the %d byte budget", len(oversized), maxImageBytes)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write(oversized)
	}))
	defer server.Close()

	fetcher := isolatedFetcher(t)
	parts, err := fetcher.parts(loCoMoRawTurn{ImgURL: []string{server.URL + "/big.png"}})
	if err != nil {
		t.Fatalf("an oversized image was dropped instead of shrunk: %v", err)
	}
	imagePart, ok := parts[0].(coremessage.ImagePart)
	if !ok {
		t.Fatalf("expected an image part, got %T", parts[0])
	}
	if size := len(imagePart.Source.Bytes()); size > maxImageBytes {
		t.Fatalf("the shrunk payload is %d bytes, over the %d byte budget", size, maxImageBytes)
	}
	if got := fetcher.shrunkCount(); got != 1 {
		t.Fatalf("shrunkCount = %d, want 1", got)
	}
	if kind := imageSignature(imagePart.Source.Bytes()); kind != "image/jpeg" {
		t.Fatalf("the shrunk payload is %q, want image/jpeg", kind)
	}
}

// TestImageFailureClassification pins the decision table: only a cause that
// describes the url may be written to the cross-run cache.
func TestImageFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		kind   imageFailureKind
		cached bool
	}{
		{name: "404", err: classifyStatus(http.StatusNotFound), kind: failurePermanent, cached: true},
		{name: "403 hotlink block", err: classifyStatus(http.StatusForbidden), kind: failurePermanent, cached: true},
		{name: "500", err: classifyStatus(http.StatusInternalServerError), kind: failureBusy, cached: false},
		{name: "429", err: classifyStatus(http.StatusTooManyRequests), kind: failureBusy, cached: false},
		{name: "530", err: classifyStatus(530), kind: failureBusy, cached: false},
		{name: "404 reported as readable", err: &fetchError{kind: failurePermanent, detail: "status 404"}, kind: failurePermanent, cached: true},
		{name: "oversized", err: &fetchError{kind: failureOversized, detail: "5MB"}, kind: failureOversized, cached: true},
		{
			name:   "timeout",
			err:    classifyTransportError(timeoutError{}),
			kind:   failureTransient,
			cached: false,
		},
		{
			name:   "dns not found",
			err:    classifyTransportError(&net.DNSError{Err: "no such host", Name: "dead.example", IsNotFound: true}),
			kind:   failurePermanent,
			cached: true,
		},
		{
			name:   "connection refused",
			err:    classifyTransportError(errors.New("dial tcp: connection refused")),
			kind:   failureTransient,
			cached: false,
		},
		{name: "unclassified", err: errors.New("something new"), kind: failurePermanent, cached: true},
		{name: "success", err: nil, kind: "", cached: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, _ := failureKind(test.err)
			if kind != test.kind {
				t.Fatalf("failureKind = %q, want %q", kind, test.kind)
			}
			if got := cachedFailure(kind); got != test.cached {
				t.Fatalf("cachedFailure(%q) = %v, want %v", kind, got, test.cached)
			}
		})
	}
}

// TestScaleHalfAveragesBlocks checks the resampler: the standard library has no
// scaler, and a nearest-neighbour copy would alias on photographs.
func TestScaleHalfAveragesBlocks(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			value := uint8(0)
			if x >= 16 {
				value = 200
			}
			source.SetRGBA(x, y, color.RGBA{R: value, G: value, B: value, A: 255})
		}
	}
	halved := scaleHalf(source)
	if halved == nil || halved.Bounds().Dx() != 16 {
		t.Fatalf("scaleHalf returned %#v", halved)
	}
	if got := halved.At(0, 0); got.(color.RGBA).R != 0 {
		t.Fatalf("left half averaged to %v, want 0", got)
	}
	if got := halved.At(15, 0); got.(color.RGBA).R != 200 {
		t.Fatalf("right half averaged to %v, want 200", got)
	}
	if scaleHalf(image.NewRGBA(image.Rect(0, 0, 8, 8))) != nil {
		t.Fatal("an image too small to halve must report nil rather than produce a 1x1")
	}
}

// noisePNG builds a payload that no codec can compress, so the test can rely on
// a real oversized download instead of a synthesised error.
func noisePNG(t *testing.T, size int) []byte {
	t.Helper()
	random := rand.New(rand.NewSource(1))
	picture := image.NewRGBA(image.Rect(0, 0, size, size))
	for index := range picture.Pix {
		picture.Pix[index] = uint8(random.Intn(256))
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// timeoutError stands in for a request deadline, which net's own type cannot be
// constructed directly.
type timeoutError struct{}

func (timeoutError) Error() string   { return "context deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}
