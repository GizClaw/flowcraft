package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/internal/stream"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func sseWriteEvent(w http.ResponseWriter, eventType string, data any) {
	raw, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(raw))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func sseWriteError(w http.ResponseWriter, code, message string) {
	sseWriteEvent(w, "error", map[string]string{"code": code, "message": message})
}

type sseSink struct {
	w http.ResponseWriter
}

func (s *sseSink) Send(ev stream.MappedEvent) error {
	sseWriteEvent(s.w, ev.Type, ev.Payload)
	return nil
}

func (s *sseSink) Done(result *workflow.Result) error {
	sseWriteEvent(s.w, "done", result)
	return nil
}

func (s *sseSink) Error(code, message string) error {
	sseWriteError(s.w, code, message)
	return nil
}

type wsSink struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *wsSink) Send(ev stream.MappedEvent) error {
	flat := make(map[string]any, len(ev.Payload)+1)
	for k, v := range ev.Payload {
		flat[k] = v
	}
	flat["type"] = ev.Type
	return wsjson.Write(s.ctx, s.conn, flat)
}

func (s *wsSink) Done(result *workflow.Result) error {
	raw, _ := json.Marshal(result)
	var flat map[string]any
	_ = json.Unmarshal(raw, &flat)
	if flat == nil {
		flat = make(map[string]any)
	}
	flat["type"] = "done"
	return wsjson.Write(s.ctx, s.conn, flat)
}

func (s *wsSink) Error(code, message string) error {
	return wsjson.Write(s.ctx, s.conn, map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	})
}
