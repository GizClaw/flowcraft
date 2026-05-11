package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FeishuApp delivers run events as a single, live-updated Feishu CardKit
// card. Unlike the custom-bot webhook, this backend posts ONE message per
// eval run and rewrites the markdown body on every event, so a 4 h
// LongMemEval run shows up as a single growing card instead of N separate
// chat messages.
//
// Wire it via env (no CLI flags carry secrets):
//
//	FEISHU_APP_ID     — application ID (`cli_xxxxxxxxxxxxxxxx`)
//	FEISHU_APP_SECRET — application secret (32 hex)
//	FEISHU_CHAT_ID    — target group chat ID (`oc_…`)
//
// Required app scopes: `im:chat:readonly` (to enumerate chats during
// onboarding), `im:message`, `im:message:send_as_bot`, `cardkit:card`.
//
// Endpoints used (Feishu open platform):
//
//   - POST /open-apis/auth/v3/tenant_access_token/internal
//   - POST /open-apis/cardkit/v1/cards
//   - POST /open-apis/im/v1/messages?receive_id_type=chat_id
//   - PATCH /open-apis/cardkit/v1/cards/{card_id}/elements/{element_id}
//   - POST /open-apis/im/v1/messages/{message_id}/reply
//
// Notification model:
//
//   - The initial `interactive` card send triggers a normal chat
//     notification (mobile + desktop), so operators see the run begin.
//   - Every subsequent CardKit PATCH updates the body silently — Feishu
//     does NOT notify on card edits and there is no "edited" badge. This
//     is by design: a 100-milestone run does not spam the chat.
//   - For lifecycle kinds {ingest_done, done, error} we additionally
//     post a one-line TEXT reply threaded to the card. Replies DO fire
//     normal notifications, so the operator gets pinged exactly at the
//     handful of moments they actually need to look (phase boundaries,
//     final scores, failures) while progress milestones remain silent
//     edits on the same card.
type FeishuApp struct {
	AppID     string
	AppSecret string
	ChatID    string
	Name      string // run identifier, shown in the card header

	HTTP *http.Client // optional override; defaults to a 15s-timeout client
	Base string       // optional override of base URL; default https://open.feishu.cn
	Now  func() time.Time

	mu          sync.Mutex
	cardID      string
	messageID   string // chat message that hosts the card; reply target for lifecycle pings
	events      []renderedEvent
	token       string
	tokenExpiry time.Time
	sequence    int // monotonically increasing version number for CardKit PATCH ordering
}

type renderedEvent struct {
	At     time.Time
	Kind   string
	Title  string
	Body   string
	Fields map[string]string
}

// Notify implements [Notifier]. It is safe to call concurrently — each
// call serialises through a mutex because all events feed the same shared
// card body. Network latency is dominant (~200 ms per call) so contention
// here is irrelevant compared with the LLM-bound critical paths upstream.
func (f *FeishuApp) Notify(ctx context.Context, e Event) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if e.Time.IsZero() {
		e.Time = f.now()
	}
	f.events = append(f.events, renderedEvent{
		At: e.Time, Kind: e.Kind, Title: e.Title, Body: e.Body, Fields: e.Fields,
	})

	if err := f.ensureToken(ctx); err != nil {
		return err
	}

	if f.cardID == "" {
		// First event always creates the card. The interactive message
		// send fires its own chat notification, so we don't double-ping
		// with a thread reply on `start`.
		return f.createAndSendCard(ctx)
	}
	if err := f.updateLogElement(ctx); err != nil {
		return err
	}
	// Silent patches above keep progress noise off the chat list.
	// Lifecycle events get a thread reply so the operator's device
	// actually buzzes when the phase boundary lands.
	if isLifecycleNotify(e.Kind) && f.messageID != "" {
		if err := f.replyLifecycle(ctx, e); err != nil {
			return fmt.Errorf("feishu reply: %w", err)
		}
	}
	return nil
}

