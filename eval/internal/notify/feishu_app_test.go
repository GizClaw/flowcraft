package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFeishu mocks the four endpoints FeishuApp talks to. It records every
// request body for assertion, returns a stable card_id on the create call,
// and reports the number of times each endpoint was hit.
type fakeFeishu struct {
	mu             sync.Mutex
	tokenCalls     int
	createCalls    int
	sendCalls      int
	patchCalls     int
	replyCalls     int
	lastCreateBody map[string]any
	lastSendBody   map[string]any
	lastPatchBody  map[string]any
	lastPatchPath  string
	lastReplyBody  map[string]any
	lastReplyPath  string
	cardID         string
}

func (s *fakeFeishu) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)

		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			s.tokenCalls++
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-fake","expire":7200}`))

		case r.URL.Path == "/open-apis/cardkit/v1/cards" && r.Method == http.MethodPost:
			s.createCalls++
			s.lastCreateBody = parsed
			s.cardID = "AAqcardfake"
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"card_id":"AAqcardfake"}}`))

		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/") && strings.HasSuffix(r.URL.Path, "/reply"):
			s.replyCalls++
			s.lastReplyBody = parsed
			s.lastReplyPath = r.URL.Path
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))

		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages"):
			s.sendCalls++
			s.lastSendBody = parsed
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_fake"}}`))

		case strings.Contains(r.URL.Path, "/cardkit/v1/cards/") && strings.Contains(r.URL.Path, "/elements/") && r.Method == http.MethodPatch:
			s.patchCalls++
			s.lastPatchBody = parsed
			s.lastPatchPath = r.URL.Path
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

// TestFeishuApp_FirstEventCreatesAndSendsCard walks the happy path for
// the first event of a run: token fetch, card creation, send-to-chat —
// no patch yet, since the card content is set inline at creation time.
func TestFeishuApp_FirstEventCreatesAndSendsCard(t *testing.T) {
	mock := &fakeFeishu{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	app := &FeishuApp{
		AppID:     "cli_test",
		AppSecret: "secret-test",
		ChatID:    "oc_test",
		Name:      "smoke",
		Base:      srv.URL,
		Now:       func() time.Time { return time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC) },
	}

	err := app.Notify(context.Background(), Event{
		Kind:  "start",
		Title: "eval start: runner=flowcraft dataset=synthetic",
		Body:  "conversations=2 questions=3",
	})
	if err != nil {
		t.Fatalf("first Notify: %v", err)
	}

	if mock.tokenCalls != 1 {
		t.Errorf("token calls: got %d want 1", mock.tokenCalls)
	}
	if mock.createCalls != 1 {
		t.Errorf("create calls: got %d want 1", mock.createCalls)
	}
	if mock.sendCalls != 1 {
		t.Errorf("send calls: got %d want 1", mock.sendCalls)
	}
	if mock.patchCalls != 0 {
		t.Errorf("patch calls: got %d want 0", mock.patchCalls)
	}

	// Verify the card body actually carries the run name and event title.
	cardJSON, _ := mock.lastCreateBody["data"].(string)
	if !strings.Contains(cardJSON, "smoke") {
		t.Errorf("card data should contain name 'smoke': %s", cardJSON)
	}
	if !strings.Contains(cardJSON, "eval start") {
		t.Errorf("card data should contain event title: %s", cardJSON)
	}
	// Verify chat targeting.
	if mock.lastSendBody["receive_id"] != "oc_test" {
		t.Errorf("receive_id mismatch: %v", mock.lastSendBody["receive_id"])
	}
	if mock.lastSendBody["msg_type"] != "interactive" {
		t.Errorf("msg_type mismatch: %v", mock.lastSendBody["msg_type"])
	}
}

