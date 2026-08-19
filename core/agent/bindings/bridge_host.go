package bindings

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/inference"
)

// StreamEmitter is the structural contract NewHostBridge accepts for
// per-node delta emission, exposed to scripts via host.emit. The
// graph executor's per-node emitter (ExecutionContext.EmitStreamDelta)
// satisfies it through a thin adapter; the bridge takes the smaller
// shape so the bindings package does not depend on core/graph.
type StreamEmitter interface {
	Emit(eventType string, payload any)
}

// NewHostBridge exposes the agent.Host control plane to scripts as the
// global "host". It is the script-side mirror of the small interfaces
// composed in agent.Host (Publisher, Interrupter, UserPrompter,
// Checkpointer, UsageReporter), with one extra method (host.emit)
// surfacing the per-node stream emitter the executor pre-baked with
// the right run / node identity.
//
// Script-facing API (every method returns nil / "" on a NoopHost so
// scripts can call it unconditionally):
//
//	host.publish(subject, payload)         -> nil | error
//	host.emit(type, payload)               -> void   (per-node stream
//	                                          delta; the envelope is
//	                                          composed by the caller
//	                                          from run / node identity)
//	host.checkInterrupt()                  -> {cause, detail} | null
//	host.askUser({ parts, schema, source, metadata })
//	                                       -> { parts, metadata }
//	host.reportUsage({ input, output, total })
//	                                       -> nil
//
// Identity (which run / which node) is intentionally NOT exposed on
// host: it belongs on a dedicated "run" global (the run-info bridge,
// pending restoration) — one source of truth for "where am I",
// separate from the host control plane.
//
// host.publish vs host.emit:
//
//   - host.publish is the low-level escape hatch. Scripts that need a
//     specific Subject (kanban callbacks, custom analytics subjects,
//     cross-run signalling) construct the subject themselves and pass
//     a full envelope payload.
//   - host.emit is the high-level node-stream channel. Callers adapt
//     it to their emitter (e.g. graph's EmitStreamDelta); payloads
//     should decode cleanly into agent.StreamDeltaPayload — passing a
//     bare value is wrapped by the adapter, not here. Unknown event
//     types carry the raw payload in the delta's "payload" field.
//
// The bridge does NOT own the agent.Host nor the StreamEmitter; the
// caller (typically a script node) feeds it whatever ctx.Host /
// emitter the executor installed and reuses those instances for all
// bindings. When emitter is nil host.emit silently drops the call.
func NewHostBridge(host agent.Host, source string, emitter StreamEmitter, opts ...HostBridgeOption) BindingFunc {
	cfg := hostBridgeConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return func(callCtx context.Context) (string, any) {
		if host == nil {
			host = agent.NoopHost{}
		}

		var (
			latchMu sync.Mutex
			latched *agent.Interrupt
		)

		// pollInterrupt does the latch-or-fetch dance once per call.
		// It deliberately uses a non-blocking select so scripts that
		// poll in a loop never freeze the script VM.
		pollInterrupt := func() *agent.Interrupt {
			latchMu.Lock()
			defer latchMu.Unlock()
			if latched != nil {
				return latched
			}
			ch := host.Interrupts()
			if ch == nil {
				return nil
			}
			select {
			case intr, ok := <-ch:
				if !ok {
					return nil
				}
				latched = &intr
				return latched
			default:
				return nil
			}
		}

		return "host", map[string]any{
			"emit": func(eventType string, payload any) {
				if emitter == nil {
					return
				}
				emitter.Emit(eventType, payload)
			},

			"publish": func(subject string, payload any) error {
				if err := validateHostPublishSubject(subject, cfg); err != nil {
					return err
				}
				env, err := event.NewEnvelope(callCtx, event.Subject(subject), payload)
				if err != nil {
					// Bad subject / unmarshallable payload — this is
					// invalid input from the script side. Classify
					// so consumers (script runtime / pod audit log)
					// can react with the same Validation handling
					// they use for other malformed bridge calls.
					return errdefs.Validationf("host.publish: %s", err.Error())
				}
				if cfg.enforceRunID && strings.HasPrefix(subject, agent.SubjectPrefix) {
					env.SetRunID(cfg.runID)
				}
				return host.Publish(callCtx, env)
			},

			"checkInterrupt": func() any {
				intr := pollInterrupt()
				if intr == nil {
					return nil
				}
				return map[string]any{
					"cause":  string(intr.Cause),
					"detail": intr.Detail,
				}
			},

			"askUser": func(raw any) (map[string]any, error) {
				prompt, err := parseUserPrompt(raw, source)
				if err != nil {
					return nil, err
				}
				reply, err := host.AskUser(callCtx, prompt)
				if err != nil {
					return nil, err
				}
				return userReplyToMap(reply)
			},

			"reportUsage": func(raw any) error {
				usage, err := parseUsage(raw)
				if err != nil {
					// parseUsage rejects malformed scripts inputs; a
					// Validation classification lets the runtime
					// surface a typed error rather than a generic
					// "host.reportUsage: ..." string.
					return errdefs.Validationf("host.reportUsage: %s", err.Error())
				}
				// Surface budget errors back to the script runtime
				// so a sandboxed script aborts instead of running on
				// past its quota; other errors are observability-only
				// per the agent.UsageReporter contract. The %w wrap
				// preserves any classification (e.g. BudgetExceeded)
				// the host attached, so errdefs.IsBudgetExceeded
				// still reports true on the returned error.
				if err := host.ReportUsage(callCtx, usage); err != nil {
					return fmt.Errorf("host.reportUsage: %w", err)
				}
				return nil
			},
		}
	}
}

