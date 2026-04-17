// Package gateway implements the webhook gateway for external IM integrations
// (Slack, DingTalk, Feishu) with both incoming and outgoing message support.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/errcode"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/stream"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	otellog "go.opentelemetry.io/otel/log"
)

const defaultWebhookTimeout = 5 * time.Minute

// Channel is the interface for incoming webhook message handling.
type Channel interface {
	Name() string
	ParseRequest(r *http.Request) (*InboundMessage, *ChallengeResponse, error)
	FormatResponse(result *workflow.Result) ([]byte, error)
	VerifySignature(r *http.Request) error
}

// OutboundChannel extends Channel with the ability to send messages.
type OutboundChannel interface {
	Channel
	Send(ctx context.Context, recipientID string, message string) error
}

// InboundMessage represents a parsed incoming webhook message.
type InboundMessage struct {
	ChannelName string         `json:"channel"`
	SenderID    string         `json:"sender_id"`
	Text        string         `json:"text"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ChallengeResponse is returned for platform verification callbacks.
type ChallengeResponse struct {
	Body        []byte
	ContentType string
}

// GatewayOption configures a Gateway.
type GatewayOption func(*Gateway)

// WithWebhookTimeout sets the maximum duration for synchronous webhook execution.
func WithWebhookTimeout(d time.Duration) GatewayOption {
	return func(g *Gateway) {
		if d > 0 {
			g.webhookTimeout = d
		}
	}
}

// WithRealmProvider enables realm-backed webhook execution.
func WithRealmProvider(mgr *realm.SingleRealmProvider) GatewayOption {
	return func(g *Gateway) {
		g.runtimeMgr = mgr
	}
}

// Gateway routes incoming webhook requests to the appropriate Channel and runner.
type Gateway struct {
	router         *ChannelRouter
	store          model.Store
	runtimeMgr     *realm.SingleRealmProvider
	webhookTimeout time.Duration
}

// NewGateway creates a webhook gateway backed by a ChannelRouter.
func NewGateway(store model.Store, router *ChannelRouter, opts ...GatewayOption) *Gateway {
	gw := &Gateway{
		router:         router,
		store:          store,
		webhookTimeout: defaultWebhookTimeout,
	}
	for _, opt := range opts {
		opt(gw)
	}
	return gw
}

// Router returns the underlying ChannelRouter.
func (g *Gateway) Router() *ChannelRouter {
	return g.router
}

// HandleWebhook routes an incoming webhook to the agent resolved by ChannelRouter.
func (g *Gateway) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelType := r.PathValue("channel")

	agentID, ch, err := g.router.Resolve(channelType, r)
	if err != nil {
		writeGWError(ctx, w, errdefs.NotFoundf("channel binding %s not found", channelType))
		return
	}

	msg, challenge, parseErr := ch.ParseRequest(r)
	if parseErr != nil {
		writeGWError(ctx, w, errdefs.Validationf("parse webhook request: %v", parseErr))
		return
	}
	if challenge != nil {
		ct := challenge.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(challenge.Body)
		return
	}
	if msg == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	a, err := g.store.GetAgent(ctx, agentID)
	if err != nil {
		writeGWError(ctx, w, errdefs.Internalf("get agent: %v", err))
		return
	}

	if g.runtimeMgr == nil {
		writeGWError(ctx, w, errdefs.Internalf("runtime manager not configured"))
		return
	}
	rt, err := g.runtimeMgr.Get(ctx)
	if err != nil {
		writeGWError(ctx, w, errdefs.Internalf("get/create runtime: %v", err))
		return
	}
	req := &workflow.Request{
		ContextID: msg.SenderID + "--" + agentID,
		RuntimeID: msg.SenderID,
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, msg.Text),
	}

	done := rt.SendToAgent(ctx, a, req, realm.WithSource("webhook"))
	streamDone := bridgeResult(done)

	outCh, canPush := ch.(OutboundChannel)
	if canPush {
		g.writeAck(ctx, w, ch)
		go g.asyncStreamLoop(streamDone, outCh, msg.SenderID)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), g.webhookTimeout)
		defer cancel()
		stream.StreamLoop(ctx, nil, streamDone, &webhookSink{ctx: ctx, w: w, ch: ch})
	}
}

func (g *Gateway) writeAck(ctx context.Context, w http.ResponseWriter, ch Channel) {
	ackResult := &workflow.Result{
		Status:   workflow.StatusWorking,
		Messages: []sdkmodel.Message{sdkmodel.NewTextMessage(sdkmodel.RoleAssistant, "收到，正在处理...")},
	}
	resp, err := ch.FormatResponse(ackResult)
	if err != nil {
		writeGWError(ctx, w, errdefs.Internalf("format ack response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func bridgeResult(src <-chan realm.RunResult) <-chan stream.RunResult {
	ch := make(chan stream.RunResult, 1)
	go func() {
		r := <-src
		ch <- stream.RunResult{Value: r.Value, Err: r.Err}
		close(ch)
	}()
	return ch
}

func (g *Gateway) asyncStreamLoop(done <-chan stream.RunResult, outCh OutboundChannel, recipientID string) {
	ctx, cancel := context.WithTimeout(context.Background(), g.webhookTimeout+30*time.Second)
	defer cancel()
	stream.StreamLoop(ctx, nil, done, &outboundSink{ctx: ctx, ch: outCh, recipientID: recipientID})
}

type outboundSink struct {
	ctx         context.Context
	ch          OutboundChannel
	recipientID string
}

func (s *outboundSink) Send(_ stream.MappedEvent) error { return nil }

func (s *outboundSink) Done(result *workflow.Result) error {
	answer := result.Text()
	if answer == "" {
		telemetry.Warn(s.ctx, "gateway: webhook execution returned empty answer",
			otellog.String("recipient_id", s.recipientID))
		return nil
	}
	if err := s.ch.Send(s.ctx, s.recipientID, answer); err != nil {
		telemetry.Error(s.ctx, "gateway: outbound send failed",
			otellog.String("recipient_id", s.recipientID),
			otellog.String("error", err.Error()))
		return err
	}
	return nil
}

func (s *outboundSink) Error(code, message string) error {
	reply := fmt.Sprintf("执行失败: %s", message)
	if err := s.ch.Send(s.ctx, s.recipientID, reply); err != nil {
		telemetry.Error(s.ctx, "gateway: outbound error send failed",
			otellog.String("recipient_id", s.recipientID),
			otellog.String("error", err.Error()))
	}
	return nil
}

type webhookSink struct {
	ctx context.Context
	w   http.ResponseWriter
	ch  Channel
}

func (s *webhookSink) Send(_ stream.MappedEvent) error { return nil }

func (s *webhookSink) Done(result *workflow.Result) error {
	resp, err := s.ch.FormatResponse(result)
	if err != nil {
		writeGWError(s.ctx, s.w, errdefs.Internalf("format response: %v", err))
		return err
	}
	s.w.Header().Set("Content-Type", "application/json")
	_, _ = s.w.Write(resp)
	return nil
}

func (s *webhookSink) Error(code, message string) error {
	writeGWError(s.ctx, s.w, errcode.FromCode(code, message))
	return nil
}

func writeGWError(ctx context.Context, w http.ResponseWriter, err error) {
	code, status := errcode.Resolve(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": errcode.PublicMessage(err),
		},
	}); encErr != nil {
		telemetry.Warn(ctx, "gateway: failed to write error response",
			otellog.String("error", encErr.Error()))
	}
}

var outboundHTTPClient = &http.Client{Timeout: 30 * time.Second}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// FormatNotification creates a human-readable notification message from an event.
func FormatNotification(ev event.Event) string {
	switch ev.Type {
	case event.EventGraphEnd:
		return fmt.Sprintf("工作流执行完成 (run: %s)", ev.RunID)
	case event.EventNodeError:
		return fmt.Sprintf("节点 %s 执行失败 (run: %s)", ev.NodeID, ev.RunID)
	case event.EventNodeComplete:
		return fmt.Sprintf("节点 %s 执行完成 (run: %s)", ev.NodeID, ev.RunID)
	case event.EventType(kanban.EventTaskCompleted):
		return formatKanbanNotification("✅ 任务完成", ev)
	case event.EventType(kanban.EventTaskFailed):
		return formatKanbanNotification("❌ 任务失败", ev)
	case event.EventType(kanban.EventCallbackDone):
		return formatCallbackDoneNotification(ev)
	default:
		return fmt.Sprintf("事件 %s (run: %s)", ev.Type, ev.RunID)
	}
}

func formatCallbackDoneNotification(ev event.Event) string {
	p, ok := ev.Payload.(kanban.CallbackDonePayload)
	if !ok {
		return "📋 回调处理完成"
	}
	if p.Error != "" {
		return fmt.Sprintf("📋 回调处理失败 (card: %s): %s", p.CardID, p.Error)
	}
	return fmt.Sprintf("📋 回调处理完成 (card: %s)", p.CardID)
}

func formatKanbanNotification(prefix string, ev event.Event) string {
	var agentID, cardID, graphName, output, errMsg string
	switch p := ev.Payload.(type) {
	case kanban.TaskCompletedPayload:
		agentID, cardID, graphName, output = p.TargetAgentID, p.CardID, p.TargetAgentID, p.Output
	case kanban.TaskFailedPayload:
		agentID, cardID, graphName, errMsg = p.TargetAgentID, p.CardID, p.TargetAgentID, p.Error
	case map[string]any:
		agentID, _ = p["target_agent_id"].(string)
		cardID, _ = p["card_id"].(string)
		graphName, _ = p["target_agent_id"].(string)
		output, _ = p["output"].(string)
		errMsg, _ = p["error"].(string)
	}

	var b strings.Builder
	b.WriteString(prefix)
	if graphName != "" {
		fmt.Fprintf(&b, " [%s]", graphName)
	}
	if cardID != "" {
		fmt.Fprintf(&b, " (card: %s)", cardID)
	} else if agentID != "" {
		fmt.Fprintf(&b, " (agent: %s)", agentID)
	}
	if errMsg != "" {
		summary := errMsg
		if len(summary) > 100 {
			summary = summary[:100] + "..."
		}
		fmt.Fprintf(&b, "\n错误: %s", summary)
	} else if output != "" {
		summary := output
		if len(summary) > 150 {
			summary = summary[:150] + "..."
		}
		fmt.Fprintf(&b, "\n结果: %s", summary)
	}
	return b.String()
}
