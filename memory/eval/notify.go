package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// notifyEvent is one eval lifecycle event delivered to a notifier.
type notifyEvent struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

// notifier delivers eval lifecycle events. Implementations must be safe for
// concurrent use.
type notifier interface {
	Notify(context.Context, notifyEvent) error
}

// noopNotifier drops every event.
type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, notifyEvent) error { return nil }

// loggerNotifier writes events to stderr (--notify-dry-run).
type loggerNotifier struct {
	name string
}

func (l loggerNotifier) Notify(_ context.Context, event notifyEvent) error {
	title := event.Title
	if l.name != "" {
		title = fmt.Sprintf("[%s] %s", l.name, title)
	}
	if event.Body == "" {
		fmt.Fprintf(os.Stderr, "[notify dry-run] %s %s\n", event.Kind, title)
	} else {
		fmt.Fprintf(os.Stderr, "[notify dry-run] %s %s\n%s\n", event.Kind, title, indent(event.Body))
	}
	return nil
}

func indent(value string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = "    " + lines[index]
	}
	return strings.Join(lines, "\n")
}

// buildNotifier routes --notify-* flags and FEISHU_* env vars to a concrete
// backend. Dry-run wins; Feishu requires all three credentials; otherwise
// events are dropped silently.
func buildNotifier(name string, dryRun bool) notifier {
	if dryRun {
		return loggerNotifier{name: name}
	}
	appID := os.Getenv("FEISHU_APP_ID")
	appSecret := os.Getenv("FEISHU_APP_SECRET")
	chatID := os.Getenv("FEISHU_CHAT_ID")
	if appID != "" && appSecret != "" && chatID != "" {
		return &feishuApp{appID: appID, appSecret: appSecret, chatID: chatID, name: name}
	}
	return noopNotifier{}
}

// bestEffortNotify sends an event and logs failures instead of failing the
// eval: notifications are advisory.
func bestEffortNotify(ctx context.Context, n notifier, event notifyEvent) {
	if n == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	if err := n.Notify(ctx, event); err != nil {
		log.Printf("[notify] %s: %v", event.Kind, err)
	}
}

// feishuApp delivers events as one live-updated Feishu CardKit card. The
// initial interactive message notifies the chat; progress events patch the
// card silently; ingest_done / done / error additionally post a threaded
// text reply that triggers a real notification.
type feishuApp struct {
	appID     string
	appSecret string
	chatID    string
	name      string

	http  *http.Client
	base  string
	clock func() time.Time

	mu          sync.Mutex
	cardID      string
	messageID   string
	events      []renderedNotifyEvent
	token       string
	tokenExpiry time.Time
	sequence    int
}

type renderedNotifyEvent struct {
	At     time.Time
	Kind   string
	Title  string
	Body   string
	Fields map[string]string
}

func (f *feishuApp) Notify(ctx context.Context, event notifyEvent) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if event.Time.IsZero() {
		event.Time = f.now()
	}
	f.events = append(f.events, renderedNotifyEvent{
		At: event.Time, Kind: event.Kind, Title: event.Title,
		Body: event.Body, Fields: event.Fields,
	})

	if err := f.ensureToken(ctx); err != nil {
		return err
	}
	if f.cardID == "" {
		return f.createAndSendCard(ctx)
	}
	if err := f.updateLogElement(ctx); err != nil {
		return err
	}
	if isLifecycleNotify(event.Kind) && f.messageID != "" {
		if err := f.replyLifecycle(ctx, event); err != nil {
			return fmt.Errorf("feishu reply: %w", err)
		}
	}
	return nil
}

func isLifecycleNotify(kind string) bool {
	switch kind {
	case "ingest_done", "done", "error":
		return true
	}
	return false
}

func (f *feishuApp) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}

func (f *feishuApp) baseURL() string {
	if f.base != "" {
		return f.base
	}
	return "https://open.feishu.cn"
}

