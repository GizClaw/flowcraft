package stream

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/errcode"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// RunResult is the outcome of a single execution.
type RunResult struct {
	Value *workflow.Result
	Err   error
}

// EnrichFunc is called after MapEvent and before Send.
// Return false to drop the event.
type EnrichFunc func(ev event.Event, mapped *MappedEvent) bool

// StreamLoop is the single event consumption loop.
func StreamLoop(ctx context.Context, sub event.Subscription, done <-chan RunResult, sink EventSink, enrichers ...EnrichFunc) {
	var events <-chan event.Event
	if sub != nil {
		events = sub.Events()
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			mapped, valid := MapEvent(ev)
			if !valid {
				continue
			}
			skip := false
			for _, enrich := range enrichers {
				if !enrich(ev, &mapped) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if err := sink.Send(mapped); err != nil {
				return
			}

		case result, ok := <-done:
			if !ok {
				return
			}
			drainAndMap(sub, sink, enrichers)
			if result.Err != nil {
				_ = sink.Error(errorCategory(result.Err), result.Err.Error())
			} else {
				_ = sink.Done(result.Value)
			}
			return

		case <-ctx.Done():
			return
		}
	}
}

func drainAndMap(sub event.Subscription, sink EventSink, enrichers []EnrichFunc) {
	if sub == nil {
		return
	}
	for {
		select {
		case ev := <-sub.Events():
			mapped, ok := MapEvent(ev)
			if !ok {
				continue
			}
			skip := false
			for _, enrich := range enrichers {
				if !enrich(ev, &mapped) {
					skip = true
					break
				}
			}
			if !skip {
				_ = sink.Send(mapped)
			}
		default:
			return
		}
	}
}

func errorCategory(err error) string {
	switch {
	case errdefs.IsNotFound(err):
		return "not_found"
	case errdefs.IsValidation(err):
		return "validation_error"
	case errdefs.IsUnauthorized(err):
		return "unauthorized"
	case errdefs.IsForbidden(err):
		return "forbidden"
	case errdefs.IsConflict(err):
		return "conflict"
	case errdefs.IsRateLimit(err):
		return "rate_limit"
	case errdefs.IsTimeout(err):
		return "timeout"
	case errdefs.IsInterrupted(err):
		return "interrupted"
	case errdefs.IsAborted(err):
		return "aborted"
	case errdefs.IsNotAvailable(err):
		return "not_available"
	default:
		return errcode.CodeInternalError
	}
}

// WrapActorDone attaches callback metadata to the Result.State.
func WrapActorDone(ch <-chan RunResult, inputs map[string]any) <-chan RunResult {
	out := make(chan RunResult, 1)
	go func() {
		result := <-ch
		if result.Err == nil && result.Value != nil {
			if cbVal, ok := inputs[model.InputKeyCallback]; ok && cbVal != nil {
				if result.Value.State == nil {
					result.Value.State = make(map[string]any)
				}
				result.Value.State["callback"] = true
				if cardID, ok := cbVal.(string); ok && cardID != "" {
					result.Value.State["card_id"] = cardID
				}
			}
		}
		out <- result
	}()
	return out
}
