// Package webhook contains the WebhookOutboundSender, a projector that consumes
// webhook.outbound.{queued,scheduled} envelopes and performs the actual HTTP
// delivery with exponential-backoff retry and SSRF protection.
package webhook

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// SenderProjectorName is the canonical projector name in checkpoints.
const SenderProjectorName = "webhook_outbound"

// EndpointSecretLookup yields the HMAC secret for an endpoint, or "" to skip.
type EndpointSecretLookup func(endpointID string) string

// scheduledItem is one pending delivery in the in-memory min-heap.
type scheduledItem struct {
	DeliveryID  string
	EndpointID  string
	URL         string
	Method      string
	Headers     map[string]string
	Body        string
	Attempt     int32
	MaxAttempts int32
	NotBefore   time.Time
	Index       int // for heap.Interface
}

type webhookOutboundHeap []*scheduledItem

func (h webhookOutboundHeap) Len() int { return len(h) }
func (h webhookOutboundHeap) Less(i, j int) bool {
	return h[i].NotBefore.Before(h[j].NotBefore)
}
func (h webhookOutboundHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].Index = i
	h[j].Index = j
}
func (h *webhookOutboundHeap) Push(x any) {
	item := x.(*scheduledItem)
	item.Index = len(*h)
	*h = append(*h, item)
}
func (h *webhookOutboundHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.Index = -1
	*h = old[0 : n-1]
	return item
}

// Options bundles the optional knobs of a sender.
type Options struct {
	HTTPClient             *http.Client
	SSRF                   *SSRFGuard
	SecretLookup           EndpointSecretLookup // nil => no HMAC signing
	PerEndpointConcurrency int                  // 0 => 4
	TickInterval           time.Duration        // 0 => 500ms
	MaxBodyBytes           int64                // 0 => 1 MiB
}

// WebhookOutboundSender delivers outbound webhooks. It implements
// projection.Projector and is registered with projection.Manager.
type WebhookOutboundSender struct {
	log         eventlog.Log
	httpClient  *http.Client
	ssrf        *SSRFGuard
	secrets     EndpointSecretLookup
	tickEvery   time.Duration
	maxBody     int64
	endpointSem int

	mu       sync.Mutex
	heap     webhookOutboundHeap
	semByEp  map[string]chan struct{}
	ticker   *time.Ticker
	done     chan struct{}
	stopOnce sync.Once
}

var _ projection.Projector = (*WebhookOutboundSender)(nil)

