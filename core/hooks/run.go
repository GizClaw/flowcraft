package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// strippedFields are the content-bearing payload fields an untrusted
// source never sees: they carry conversation text, tool arguments, tool
// results or a subagent's instructions, none of which a plugin command
// needs in order to observe that an event happened.
var strippedFields = []string{
	"tool_input",
	"tool_result",
	"prompt",
	"command",
	"error",
	"message",
	"target",
}

// run executes one command hook with the event payload on stdin. A
// failure is logged and swallowed: see [Manager.Fire].
func (m *Manager) run(
	ctx context.Context,
	event string,
	entry groupEntry,
	hook Hook,
	payload map[string]any,
) {
	timeout := defaultTimeout
	if hook.Timeout > 0 {
		timeout = time.Duration(hook.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := json.Marshal(eventBody(event, payload, entry.trusted))
	if err != nil {
		telemetry.Warn(ctx, "hooks: encode event payload failed",
			attribute.String("event", event),
			attribute.String("error", err.Error()))
		return
	}

	cmd := exec.CommandContext(ctx, commandShell, "-c", hook.Command)
	if entry.dir != "" {
		cmd.Dir = entry.dir
	}
	cmd.Stdin = bytes.NewReader(data)
	out := &capture{limit: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	// The command gets a process group of its own, so expiry kills the
	// descendants a shell spawns as well as the shell: `sh -c` is a
	// wrapper, and the work a timeout is meant to stop is usually one
	// level down.
	cmd.SysProcAttr = processGroupAttr()
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	// The command is gone by then, but a descendant it left behind can
	// still hold the output pipe open; do not wait on that forever.
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()
	if runErr == nil {
		return
	}
	// The log has to survive the context that just expired.
	attrs := []attribute.KeyValue{
		attribute.String("event", event),
		attribute.String("command", hook.Command),
		attribute.String("error", runErr.Error()),
		attribute.String("output", out.String()),
	}
	logCtx := context.WithoutCancel(ctx)
	switch {
	case ctx.Err() != nil:
		telemetry.Warn(logCtx, "hooks: command hook did not finish in time", attrs...)
	case errors.Is(runErr, exec.ErrWaitDelay):
		telemetry.Warn(logCtx, "hooks: command hook left its output pipe open", attrs...)
	default:
		telemetry.Warn(logCtx, "hooks: command hook failed", attrs...)
	}
}

// eventBody builds the JSON object one invocation receives: the
// producer's payload plus the event name, which the runner stamps so a
// producer cannot mislabel its event. The producer's map is never
// mutated, and an untrusted source loses the content-bearing fields
// before the body is encoded.
func eventBody(
	event string, payload map[string]any, trusted bool,
) map[string]any {
	body := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		body[key] = value
	}
	body["event"] = event
	if trusted {
		return body
	}
	for _, key := range strippedFields {
		delete(body, key)
	}
	return body
}

// capture retains at most limit bytes of a hook's combined output. It
// keeps accepting writes after that — a full pipe would block the hook
// — and drops the excess, because the log line is the only consumer and
// it is bounded by the same limit.
type capture struct {
	mu        sync.Mutex
	limit     int
	buf       bytes.Buffer
	truncated bool
}

// Write implements io.Writer. The exec package copies stdout and stderr
// from separate goroutines, hence the lock.
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// String returns the captured output, marked when it was cut short.
func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.truncated {
		return c.buf.String()
	}
	return c.buf.String() + "... (truncated)"
}