// isLifecycleNotify returns true for the small set of event kinds we
// surface as threaded chat replies (in addition to the silent card
// patch). The list is hard-coded — every suite that adds a new lifecycle
// kind makes a conscious decision whether it warrants a phone-buzz, and
// progress milestones intentionally stay silent.
//
// Excluded: "start" (the card's own creation notification covers it),
// every *_progress kind (transient, would defeat the whole point of
// CardKit consolidation), and the heartbeat-style events some suites
// may add later.
func isLifecycleNotify(kind string) bool {
	switch kind {
	case "ingest_done", "done", "error":
		return true
	}
	return false
}

func (f *FeishuApp) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *FeishuApp) baseURL() string {
	if f.Base != "" {
		return f.Base
	}
	return "https://open.feishu.cn"
}

func (f *FeishuApp) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// ensureToken caches the tenant_access_token for `expire - 300 s` so a single
// long Notify chain never crosses an expiry boundary. The token endpoint is
// itself unauthenticated (uses app_id / app_secret in the body).
func (f *FeishuApp) ensureToken(ctx context.Context) error {
	if f.token != "" && f.now().Before(f.tokenExpiry) {
		return nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.AppID,
		"app_secret": f.AppSecret,
	})
	var resp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := f.doJSON(ctx, "POST", "/open-apis/auth/v3/tenant_access_token/internal", body, "", &resp); err != nil {
		return fmt.Errorf("feishu auth: %w", err)
	}
	if resp.Code != 0 || resp.TenantAccessToken == "" {
		return fmt.Errorf("feishu auth code=%d msg=%q", resp.Code, resp.Msg)
	}
	f.token = resp.TenantAccessToken
	ttl := resp.Expire - 300
	if ttl < 60 {
		ttl = 60
	}
	f.tokenExpiry = f.now().Add(time.Duration(ttl) * time.Second)
	return nil
}

// createAndSendCard builds the initial card schema, registers it as a
// CardKit entity (so we get a stable card_id), and posts a single
// interactive message into the configured chat. Subsequent events only
// patch the card body.
func (f *FeishuApp) createAndSendCard(ctx context.Context) error {
	cardJSON, err := json.Marshal(f.cardSchema())
	if err != nil {
		return fmt.Errorf("feishu marshal card: %w", err)
	}

	createBody, _ := json.Marshal(map[string]string{
		"type": "card_json",
		"data": string(cardJSON),
	})
	var createResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			CardID string `json:"card_id"`
		} `json:"data"`
	}
	if err := f.doJSON(ctx, "POST", "/open-apis/cardkit/v1/cards", createBody, f.token, &createResp); err != nil {
		return fmt.Errorf("feishu create card: %w", err)
	}
	if createResp.Code != 0 || createResp.Data.CardID == "" {
		return fmt.Errorf("feishu create card code=%d msg=%q", createResp.Code, createResp.Msg)
	}
	cardID := createResp.Data.CardID

	// Send the card to the chat. The content field is a JSON-encoded
	// string that references the card we just created.
	content, _ := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	sendBody, _ := json.Marshal(map[string]string{
		"receive_id": f.ChatID,
		"msg_type":   "interactive",
		"content":    string(content),
	})
	var sendResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := f.doJSON(ctx, "POST", "/open-apis/im/v1/messages?receive_id_type=chat_id", sendBody, f.token, &sendResp); err != nil {
		return fmt.Errorf("feishu send card: %w", err)
	}
	if sendResp.Code != 0 {
		return fmt.Errorf("feishu send card code=%d msg=%q", sendResp.Code, sendResp.Msg)
	}

	f.cardID = cardID
	// message_id is the parent for lifecycle replies; we tolerate an
	// empty value (older Feishu API shapes) and just degrade to silent
	// patches in that case rather than failing the whole run.
	f.messageID = sendResp.Data.MessageID
	return nil
}