type hostBridgeConfig struct {
	runID        string
	enforceRunID bool
}

// HostBridgeOption configures NewHostBridge.
type HostBridgeOption func(*hostBridgeConfig)

// WithHostRunID constrains host.publish for agent.run.* subjects to the
// current run while leaving custom non-agent subjects available.
func WithHostRunID(runID string) HostBridgeOption {
	return func(c *hostBridgeConfig) {
		c.runID = runID
		c.enforceRunID = true
	}
}

func validateHostPublishSubject(subject string, cfg hostBridgeConfig) error {
	if !cfg.enforceRunID {
		return nil
	}
	runID, ok := agentSubjectRunID(subject)
	if !ok {
		return nil
	}
	if cfg.runID == "" {
		return errdefs.Validationf("host.publish: agent subject %q requires a current run_id", subject)
	}
	want := agent.SanitiseID(cfg.runID)
	if runID != want {
		return errdefs.Validationf(
			"host.publish: agent subject run_id %q does not match current run_id %q",
			runID, want,
		)
	}
	return nil
}

func agentSubjectRunID(subject string) (string, bool) {
	if !strings.HasPrefix(subject, agent.SubjectPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(subject, agent.SubjectPrefix)
	idx := strings.IndexByte(rest, '.')
	if idx < 0 {
		return rest, true
	}
	return rest[:idx], true
}

// parseUserPrompt projects a script-supplied object onto agent.UserPrompt.
// Accepted shapes (any subset, all fields optional):
//
//	{ parts: [...], schema: "..." | bytes, source: "...", metadata: {...} }
//
// Parts round-trip through the inference JSON contract (see
// project.go), so any modality the runtime produced survives.
func parseUserPrompt(raw any, source string) (agent.UserPrompt, error) {
	prompt := agent.UserPrompt{Source: source}
	if raw == nil {
		return prompt, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return prompt, errdefs.Validationf("askUser: prompt must be an object, got %T", raw)
	}

	if rawParts, ok := m["parts"]; ok && rawParts != nil {
		parts, err := partsFromScript(rawParts, "askUser.parts")
		if err != nil {
			return prompt, err
		}
		prompt.Parts = parts
	}

	if rawSchema, ok := m["schema"]; ok && rawSchema != nil {
		switch v := rawSchema.(type) {
		case string:
			prompt.Schema = []byte(v)
		case []byte:
			prompt.Schema = v
		default:
			return prompt, errdefs.Validationf("askUser.schema: expected string or bytes, got %T", v)
		}
	}

	if rawSrc, ok := m["source"]; ok && rawSrc != nil {
		s, ok := rawSrc.(string)
		if !ok {
			return prompt, errdefs.Validationf("askUser.source: expected string, got %T", rawSrc)
		}
		// Caller-supplied source overrides the bridge default so a
		// script can attribute the prompt to a sub-step of itself.
		prompt.Source = s
	}

	if rawMeta, ok := m["metadata"]; ok && rawMeta != nil {
		meta, err := parseStringMap(rawMeta, "askUser.metadata")
		if err != nil {
			return prompt, err
		}
		prompt.Metadata = meta
	}

	return prompt, nil
}

func userReplyToMap(reply agent.UserReply) (map[string]any, error) {
	parts, err := partsToScript(reply.Parts)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"parts": parts,
	}
	if len(reply.Metadata) > 0 {
		meta := make(map[string]any, len(reply.Metadata))
		for k, v := range reply.Metadata {
			meta[k] = v
		}
		out["metadata"] = meta
	}
	return out, nil
}

// parseUsage maps {input, output, total} (any of which may be missing)
// onto an inference.Usage. Numbers may arrive as float64 (the JSON
// default in script VMs) or int64; both are folded down to int64 here.
func parseUsage(raw any) (inference.Usage, error) {
	var usage inference.Usage
	if raw == nil {
		return usage, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return usage, errdefs.Validationf("usage: expected object, got %T", raw)
	}
	if v, ok := m["input"]; ok {
		n, err := asInt64(v, "input")
		if err != nil {
			return usage, err
		}
		usage.InputTokens = n
	}
	if v, ok := m["output"]; ok {
		n, err := asInt64(v, "output")
		if err != nil {
			return usage, err
		}
		usage.OutputTokens = n
	}
	if v, ok := m["total"]; ok {
		n, err := asInt64(v, "total")
		if err != nil {
			return usage, err
		}
		usage.TotalTokens = n
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, nil
}

func asInt64(v any, field string) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	case float64:
		return int64(n), nil
	default:
		return 0, errdefs.Validationf("usage.%s: expected number, got %T", field, v)
	}
}

func parseStringMap(raw any, field string) (map[string]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errdefs.Validationf("%s: expected object, got %T", field, raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, errdefs.Validationf("%s.%s: expected string, got %T", field, k, v)
		}
		out[k] = s
	}
	return out, nil
}
