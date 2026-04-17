package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// DingTalkChannel implements the DingTalk webhook channel.
type DingTalkChannel struct {
	appSecret  string
	webhookURL string
}

// NewDingTalkChannel creates a DingTalk channel.
func NewDingTalkChannel(appSecret, webhookURL string) *DingTalkChannel {
	return &DingTalkChannel{appSecret: appSecret, webhookURL: webhookURL}
}

func (c *DingTalkChannel) Name() string { return "dingtalk" }

func (c *DingTalkChannel) VerifySignature(r *http.Request) error {
	if c.appSecret == "" {
		return nil
	}
	timestamp := r.Header.Get("timestamp")
	sign := r.Header.Get("sign")

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("dingtalk invalid timestamp")
	}
	if abs64(time.Now().UnixMilli()-ts) > 3600000 {
		return fmt.Errorf("dingtalk timestamp too old")
	}

	stringToSign := timestamp + "\n" + c.appSecret
	mac := hmac.New(sha256.New, []byte(c.appSecret))
	mac.Write([]byte(stringToSign))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sign)) {
		return fmt.Errorf("dingtalk signature mismatch")
	}
	return nil
}

func (c *DingTalkChannel) ParseRequest(r *http.Request) (*InboundMessage, *ChallengeResponse, error) {
	var payload struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
		SenderStaffID  string `json:"senderStaffId"`
		SenderNick     string `json:"senderNick"`
		ConversationID string `json:"conversationId"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("unmarshal dingtalk payload: %w", err)
	}

	if payload.MsgType != "text" {
		return nil, nil, nil
	}

	return &InboundMessage{
		ChannelName: "dingtalk",
		SenderID:    payload.SenderStaffID,
		Text:        payload.Text.Content,
		Metadata: map[string]any{
			"sender_nick":     payload.SenderNick,
			"conversation_id": payload.ConversationID,
		},
	}, nil, nil
}

func (c *DingTalkChannel) FormatResponse(result *workflow.Result) ([]byte, error) {
	return json.Marshal(map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": result.Text(),
		},
	})
}

// Send implements OutboundChannel for DingTalk.
// Note: DingTalk webhook mode posts to a fixed webhook URL and cannot target
// individual users. The recipientID parameter is accepted for interface compliance
// but is not used in the outgoing request.
func (c *DingTalkChannel) Send(ctx context.Context, _ string, message string) error {
	if c.webhookURL == "" {
		telemetry.Warn(ctx, "gateway: dingtalk outgoing not configured (no webhook_url)")
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": message,
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dingtalk send: status %d", resp.StatusCode)
	}
	return nil
}