// replyLifecycle posts a one-line TEXT reply threaded to the card
// message. Unlike CardKit PATCHes (silent edits), replies trigger the
// normal Feishu chat notification path, so operators get pinged on
// phase boundaries / final scores / failures without subscribing to
// every milestone.
//
// Endpoint: `POST /open-apis/im/v1/messages/{message_id}/reply`
//
// Body fields:
//
//   - msg_type:  "text" — keeps the reply lightweight; nested cards
//     would re-render unnecessary chrome under the parent
//   - content:   JSON string with a single "text" field, capped at
//     ~300 chars so a long error body doesn't blow past
//     Feishu's per-message limit
func (f *FeishuApp) replyLifecycle(ctx context.Context, e Event) error {
	icon := iconFor(e.Kind)
	text := strings.TrimSpace(fmt.Sprintf("%s [%s] %s", icon, e.Kind, firstLine(e.Title)))
	// Compact body summary onto a second line when present so the
	// notification preview surfaces the headline number (qa.judge, p95,
	// error message). Trim aggressively — the full body lives on the
	// card; replies are just notification bait.
	if e.Body != "" {
		body := firstLine(e.Body)
		if len(text)+len(body)+1 > 300 {
			cut := 300 - len(text) - 4
			if cut < 0 {
				cut = 0
			}
			body = body[:cut] + "…"
		}
		text = text + "\n" + body
	}
	if len(text) > 300 {
		text = text[:297] + "…"
	}

	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{
		"msg_type": "text",
		"content":  string(contentJSON),
	})
	url := fmt.Sprintf("/open-apis/im/v1/messages/%s/reply", f.messageID)
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := f.doJSON(ctx, "POST", url, body, f.token, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("code=%d msg=%q", resp.Code, resp.Msg)
	}
	return nil
}

// updateLogElement rewrites the "log" markdown element with the freshly
// rendered event timeline. We deliberately re-render every time instead of
// using append-only operations: it keeps the implementation idempotent
// (retrying yields the same state) and avoids the 50-element CardKit cap.
//
// Endpoint: `PATCH /open-apis/cardkit/v1/cards/{card_id}/elements/{el_id}`
//
// Body fields:
//
//   - uuid             — random idempotency key.
//   - partial_element  — JSON-encoded STRING of the new element. The
//     encoded form (not a nested object) is required
//     by the CardKit API.
//   - sequence         — monotonically increasing version number. The
//     server rejects out-of-order updates with code
//     99992402; we keep f.sequence behind the same
//     mutex as f.cardID so ordering is guaranteed.
func (f *FeishuApp) updateLogElement(ctx context.Context) error {
	f.sequence++
	// CardKit's partial update only accepts mutable fields. `tag`,
	// `text_align`, `text_size` are immutable post-creation and are
	// rejected with code 300312 ("tag cannot be updated") if included.
	elem, _ := json.Marshal(map[string]any{
		"element_id": "log",
		"content":    f.renderMarkdown(),
	})
	body, _ := json.Marshal(map[string]any{
		"uuid":            randomUUID(),
		"partial_element": string(elem),
		"sequence":        f.sequence,
	})
	url := fmt.Sprintf("/open-apis/cardkit/v1/cards/%s/elements/log", f.cardID)
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := f.doJSON(ctx, "PATCH", url, body, f.token, &resp); err != nil {
		return fmt.Errorf("feishu patch element: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("feishu patch element code=%d msg=%q", resp.Code, resp.Msg)
	}
	return nil
}

// cardSchema builds the initial Feishu CardKit 2.0 card. The body contains
// a single named markdown element ("log") that subsequent events patch in
// place; this keeps the surface area small (one element ID, one endpoint).
//
// Header colour mapping (Feishu template values): blue=running (default).
// Done / error variants are reflected in the markdown body header line,
// not the card header, to avoid a second header-patch round-trip.
func (f *FeishuApp) cardSchema() any {
	name := f.Name
	if name == "" {
		name = "eval run"
	}
	subtitle := f.now().Format("2006-01-02 15:04:05 MST")
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
		},
		"header": map[string]any{
			"title": map[string]string{
				"tag":     "plain_text",
				"content": "🟦 " + name,
			},
			"subtitle": map[string]string{
				"tag":     "plain_text",
				"content": subtitle,
			},
			"template": "blue",
		},
		"body": map[string]any{
			"direction": "vertical",
			"padding":   "12px 12px 12px 12px",
			"elements": []any{
				map[string]any{
					"element_id": "log",
					"tag":        "markdown",
					"content":    f.renderMarkdown(),
					"text_align": "left",
					"text_size":  "normal_v2",
				},
			},
		},
	}
}

