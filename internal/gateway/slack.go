package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// SlackChannel implements the Slack webhook channel.
type SlackChannel struct {
	signingSecret string
	botToken      string
}

// NewSlackChannel creates a Slack channel with the given credentials.
func NewSlackChannel(signingSecret, botToken string) *SlackChannel {
	return &SlackChannel{signingSecret: signingSecret, botToken: botToken}
}

func (c *SlackChannel) Name() string { return "slack" }

func (c *SlackChannel) VerifySignature(r *http.Request) error {
	if c.signingSecret == "" {
		return nil
	}
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp")
	}
	if abs64(time.Now().Unix()-ts) > 300 {
		return fmt.Errorf("timestamp too old")
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	baseString := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(c.signingSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func (c *SlackChannel) ParseRequest(r *http.Request) (*InboundMessage, *ChallengeResponse, error) {
	var payload struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Event     struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			User    string `json:"user"`
			Channel string `json:"channel"`
			BotID   string `json:"bot_id"`
		} `json:"event"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("unmarshal slack payload: %w", err)
	}

	if payload.Type == "url_verification" {
		resp, _ := json.Marshal(map[string]string{"challenge": payload.Challenge})
		return nil, &ChallengeResponse{Body: resp, ContentType: "application/json"}, nil
	}

	if payload.Event.Type != "message" || payload.Event.BotID != "" {
		return nil, nil, nil
	}

	return &InboundMessage{
		ChannelName: "slack",
		SenderID:    payload.Event.User,
		Text:        payload.Event.Text,
		Metadata: map[string]any{
			"slack_channel": payload.Event.Channel,
		},
	}, nil, nil
}

func (c *SlackChannel) FormatResponse(result *workflow.Result) ([]byte, error) {
	return json.Marshal(map[string]string{"text": result.Text()})
}

// Send implements OutboundChannel for Slack using chat.postMessage API.
func (c *SlackChannel) Send(ctx context.Context, recipientID string, message string) error {
	if c.botToken == "" {
		telemetry.Warn(ctx, "gateway: slack outgoing not configured (no bot_token)")
		return nil
	}

	// Convert basic markdown to Slack mrkdwn
	slackMsg := markdownToMrkdwn(message)

	body, _ := json.Marshal(map[string]any{
		"channel": recipientID,
		"text":    slackMsg,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slackResp); err != nil {
		return fmt.Errorf("slack send: decode response: %w", err)
	}
	if !slackResp.OK {
		return fmt.Errorf("slack send: api error: %s", slackResp.Error)
	}
	return nil
}

func markdownToMrkdwn(md string) string {
	s := strings.ReplaceAll(md, "**", "*")
	s = strings.ReplaceAll(s, "__", "_")
	return s
}
