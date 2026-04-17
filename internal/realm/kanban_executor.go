package realm

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	otellog "go.opentelemetry.io/otel/log"
)

// kanbanAgentExecutor bridges Kanban task dispatch to Realm actor execution.
type kanbanAgentExecutor struct {
	realm *Realm
}

func (e *kanbanAgentExecutor) ExecuteTask(ctx context.Context, _ string, targetAgentID string, card *kanban.Card, query string, inputs map[string]any) error {
	taskCtx := context.WithoutCancel(ctx)
	board := e.realm.Board()
	store := e.realm.deps.Store
	if store == nil || board == nil {
		return nil
	}
	if !board.Claim(card.ID, targetAgentID) {
		return fmt.Errorf("kanban: claim card %s for %s failed", card.ID, targetAgentID)
	}
	e.publishEvent(taskCtx, kanban.EventTaskClaimed, targetAgentID, kanban.TaskClaimedPayload{
		CardID:        card.ID,
		TargetAgentID: targetAgentID,
		RuntimeID:     e.realm.id,
	})
	agent, err := store.GetAgent(taskCtx, targetAgentID)
	if err != nil {
		board.Fail(card.ID, err.Error())
		e.publishEvent(taskCtx, kanban.EventTaskFailed, targetAgentID, kanban.TaskFailedPayload{
			CardID:        card.ID,
			TargetAgentID: targetAgentID,
			RuntimeID:     e.realm.id,
			Error:         err.Error(),
			ElapsedMs:     time.Since(card.CreatedAt).Milliseconds(),
		})
		e.dispatchCallback(taskCtx, card.ID, card.Producer, &kanban.ResultPayload{Error: err.Error()})
		return err
	}
	req := &workflow.Request{
		ContextID: e.realm.id + "--" + targetAgentID,
		RuntimeID: e.realm.id,
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, query),
		Inputs:    inputs,
	}
	done := e.realm.SendToAgent(taskCtx, agent, req)
	result := <-done
	if result.Err != nil {
		board.Fail(card.ID, result.Err.Error())
		e.publishEvent(taskCtx, kanban.EventTaskFailed, targetAgentID, kanban.TaskFailedPayload{
			CardID:        card.ID,
			TargetAgentID: targetAgentID,
			RuntimeID:     e.realm.id,
			Error:         result.Err.Error(),
			ElapsedMs:     time.Since(card.CreatedAt).Milliseconds(),
		})
		e.dispatchCallback(taskCtx, card.ID, card.Producer, &kanban.ResultPayload{Error: result.Err.Error()})
		return result.Err
	}
	answer := result.Value.Text()
	donePayload := e.buildDonePayload(card, query, targetAgentID, inputs, result.Value)
	board.Done(card.ID, donePayload)
	e.publishEvent(taskCtx, kanban.EventTaskCompleted, targetAgentID, kanban.TaskCompletedPayload{
		CardID:        card.ID,
		TargetAgentID: targetAgentID,
		RuntimeID:     e.realm.id,
		Output:        answer,
		ElapsedMs:     time.Since(card.CreatedAt).Milliseconds(),
	})
	e.dispatchCallback(taskCtx, card.ID, card.Producer, &kanban.ResultPayload{Output: answer})
	return nil
}

func (e *kanbanAgentExecutor) buildDonePayload(card *kanban.Card, query, targetAgentID string, inputs map[string]any, res *workflow.Result) map[string]any {
	payload := kanban.PayloadMap(card.Payload)
	if payload == nil {
		payload = make(map[string]any)
	}
	if payload["query"] == nil && query != "" {
		payload["query"] = query
	}
	if payload["target_agent_id"] == nil && targetAgentID != "" {
		payload["target_agent_id"] = targetAgentID
	}
	if payload["inputs"] == nil && len(inputs) > 0 {
		payload["inputs"] = inputs
	}
	if res != nil {
		payload["output"] = res.Text()
		if runID, ok := res.State["run_id"]; ok {
			if s, ok := runID.(string); ok && s != "" {
				payload["run_id"] = s
			}
		}
	}
	return payload
}

func (e *kanbanAgentExecutor) dispatchCallback(ctx context.Context, cardID, producerID string, result *kanban.ResultPayload) {
	store := e.realm.deps.Store
	if producerID == "" || producerID == "*" || producerID == "scheduler" || result == nil || store == nil {
		return
	}
	card, ok := e.lookupCard(cardID)
	if !ok {
		return
	}
	producer, err := store.GetAgent(ctx, producerID)
	if err != nil {
		return
	}
	callbackQuery := kanban.BuildCallbackQuery(card, result)
	e.publishEvent(ctx, kanban.EventCallbackStart, producerID, kanban.CallbackStartPayload{
		CardID:    cardID,
		RuntimeID: e.realm.id,
		AgentID:   producerID,
		Query:     callbackQuery,
	})

	req := &workflow.Request{
		ContextID: e.realm.id + "--" + producerID,
		RuntimeID: e.realm.id,
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, callbackQuery),
		Inputs:    map[string]any{model.InputKeyCallback: cardID},
	}
	var opts []ActorOption
	if producerID == model.CoPilotAgentID {
		opts = append(opts, WithPersistent())
	}
	done := e.realm.SendToAgent(ctx, producer, req, opts...)
	callbackResult := <-done
	if callbackResult.Err != nil {
		e.publishEvent(ctx, kanban.EventCallbackDone, producerID, kanban.CallbackDonePayload{
			CardID:    cardID,
			RuntimeID: e.realm.id,
			AgentID:   producerID,
			Error:     callbackResult.Err.Error(),
		})
		telemetry.Warn(ctx, "realm: callback execution failed",
			otellog.String("runtime_id", e.realm.id),
			otellog.String("producer_id", producerID),
			otellog.String("card_id", cardID),
			otellog.String("error", callbackResult.Err.Error()))
		return
	}
	e.publishEvent(ctx, kanban.EventCallbackDone, producerID, kanban.CallbackDonePayload{
		CardID:    cardID,
		RuntimeID: e.realm.id,
		AgentID:   producerID,
	})
}

func (e *kanbanAgentExecutor) lookupCard(cardID string) (*kanban.Card, bool) {
	board := e.realm.Board()
	if board == nil {
		return nil, false
	}
	card, ok := board.GetCardByID(cardID)
	if !ok {
		return nil, false
	}
	return card, true
}

func (e *kanbanAgentExecutor) publishEvent(ctx context.Context, eventType, actorID string, payload any) {
	bus := e.realm.Bus()
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, event.Event{
		Type:    event.EventType(eventType),
		ActorID: actorID,
		Payload: payload,
	})
}