// renderMarkdown projects the accumulated event slice into the card body.
//
// Layout (top to bottom):
//
//  1. One-line status header        — icon + run name + elapsed + phase
//  2. "Latest" block                — title + body of the most recent event
//     (the body uses a fenced code block so
//     metric tables stay aligned)
//  3. "History" list (compact)      — one bullet per earlier event, body
//     intentionally omitted to bound the
//     card height
//
// Status icons (Feishu does not let us re-template the card header after
// creation, so the icon also gets folded into the markdown so the operator
// can see run state at a glance from the body alone):
//
//	🟦 running   any kind other than done/error
//	✅ done      kind == "done"
//	❌ error     kind == "error"
//
// Card height is O(N) in event count but each historical event contributes
// exactly one line, so even a 25 %-resolution 50 h run (~10 events) stays
// well under 20 lines of markdown.
func (f *FeishuApp) renderMarkdown() string {
	if len(f.events) == 0 {
		return "_waiting for first event…_"
	}
	last := f.events[len(f.events)-1]
	first := f.events[0]
	icon := iconFor(last.Kind)
	elapsed := last.At.Sub(first.At).Truncate(time.Second)

	var b strings.Builder
	fmt.Fprintf(&b, "%s started `%s` · elapsed `%s` · phase `%s`\n",
		icon, first.At.Format("2006-01-02 15:04:05"), elapsed, last.Kind)

	b.WriteString("\n**Latest**\n")
	if last.Title != "" {
		fmt.Fprintf(&b, "%s\n", last.Title)
	}
	if last.Body != "" {
		b.WriteString("```\n")
		b.WriteString(last.Body)
		b.WriteString("\n```\n")
	}

	if len(f.events) > 1 {
		var historyLines []string
		for i := 0; i < len(f.events)-1; i++ {
			e := f.events[i]
			// *_progress milestones are transient: they give live
			// visibility while a phase is running but are superseded
			// by the matching *_done. Keeping them in History bloats
			// the card on long runs (many progress lines + one done
			// line all saying essentially the same thing), so we drop
			// them once a newer event arrives. The current progress
			// event is still rendered in the Latest block.
			//
			// Covered kinds: ingest_progress, qa_progress, and the
			// history-suite per-strategy progress events
			// (strategy_progress, lane_progress). Hard-coded on
			// purpose: future eval suites that add their own *_progress
			// kind opt in by extending this list, which forces the
			// author to think about History-vs-Latest semantics.
			switch e.Kind {
			case "ingest_progress", "qa_progress",
				"strategy_progress", "lane_progress":
				continue
			}
			since := e.At.Sub(first.At).Truncate(time.Second)
			historyLines = append(historyLines,
				fmt.Sprintf("• `[%s]` `%s` (+%s) — %s",
					e.Kind, e.At.Format("15:04:05"), since, firstLine(e.Title)))
		}
		if len(historyLines) > 0 {
			b.WriteString("\n**History**\n")
			b.WriteString(strings.Join(historyLines, "\n"))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// iconFor maps an event kind to a single-glyph status indicator suitable
// for inline use in markdown.
func iconFor(kind string) string {
	switch kind {
	case "done":
		return "✅"
	case "error":
		return "❌"
	default:
		return "🟦"
	}
}

// firstLine returns the first line of s, stripped of trailing whitespace.
// Used in the History list so a multi-line title (rare, but possible from
// caller code) doesn't break the one-bullet-per-event invariant.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], " \t\r")
	}
	return s
}

// doJSON is the single HTTP helper shared by all CardKit + IM endpoints.
// authToken may be empty for the auth endpoint itself; for everything
// else it must be a valid tenant_access_token.
func (f *FeishuApp) doJSON(ctx context.Context, method, path string, body []byte, authToken string, decode any) error {
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if decode != nil {
		return json.Unmarshal(respBody, decode)
	}
	return nil
}

// randomUUID generates an RFC-4122-v4 UUID using crypto/rand. Used to
// stamp the `uuid` idempotency field on PATCH calls so a retried update
// is recognised as the same logical write by Feishu.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