// NewWebhookOutboundSender constructs a sender with the given options.
func NewWebhookOutboundSender(log eventlog.Log, opts Options) *WebhookOutboundSender {
	s := &WebhookOutboundSender{
		log:         log,
		httpClient:  opts.HTTPClient,
		ssrf:        opts.SSRF,
		secrets:     opts.SecretLookup,
		tickEvery:   opts.TickInterval,
		maxBody:     opts.MaxBodyBytes,
		endpointSem: opts.PerEndpointConcurrency,
		heap:        make(webhookOutboundHeap, 0),
		semByEp:     make(map[string]chan struct{}),
		done:        make(chan struct{}),
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if s.ssrf == nil {
		s.ssrf = NewSSRFGuard()
	}
	if s.tickEvery <= 0 {
		s.tickEvery = 500 * time.Millisecond
	}
	if s.maxBody <= 0 {
		s.maxBody = 1 << 20
	}
	if s.endpointSem <= 0 {
		s.endpointSem = 4
	}
	return s
}

// Name returns the canonical projector name.
func (s *WebhookOutboundSender) Name() string { return SenderProjectorName }

// Subscribes returns the envelope types this sender consumes.
func (s *WebhookOutboundSender) Subscribes() []string {
	return []string{
		eventlog.EventTypeWebhookOutboundQueued,
		eventlog.EventTypeWebhookOutboundScheduled,
	}
}

// RestoreMode returns RestoreReplay so the in-memory heap is rebuilt on
// every cold start by replaying queued + scheduled envelopes from
// the projector's checkpoint forward.
func (s *WebhookOutboundSender) RestoreMode() projection.RestoreMode {
	return projection.RestoreReplay
}

// OnReady starts the ticker that drains the heap.
func (s *WebhookOutboundSender) OnReady(ctx context.Context) error {
	s.ticker = time.NewTicker(s.tickEvery)
	go s.run(ctx)
	return nil
}

// Apply rebuilds the heap entry for one envelope. The runner ensures Apply
// runs before the corresponding checkpoint update commits, so a crash
// between the commit and the actual HTTP send is recovered by replaying
// the same envelope on next start.
func (s *WebhookOutboundSender) Apply(_ context.Context, _ eventlog.UnitOfWork, env eventlog.Envelope) error {
	switch env.Type {
	case eventlog.EventTypeWebhookOutboundQueued:
		return s.applyQueued(env)
	case eventlog.EventTypeWebhookOutboundScheduled:
		return s.applyScheduled(env)
	}
	return nil
}

func (s *WebhookOutboundSender) applyQueued(env eventlog.Envelope) error {
	var p eventlog.WebhookOutboundQueuedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return err
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	item := &scheduledItem{
		DeliveryID:  p.DeliveryID,
		EndpointID:  p.EndpointID,
		URL:         p.URL,
		Method:      p.Method,
		Headers:     p.Headers,
		Body:        p.Body,
		MaxAttempts: maxAttempts,
		Attempt:     1,
		NotBefore:   time.Now(),
	}
	s.mu.Lock()
	heap.Push(&s.heap, item)
	s.mu.Unlock()
	return nil
}

func (s *WebhookOutboundSender) applyScheduled(env eventlog.Envelope) error {
	var p eventlog.WebhookOutboundScheduledPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return err
	}
	notBefore, _ := time.Parse(time.RFC3339Nano, p.NotBefore)
	// We need URL/Method/Body from the originating queued envelope. For this
	// we look up the latest queued item already in the heap (replay path) or
	// we fall back to scanning the heap snapshot. If neither is present we
	// drop the schedule: the queued envelope must come first via replay.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.heap {
		if it.DeliveryID == p.DeliveryID {
			it.Attempt = p.Attempt
			it.NotBefore = notBefore
			heap.Fix(&s.heap, it.Index)
			return nil
		}
	}
	// Not present: this is the post-failure schedule emitted from a previous
	// run that already drained the queued. Recover the request descriptor by
	// scanning event_log for the queued envelope of the same delivery.
	queued, err := s.lookupQueued(p.EndpointID, p.DeliveryID)
	if err != nil {
		slog.Warn("webhook_outbound: cannot recover queued for scheduled",
			"delivery_id", p.DeliveryID, "err", err)
		return nil
	}
	if queued == nil {
		slog.Warn("webhook_outbound: queued envelope missing", "delivery_id", p.DeliveryID)
		return nil
	}
	maxAttempts := queued.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	heap.Push(&s.heap, &scheduledItem{
		DeliveryID:  p.DeliveryID,
		EndpointID:  p.EndpointID,
		URL:         queued.URL,
		Method:      queued.Method,
		Headers:     queued.Headers,
		Body:        queued.Body,
		MaxAttempts: maxAttempts,
		Attempt:     p.Attempt,
		NotBefore:   notBefore,
	})
	return nil
}

// lookupQueued pages back through event_log for the matching queued envelope.
// Bounded to a recent window via the partition index.
func (s *WebhookOutboundSender) lookupQueued(endpointID, deliveryID string) (*eventlog.WebhookOutboundQueuedPayload, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.log.Read(ctx, eventlog.PartitionWebhook(endpointID), eventlog.SinceBeginning, 1024)
	if err != nil {
		return nil, err
	}
	for i := len(res.Events) - 1; i >= 0; i-- {
		env := res.Events[i]
		if env.Type != eventlog.EventTypeWebhookOutboundQueued {
			continue
		}
		var p eventlog.WebhookOutboundQueuedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		if p.DeliveryID == deliveryID {
			return &p, nil
		}
	}
	return nil, nil
}

func (s *WebhookOutboundSender) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-s.ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *WebhookOutboundSender) tick(ctx context.Context) {
	now := time.Now()
	for {
		s.mu.Lock()
		if s.heap.Len() == 0 || s.heap[0].NotBefore.After(now) {
			s.mu.Unlock()
			return
		}
		item := heap.Pop(&s.heap).(*scheduledItem)
		s.mu.Unlock()
		go s.executeWithSema(ctx, item)
	}
}

func (s *WebhookOutboundSender) executeWithSema(ctx context.Context, item *scheduledItem) {
	sem := s.semaFor(item.EndpointID)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-sem }()
	s.executeOne(ctx, item)
}

func (s *WebhookOutboundSender) semaFor(endpointID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.semByEp[endpointID]; ok {
		return c
	}
	c := make(chan struct{}, s.endpointSem)
	s.semByEp[endpointID] = c
	return c
}

