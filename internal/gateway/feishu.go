package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	otellog "go.opentelemetry.io/otel/log"
)

// FeishuChannel implements the Feishu webhook channel with outgoing token refresh.
type FeishuChannel struct {
	verificationToken string
	encryptKey        string
	appID             string
	appSecret         string
	token             atomic.Value // cached tenant_access_token
}

// NewFeishuChannelFromBinding creates a FeishuChannel from a generic config map.
func NewFeishuChannelFromBinding(m map[string]any) *FeishuChannel {
	s := func(key string) string { v, _ := m[key].(string); return v }
	return &FeishuChannel{
		verificationToken: s("verification_token"),
		encryptKey:        s("encrypt_key"),
		appID:             s("app_id"),
		appSecret:         s("app_secret"),
	}
}

// StartTokenRefresh starts the background goroutine for token refresh.
func (c *FeishuChannel) StartTokenRefresh(ctx context.Context) {
	if c.appID == "" || c.appSecret == "" {
		return
	}
	go c.refreshLoop(ctx)
}

func (c *FeishuChannel) Name() string { return "feishu" }

func (c *FeishuChannel) VerifySignature(r *http.Request) error {
	if c.encryptKey == "" {
		return nil
	}
	timestamp := r.Header.Get("X-Lark-Request-Timestamp")
	nonce := r.Header.Get("X-Lark-Request-Nonce")
	sig := r.Header.Get("X-Lark-Signature")

	if timestamp == "" || nonce == "" || sig == "" {
		return fmt.Errorf("feishu missing signature headers")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("feishu invalid timestamp")
	}
	if abs64(time.Now().Unix()-ts) > 300 {
		return fmt.Errorf("feishu timestamp too old")
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	content := timestamp + nonce + c.encryptKey + string(body)
	h := sha256.Sum256([]byte(content))
	expected := hex.EncodeToString(h[:])

	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return fmt.Errorf("feishu signature mismatch")
	}
	return nil
}

func (c *FeishuChannel) ParseRequest(r *http.Request) (*InboundMessage, *ChallengeResponse, error) {
	var payload struct {
		Schema    string `json:"schema"`
		Challenge string `json:"challenge"`
		Type      string `json:"type"`
		Header    struct {
			EventType string `json:"event_type"`
		} `json:"header"`
		Event struct {
			Sender struct {
				SenderID struct {
					OpenID string `json:"open_id"`
				} `json:"sender_id"`
			} `json:"sender"`
			Message struct {
				MessageType string `json:"message_type"`
				Content     string `json:"content"`
			} `json:"message"`
		} `json:"event"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("unmarshal feishu payload: %w", err)
	}

	if payload.Type == "url_verification" || payload.Challenge != "" {
		resp, _ := json.Marshal(map[string]string{"challenge": payload.Challenge})
		return nil, &ChallengeResponse{Body: resp, ContentType: "application/json"}, nil
	}

	if payload.Header.EventType != "im.message.receive_v1" {
		return nil, nil, nil
	}

	if payload.Event.Message.MessageType != "text" {
		return nil, nil, nil
	}

	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload.Event.Message.Content), &content); err != nil {
		return nil, nil, fmt.Errorf("unmarshal feishu message content: %w", err)
	}

	return &InboundMessage{
		ChannelName: "feishu",
		SenderID:    payload.Event.Sender.SenderID.OpenID,
		Text:        content.Text,
	}, nil, nil
}

func (c *FeishuChannel) FormatResponse(result *workflow.Result) ([]byte, error) {
	return json.Marshal(map[string]any{
		"msg_type": "text",
		"content": map[string]string{
			"text": result.Text(),
		},
	})
}

// Send implements OutboundChannel for Feishu using im/v1/messages API.
func (c *FeishuChannel) Send(ctx context.Context, recipientID string, message string) error {
	tok, _ := c.token.Load().(string)
	if tok == "" {
		telemetry.Warn(ctx, "gateway: feishu outgoing not configured (no access token)")
		return nil
	}

	msgContent, _ := json.Marshal(map[string]string{"text": message})
	body, _ := json.Marshal(map[string]any{
		"receive_id": recipientID,
		"msg_type":   "text",
		"content":    string(msgContent),
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=open_id",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("feishu send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu send: status %d", resp.StatusCode)
	}
	return nil
}

func (c *FeishuChannel) refreshLoop(ctx context.Context) {
	c.doRefresh(ctx)
	ticker := time.NewTicker(100 * time.Minute) // 2h validity, refresh at 100min
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.doRefresh(ctx)
		}
	}
}

func (c *FeishuChannel) doRefresh(ctx context.Context) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     c.appID,
		"app_secret": c.appSecret,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		bytes.NewReader(body))
	if err != nil {
		telemetry.Error(ctx, "gateway: feishu token refresh request", otellog.String("error", err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		telemetry.Error(ctx, "gateway: feishu token refresh", otellog.String("error", err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		telemetry.Error(ctx, "gateway: feishu token decode", otellog.String("error", err.Error()))
		return
	}
	if result.Code != 0 {
		telemetry.Error(ctx, "gateway: feishu token refresh failed", otellog.Int("code", result.Code))
		return
	}

	c.token.Store(result.TenantAccessToken)
	telemetry.Info(ctx, "gateway: feishu token refreshed", otellog.Int("expires_in", result.Expire))
}
