package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
)

const defaultSinkDeliveryTimeout = 30 * time.Second

type sinkItem struct {
	ctx   context.Context
	env   event.Envelope
	delta agent.StreamDeltaPayload
}

type queuedSink struct {
	spec      SinkSpec
	session   *Session
	runEnd    event.Subject
	offered   func(string, DeliveryCursor)
	delivered func(string, DeliveryCursor)
	onDetach  func(error)
	queue     chan sinkItem
	stop      chan struct{}
	done      chan struct{}

	mu       sync.Mutex
	detached bool
}

type sinkDetach struct {
	callback func(error)
}

func newQueuedSink(session *Session, runID string, spec SinkSpec, size int) *queuedSink {
	return &queuedSink{
		spec:    spec,
		session: session,
		runEnd:  agent.SubjectRunEnd(runID),
		queue:   make(chan sinkItem, size),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *queuedSink) OnDelta(ctx context.Context, env event.Envelope, delta agent.StreamDeltaPayload) error {
	s.mu.Lock()
	if s.detached {
		s.mu.Unlock()
		return nil
	}
	select {
	case s.queue <- sinkItem{ctx: context.WithoutCancel(ctx), env: env, delta: delta}:
		s.mu.Unlock()
		return nil
	default:
		err := fmt.Errorf("%w: %s", ErrSinkQueueFull, s.spec.ID)
		cleanup := s.markDetachedLocked(err)
		s.mu.Unlock()
		go s.completeDetach(cleanup, err)
		return nil
	}
}

func (s *queuedSink) start() {
	go func() {
		for {
			select {
			case <-s.stop:
				return
			case item := <-s.queue:
				delivered, err := s.deliver(item)
				if err != nil {
					s.detach(err)
					return
				}
				if delivered && s.delivered != nil {
					if cursor, err := DeliveryCursorFromEnvelope(item.env); err == nil {
						s.delivered(s.spec.ID, cursor)
					}
				}
				if item.env.Subject == s.runEnd {
					s.detach(nil)
					return
				}
			}
		}
	}()
}

func (s *queuedSink) deliver(item sinkItem) (bool, error) {
	timeout := s.spec.DeliveryTimeout
	if timeout == 0 {
		timeout = defaultSinkDeliveryTimeout
	}
	ctx, cancel := context.WithTimeout(item.ctx, timeout)
	defer cancel()

	if s.offered != nil {
		if cursor, err := DeliveryCursorFromEnvelope(item.env); err == nil {
			s.offered(s.spec.ID, cursor)
		}
	}
	result := make(chan error, 1)
	go func() {
		result <- s.spec.Sink.OnDelta(ctx, item.env, item.delta)
	}()

	select {
	case err := <-result:
		return err == nil, err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.stop:
		return false, nil
	}
}

func (s *queuedSink) detach(err error) {
	s.mu.Lock()
	if s.detached {
		s.mu.Unlock()
		return
	}
	cleanup := s.markDetachedLocked(err)
	s.mu.Unlock()
	s.completeDetach(cleanup, err)
}

func (s *queuedSink) markDetachedLocked(err error) sinkDetach {
	s.detached = true
	close(s.stop)
	if s.onDetach != nil {
		s.onDetach(err)
	}
	return sinkDetach{callback: s.spec.OnDetach}
}

func (s *queuedSink) completeDetach(cleanup sinkDetach, err error) {
	s.session.changeActivity(activitySink, -1)
	close(s.done)
	if cleanup.callback != nil {
		go func() {
			defer func() { _ = recover() }()
			cleanup.callback(err)
		}()
	}
}

func (s *queuedSink) wait() {
	if s != nil {
		<-s.done
	}
}