func (s *WebhookOutboundSender) executeOne(ctx context.Context, item *scheduledItem) {
	if err := s.ssrf.Check(item.URL); err != nil {
		s.publishExhausted(ctx, item, "ssrf_blocked", err.Error(), item.Attempt)
		return
	}
	body := item.Body
	if int64(len(body)) > s.maxBody {
		body = body[:s.maxBody]
	}
	method := item.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, item.URL, bytes.NewReader([]byte(body)))
	if err != nil {
		s.publishExhausted(ctx, item, "bad_request", err.Error(), item.Attempt)
		return
	}
	for k, v := range item.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Webhook-Delivery-ID", item.DeliveryID)
	req.Header.Set("X-Webhook-Attempt", fmt.Sprintf("%d", item.Attempt))
	if s.secrets != nil {
		if secret := s.secrets(item.EndpointID); secret != "" {
			sig := SignHMAC([]byte(body), secret)
			req.Header.Set("X-Webhook-Signature", "sha256="+sig)
		}
	}
	start := time.Now()
	resp, err := s.httpClient.Do(req)
	duration := time.Since(start)
	if err != nil {
		s.handleFailure(ctx, item, "transport_error", err.Error(), 0, duration)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		s.publishSent(ctx, item, int32(resp.StatusCode), string(respBody), duration)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 4xx are non-retryable: directly publish exhausted.
		s.handlePermanentFailure(ctx, item, int32(resp.StatusCode), string(respBody), duration)
	default:
		s.handleFailure(ctx, item, fmt.Sprintf("http_%d", resp.StatusCode), string(respBody), int32(resp.StatusCode), duration)
	}
}

func (s *WebhookOutboundSender) publishSent(ctx context.Context, item *scheduledItem, status int32, respBody string, duration time.Duration) {
	_, err := s.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishWebhookOutboundSentInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundSentPayload{
			DeliveryID:   item.DeliveryID,
			EndpointID:   item.EndpointID,
			Attempt:      item.Attempt,
			StatusCode:   status,
			ResponseBody: respBody,
			DurationMs:   duration.Milliseconds(),
		})
	})
	if err != nil {
		slog.Error("webhook_outbound: publish sent failed", "delivery_id", item.DeliveryID, "err", err)
	}
}

func (s *WebhookOutboundSender) handlePermanentFailure(ctx context.Context, item *scheduledItem, status int32, respBody string, duration time.Duration) {
	_, err := s.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		if err := eventlog.PublishWebhookOutboundAttemptFailedInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundAttemptFailedPayload{
			DeliveryID:   item.DeliveryID,
			EndpointID:   item.EndpointID,
			Attempt:      item.Attempt,
			ErrorClass:   "http_4xx",
			ErrorMessage: respBody,
			StatusCode:   status,
			DurationMs:   duration.Milliseconds(),
		}); err != nil {
			return err
		}
		return eventlog.PublishWebhookOutboundExhaustedInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundExhaustedPayload{
			DeliveryID:    item.DeliveryID,
			EndpointID:    item.EndpointID,
			TotalAttempts: item.Attempt,
			FinalError:    fmt.Sprintf("http_%d: %s", status, respBody),
		})
	})
	if err != nil {
		slog.Error("webhook_outbound: publish exhausted (4xx) failed",
			"delivery_id", item.DeliveryID, "err", err)
	}
}

func (s *WebhookOutboundSender) handleFailure(ctx context.Context, item *scheduledItem, errorClass, errorMsg string, status int32, duration time.Duration) {
	if item.Attempt >= item.MaxAttempts {
		s.publishExhausted(ctx, item, errorClass, errorMsg, item.Attempt)
		return
	}
	backoff := computeBackoff(int(item.Attempt), 1000)
	notBefore := time.Now().Add(backoff)
	_, err := s.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		if err := eventlog.PublishWebhookOutboundAttemptFailedInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundAttemptFailedPayload{
			DeliveryID:   item.DeliveryID,
			EndpointID:   item.EndpointID,
			Attempt:      item.Attempt,
			ErrorClass:   errorClass,
			ErrorMessage: errorMsg,
			StatusCode:   status,
			DurationMs:   duration.Milliseconds(),
		}); err != nil {
			return err
		}
		return eventlog.PublishWebhookOutboundScheduledInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundScheduledPayload{
			DeliveryID: item.DeliveryID,
			EndpointID: item.EndpointID,
			Attempt:    item.Attempt + 1,
			NotBefore:  notBefore.Format(time.RFC3339Nano),
			Reason:     errorClass,
		})
	})
	if err != nil {
		slog.Error("webhook_outbound: schedule retry failed",
			"delivery_id", item.DeliveryID, "err", err)
	}
}