// TestFeishuApp_SubsequentEventsPatch verifies that once a card_id is
// known, every Notify rewrites the single "log" element rather than
// posting a new card or sending a new chat message.
func TestFeishuApp_SubsequentEventsPatch(t *testing.T) {
	mock := &fakeFeishu{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	app := &FeishuApp{
		AppID: "cli_test", AppSecret: "s", ChatID: "oc_test", Name: "smoke",
		Base: srv.URL,
		Now:  func() time.Time { return time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC) },
	}
	ctx := context.Background()
	if err := app.Notify(ctx, Event{Kind: "start", Title: "begin"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := app.Notify(ctx, Event{Kind: "qa_progress", Title: "25%", Body: "12/48 questions"}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := app.Notify(ctx, Event{Kind: "done", Title: "complete", Body: "qa.judge=0.612"}); err != nil {
		t.Fatalf("done: %v", err)
	}

	if mock.createCalls != 1 {
		t.Errorf("create calls: got %d want 1 (only on first event)", mock.createCalls)
	}
	if mock.sendCalls != 1 {
		t.Errorf("send calls: got %d want 1 (only on first event)", mock.sendCalls)
	}
	if mock.patchCalls != 2 {
		t.Errorf("patch calls: got %d want 2 (one per non-first event)", mock.patchCalls)
	}
	if !strings.HasSuffix(mock.lastPatchPath, "/elements/log") {
		t.Errorf("patch path should end in /elements/log: %s", mock.lastPatchPath)
	}

	// The partial_element field is a JSON-encoded string per the CardKit
	// contract. The CardKit API rejects nested-object form with code
	// 99992402, so the test pins the stringified shape.
	elementStr, _ := mock.lastPatchBody["partial_element"].(string)
	if elementStr == "" {
		t.Fatalf("patch body missing partial_element field: %v", mock.lastPatchBody)
	}
	if _, ok := mock.lastPatchBody["sequence"]; !ok {
		t.Errorf("patch body must include sequence for ordering: %v", mock.lastPatchBody)
	}
	// The final patch (after `done`) MUST contain the done title +
	// body and the start title (which lives in History). It MUST NOT
	// contain the qa_progress title — _progress events are transient
	// and are dropped from History once superseded by the next event,
	// so the card doesn't bloat over a 50 h run with dozens of
	// progress milestones.
	for _, want := range []string{"begin", "complete", "qa.judge=0.612", "✅"} {
		if !strings.Contains(elementStr, want) {
			t.Errorf("patched element should contain %q\n--- element ---\n%s", want, elementStr)
		}
	}
	if strings.Contains(elementStr, "25%") {
		t.Errorf("patched element MUST NOT contain superseded qa_progress title %q\n--- element ---\n%s", "25%", elementStr)
	}
}

// TestFeishuApp_ProgressEventsFilteredFromHistory pins the filter
// rule end-to-end across every *_progress kind we know about:
// ingest_progress (locomo/longmemeval), qa_progress (locomo /
// longmemeval / simpleqa / beir / taubench), strategy_progress
// (history), lane_progress (knowledge). All of them must be visible
// in Latest while they're the most recent event, then dropped from
// History the moment a non-progress event arrives.
func TestFeishuApp_ProgressEventsFilteredFromHistory(t *testing.T) {
	mock := &fakeFeishu{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	app := &FeishuApp{
		AppID: "cli_test", AppSecret: "s", ChatID: "oc_test", Name: "smoke",
		Base: srv.URL,
		Now:  func() time.Time { return time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC) },
	}
	ctx := context.Background()
	progressKinds := []string{"ingest_progress", "qa_progress", "strategy_progress", "lane_progress"}
	if err := app.Notify(ctx, Event{Kind: "start", Title: "begin"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, k := range progressKinds {
		title := k + "-25%"
		if err := app.Notify(ctx, Event{Kind: k, Title: title}); err != nil {
			t.Fatalf("%d %s: %v", i, k, err)
		}
		// While this kind is Latest its title must be visible.
		body, _ := mock.lastPatchBody["partial_element"].(string)
		if !strings.Contains(body, title) {
			t.Errorf("Latest must show current %s title %q; body=%s", k, title, body)
		}
	}
	if err := app.Notify(ctx, Event{Kind: "done", Title: "complete"}); err != nil {
		t.Fatalf("done: %v", err)
	}
	final, _ := mock.lastPatchBody["partial_element"].(string)
	if !strings.Contains(final, "begin") || !strings.Contains(final, "complete") {
		t.Errorf("final card must keep start (in History) + done (in Latest); body=%s", final)
	}
	for _, k := range progressKinds {
		title := k + "-25%"
		if strings.Contains(final, title) {
			t.Errorf("%s title %q must be dropped from History once superseded; body=%s", k, title, final)
		}
	}
}

// TestFeishuApp_LifecycleReply verifies that lifecycle events
// (ingest_done / done / error) additionally post a threaded text reply
// to the parent card message, while progress and start events do not.
// The reply path embeds the message_id captured from the initial card
// send, so this also indirectly checks the message_id wire-up.
func TestFeishuApp_LifecycleReply(t *testing.T) {
	mock := &fakeFeishu{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	app := &FeishuApp{
		AppID: "cli", AppSecret: "s", ChatID: "oc",
		Name: "lme-s", Base: srv.URL,
	}
	ctx := context.Background()

	// 1. start: creates card, sends to chat — no reply expected.
	if err := app.Notify(ctx, Event{Kind: "start", Title: "begin"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if mock.replyCalls != 0 {
		t.Errorf("start must NOT trigger reply; got %d reply calls", mock.replyCalls)
	}

	// 2. ingest_progress (any *_progress): silent patch only.
	if err := app.Notify(ctx, Event{Kind: "ingest_progress", Title: "ingest 5%"}); err != nil {
		t.Fatalf("ingest_progress: %v", err)
	}
	if mock.replyCalls != 0 {
		t.Errorf("ingest_progress must NOT trigger reply; got %d reply calls", mock.replyCalls)
	}

	// 3. ingest_done: lifecycle — reply expected.
	if err := app.Notify(ctx, Event{
		Kind:  "ingest_done",
		Title: "ingest done in 2h48m (2436 Save calls)",
		Body:  "save.p50=42s save.p95=2m18s",
	}); err != nil {
		t.Fatalf("ingest_done: %v", err)
	}
	if mock.replyCalls != 1 {
		t.Fatalf("ingest_done must trigger 1 reply; got %d", mock.replyCalls)
	}
	if !strings.HasPrefix(mock.lastReplyPath, "/open-apis/im/v1/messages/om_fake/reply") {
		t.Errorf("reply path must embed parent message_id; got %s", mock.lastReplyPath)
	}
	// Reply payload is {msg_type: text, content: JSON-string-with-text,
	// reply_in_thread: true}. The thread flag is the difference between
	// "tidy thread under the card" and "flooding the main chat list" —
	// see the doc on replyLifecycle for the rationale.
	if got := mock.lastReplyBody["msg_type"]; got != "text" {
		t.Errorf("reply msg_type: got %v want text", got)
	}
	if got := mock.lastReplyBody["reply_in_thread"]; got != true {
		t.Errorf("reply MUST set reply_in_thread=true so the reply stays inside the card's thread; got %v", got)
	}
	contentRaw, _ := mock.lastReplyBody["content"].(string)
	var content map[string]string
	_ = json.Unmarshal([]byte(contentRaw), &content)
	if !strings.Contains(content["text"], "ingest_done") || !strings.Contains(content["text"], "2436 Save calls") {
		t.Errorf("reply text should surface kind + headline title; got %q", content["text"])
	}
	if !strings.Contains(content["text"], "save.p95=2m18s") {
		t.Errorf("reply text should append first line of Body for the notification preview; got %q", content["text"])
	}

	// 4. done: lifecycle — second reply.
	if err := app.Notify(ctx, Event{Kind: "done", Title: "eval done in 2h50m", Body: "qa.judge=0.960"}); err != nil {
		t.Fatalf("done: %v", err)
	}
	if mock.replyCalls != 2 {
		t.Errorf("done must trigger 1 additional reply; got total %d", mock.replyCalls)
	}

	// 5. error: lifecycle — third reply.
	if err := app.Notify(ctx, Event{Kind: "error", Title: "extractor blew up"}); err != nil {
		t.Fatalf("error: %v", err)
	}
	if mock.replyCalls != 3 {
		t.Errorf("error must trigger 1 additional reply; got total %d", mock.replyCalls)
	}

	// Sanity: silent patches kept growing across every non-start event
	// (start used inline content, no patch yet).
	if mock.patchCalls != 4 {
		t.Errorf("expected 4 patches (progress + ingest_done + done + error); got %d", mock.patchCalls)
	}
}

// TestFeishuApp_TokenCached confirms that the token endpoint is hit
// exactly once across many events when the cached value is still
// fresh. Without caching, a 50 h run would hammer the auth endpoint
// thousands of times.
func TestFeishuApp_TokenCached(t *testing.T) {
	mock := &fakeFeishu{}
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()

	app := &FeishuApp{
		AppID: "cli_test", AppSecret: "s", ChatID: "oc_test",
		Base: srv.URL,
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := app.Notify(ctx, Event{Kind: "qa_progress"}); err != nil {
			t.Fatalf("Notify %d: %v", i, err)
		}
	}
	if mock.tokenCalls != 1 {
		t.Errorf("token calls: got %d want 1 across 5 events", mock.tokenCalls)
	}
}

// TestFeishuApp_AuthFailure surfaces auth-level errors instead of
// silently no-op'ing the rest of the run. Without this assertion a
// rotated secret would produce a card that never shows up but leaves
// no log either.
func TestFeishuApp_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":99991663,"msg":"app secret invalid"}`))
	}))
	defer srv.Close()
	app := &FeishuApp{AppID: "cli", AppSecret: "bad", ChatID: "oc", Base: srv.URL}
	err := app.Notify(context.Background(), Event{Kind: "start"})
	if err == nil || !strings.Contains(err.Error(), "99991663") {
		t.Fatalf("expected auth error containing code 99991663, got %v", err)
	}
}

func TestFromFlags_AppRouteWhenAllCredsPresent(t *testing.T) {
	n, err := FromFlags(FlagOptions{
		FeishuAppID:     "cli_x",
		FeishuAppSecret: "secret",
		FeishuChatID:    "oc_x",
	})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if _, ok := n.(*FeishuApp); !ok {
		t.Errorf("expected *FeishuApp when app creds present, got %T", n)
	}
}

// TestFromFlags_PartialAppCredsFallsBackToNoOp documents the explicit
// "all or nothing" rule: with webhook removed, partial CardKit creds
// are not a usable backend, so the safe default is NoOp (silent) rather
// than failing the eval.
func TestFromFlags_PartialAppCredsFallsBackToNoOp(t *testing.T) {
	n, err := FromFlags(FlagOptions{FeishuAppID: "cli_x"})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if _, ok := n.(NoOp); !ok {
		t.Errorf("expected NoOp fallback, got %T", n)
	}
}

func TestFromFlags_DryRunOverridesApp(t *testing.T) {
	n, err := FromFlags(FlagOptions{
		DryRun:          true,
		FeishuAppID:     "cli_x",
		FeishuAppSecret: "s",
		FeishuChatID:    "oc_x",
	})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if _, ok := n.(Logger); !ok {
		t.Errorf("expected Logger under DryRun, got %T", n)
	}
}

func TestFromFlags_EmptyIsNoOp(t *testing.T) {
	n, err := FromFlags(FlagOptions{})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if _, ok := n.(NoOp); !ok {
		t.Errorf("expected NoOp, got %T", n)
	}
}

// TestRandomUUID is a sanity check on shape + variant bits, not on
// statistical uniformity — those are crypto/rand's problem.
func TestRandomUUID(t *testing.T) {
	got := randomUUID()
	if len(got) != 36 {
		t.Errorf("length: got %d want 36 (got %q)", len(got), got)
	}
	if got[14] != '4' {
		t.Errorf("version byte should be 4, got %c (%q)", got[14], got)
	}
	if v := got[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("variant byte should be one of 8/9/a/b, got %c (%q)", v, got)
	}
}