func (f *feishuApp) httpClient() *http.Client {
	if f.http != nil {
		return f.http
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (f *feishuApp) ensureToken(ctx context.Context) error {
	if f.token != "" && f.now().Before(f.tokenExpiry) {
		return nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.appID,
		"app_secret": f.appSecret,
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

func (f *feishuApp) createAndSendCard(ctx context.Context) error {
	cardJSON, err := json.Marshal(f.cardSchema())
	if err != nil {
		return fmt.Errorf("feishu marshal card: %w", err)
	}
	createBody, _ := json.Marshal(map[string]string{"type": "card_json", "data": string(cardJSON)})
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
	content, _ := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": createResp.Data.CardID},
	})
	sendBody, _ := json.Marshal(map[string]string{
		"receive_id": f.chatID,
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
	f.cardID = createResp.Data.CardID
	f.messageID = sendResp.Data.MessageID
	return nil
}

func (f *feishuApp) replyLifecycle(ctx context.Context, event notifyEvent) error {
	icon := iconFor(event.Kind)
	text := strings.TrimSpace(fmt.Sprintf("%s [%s] %s", icon, event.Kind, firstLine(event.Title)))
	if event.Body != "" {
		body := firstLine(event.Body)
		if runeLen(text)+runeLen(body)+1 > 300 {
			cut := 300 - runeLen(text) - 4
			if cut < 0 {
				cut = 0
			}
			body = truncateRunes(body, cut, "…")
		}
		text += "\n" + body
	}
	if runeLen(text) > 300 {
		text = truncateRunes(text, 297, "…")
	}
	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]any{
		"msg_type":        "text",
		"content":         string(contentJSON),
		"reply_in_thread": true,
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

func (f *feishuApp) updateLogElement(ctx context.Context) error {
	f.sequence++
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

func (f *feishuApp) cardSchema() any {
	name := f.name
	if name == "" {
		name = "eval run"
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
		},
		"header": map[string]any{
			"title": map[string]string{"tag": "plain_text", "content": "🟦 " + name},
			"subtitle": map[string]string{
				"tag": "plain_text", "content": f.now().Format("2006-01-02 15:04:05 MST"),
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

func (f *feishuApp) renderMarkdown() string {
	if len(f.events) == 0 {
		return "_waiting for first event…_"
	}
	last := f.events[len(f.events)-1]
	first := f.events[0]
	icon := iconFor(last.Kind)
	elapsed := last.At.Sub(first.At).Truncate(time.Second)
	host := notifyHost(first)

	var b strings.Builder
	if host != "" {
		fmt.Fprintf(&b, "%s host `%s` · started `%s` · elapsed `%s` · phase `%s`\n",
			icon, host, first.At.Format("2006-01-02 15:04:05"), elapsed, last.Kind)
	} else {
		fmt.Fprintf(&b, "%s started `%s` · elapsed `%s` · phase `%s`\n",
			icon, first.At.Format("2006-01-02 15:04:05"), elapsed, last.Kind)
	}
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
		var history []string
		for index := 0; index < len(f.events)-1; index++ {
			event := f.events[index]
			switch event.Kind {
			case "ingest_progress", "qa_progress":
				continue
			}
			since := event.At.Sub(first.At).Truncate(time.Second)
			history = append(history, fmt.Sprintf("• `[%s]` `%s` (+%s) — %s",
				event.Kind, event.At.Format("15:04:05"), since, firstLine(event.Title)))
		}
		if len(history) > 0 {
			b.WriteString("\n**History**\n")
			b.WriteString(strings.Join(history, "\n"))
			b.WriteString("\n")
		}
	}
	return b.String()
}

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

func notifyHost(event renderedNotifyEvent) string {
	if event.Fields != nil {
		if host := strings.TrimSpace(event.Fields["host"]); host != "" {
			return host
		}
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(host)
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimRight(value[:index], " \t\r")
	}
	return value
}

func truncateRunes(value string, max int, suffix string) string {
	runes := []rune(value)
	if max <= 0 {
		return suffix
	}
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + suffix
}

func runeLen(value string) int {
	return len([]rune(value))
}

func (f *feishuApp) doJSON(ctx context.Context, method, path string, body []byte, authToken string, decode any) error {
	request, err := http.NewRequestWithContext(ctx, method, f.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		request.Header.Set("Authorization", "Bearer "+authToken)
	}
	response, err := f.httpClient().Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 32*1024))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if decode != nil {
		return json.Unmarshal(responseBody, decode)
	}
	return nil
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