func (s *WebhookOutboundSender) publishExhausted(ctx context.Context, item *scheduledItem, errorClass, errorMsg string, attempt int32) {
	_, err := s.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		if err := eventlog.PublishWebhookOutboundAttemptFailedInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundAttemptFailedPayload{
			DeliveryID:   item.DeliveryID,
			EndpointID:   item.EndpointID,
			Attempt:      attempt,
			ErrorClass:   errorClass,
			ErrorMessage: errorMsg,
		}); err != nil {
			return err
		}
		return eventlog.PublishWebhookOutboundExhaustedInTx(ctx, uow, item.EndpointID, eventlog.WebhookOutboundExhaustedPayload{
			DeliveryID:    item.DeliveryID,
			EndpointID:    item.EndpointID,
			TotalAttempts: attempt,
			FinalError:    fmt.Sprintf("%s: %s", errorClass, errorMsg),
		})
	})
	if err != nil {
		slog.Error("webhook_outbound: publish exhausted failed",
			"delivery_id", item.DeliveryID, "err", err)
	}
}

func computeBackoff(attempt int, initialMs int) time.Duration {
	base := time.Duration(initialMs) * time.Millisecond
	shift := attempt
	if shift > 6 {
		shift = 6
	}
	bo := base * time.Duration(1<<shift)
	if bo <= 0 {
		bo = base
	}
	jitter := time.Duration(rand.Int63n(int64(bo / 4)))
	return bo + jitter
}

// Stop signals the run loop to exit.
// ReplayDelivery requeues a delivery_id at the head of the heap (NotBefore=now)
// using the most recent queued envelope as the source of truth for URL/Method/
// Body. Used by /api/admin/webhooks/deliveries/{id}/replay to manually retry
// an exhausted delivery; if the delivery is unknown an error is returned.
func (s *WebhookOutboundSender) ReplayDelivery(ctx context.Context, deliveryID string) error {
	queued, err := s.findQueuedAcrossEndpoints(ctx, deliveryID)
	if err != nil {
		return err
	}
	if queued == nil {
		return fmt.Errorf("webhook_outbound: delivery %q not found", deliveryID)
	}
	maxAttempts := queued.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	s.mu.Lock()
	for i, it := range s.heap {
		if it.DeliveryID == deliveryID {
			s.heap[i].NotBefore = time.Now()
			s.heap[i].Attempt = 1
			heap.Fix(&s.heap, i)
			s.mu.Unlock()
			return nil
		}
	}
	heap.Push(&s.heap, &scheduledItem{
		DeliveryID:  deliveryID,
		EndpointID:  queued.EndpointID,
		URL:         queued.URL,
		Method:      queued.Method,
		Headers:     queued.Headers,
		Body:        queued.Body,
		MaxAttempts: maxAttempts,
		Attempt:     1,
		NotBefore:   time.Now(),
	})
	s.mu.Unlock()
	return nil
}

// findQueuedAcrossEndpoints scans the global event log (capped to 5000 most
// recent envelopes) for the latest webhook.outbound.queued whose delivery_id
// matches. The bounded scan keeps replay cheap; older envelopes that have
// rolled off the operational retention window cannot be replayed.
func (s *WebhookOutboundSender) findQueuedAcrossEndpoints(ctx context.Context, deliveryID string) (*eventlog.WebhookOutboundQueuedPayload, error) {
	const window = 5000
	res, err := s.log.ReadAll(ctx, eventlog.SinceBeginning, window)
	if err != nil {
		return nil, err
	}
	for i := len(res.Events) - 1; i >= 0; i-- {
		env := res.Events[i]
		if env.Type != eventlog.EventTypeWebhookOutboundQueued {
			continue
		}
		var p eventlog.WebhookOutboundQueuedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		if p.DeliveryID == deliveryID {
			return &p, nil
		}
	}
	return nil, nil
}

// PendingForTest returns a snapshot of the heap (delivery_id -> attempt).
// Test-only helper used to assert restart-restore correctness.
func (s *WebhookOutboundSender) PendingForTest() map[string]int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int32, len(s.heap))
	for _, it := range s.heap {
		out[it.DeliveryID] = it.Attempt
	}
	return out
}

func (s *WebhookOutboundSender) Stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		if s.ticker != nil {
			s.ticker.Stop()
		}
	})
}

// SignHMAC computes hex(HMAC-SHA256(secret, payload)).
func SignHMAC(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// ErrNoSchedule is returned when a scheduled envelope cannot find its queued
// envelope. Exported for tests.
var ErrNoSchedule = errors.New("webhook_outbound: scheduled without queued")
