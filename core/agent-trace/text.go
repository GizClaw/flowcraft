package agenttrace

import (
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
)

// RenderOption configures [WriteText].
type RenderOption func(*renderConfig)

type renderConfig struct {
	width int
	kinds map[string]struct{}
}

// WithWidth clips one line to n characters. Zero uses the default.
func WithWidth(n int) RenderOption {
	return func(c *renderConfig) { c.width = n }
}

// WithKinds renders only these record kinds, which is what an operator asks for
// when following one thread ("only the model calls").
func WithKinds(kinds ...string) RenderOption {
	return func(c *renderConfig) {
		c.kinds = make(map[string]struct{}, len(kinds))
		for _, kind := range kinds {
			c.kinds[kind] = struct{}{}
		}
	}
}

// WriteText renders a timeline for a person.
//
// It is the operator's half of this package: one line per fact under the step
// that produced it, with the time it happened. A fact with no time (a row
// written before events carried one) prints "?" rather than a made-up clock: a
// wrong time is harder to notice than a missing one.
func WriteText(w io.Writer, t *Trace, opts ...RenderOption) error {
	settings := renderConfig{width: 120}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if settings.width <= 0 {
		settings.width = 120
	}
	headline := fmt.Sprintf("task %s  %s  %d facts",
		t.Task.ID, t.Task.State, len(t.Facts))
	if t.Task.SessionID != "" && t.Task.SessionID != t.Task.ID {
		headline += "  session " + t.Task.SessionID
	}
	if id := t.Manifest.ID(); id != "" {
		headline += "  manifest " + clip(id, 20)
	}
	if t.Projection.ContextHash != "" {
		headline += "  context " + clip(t.Projection.ContextHash, 20)
	}
	if _, err := fmt.Fprintln(w, headline); err != nil {
		return err
	}
	for _, step := range t.Steps {
		mode := "live"
		if step.Replay {
			mode = "replayed"
		}
		where := "seed"
		if step.Ordinal > 0 {
			where = fmt.Sprintf("step %d", step.Ordinal)
		}
		nodes := ""
		if len(step.Nodes) > 0 {
			nodes = "  " + strings.Join(step.Nodes, ", ")
		}
		if _, err := fmt.Fprintf(w, "\n%s%s  (%s)\n", where, nodes, mode); err != nil {
			return err
		}
		for _, fact := range step.Facts {
			if settings.kinds != nil {
				if _, wanted := settings.kinds[fact.Kind]; !wanted {
					continue
				}
			}
			// The marker is the step's heading, not a fact of its own.
			if _, isMarker := fact.Value.(graph.FrontierRecord); isMarker {
				continue
			}
			if _, err := fmt.Fprintf(w, "  %s  %s\n", at(fact), summarize(fact, settings.width)); err != nil {
				return err
			}
		}
	}
	return nil
}

// at renders the fact's time, or "?" when the store could not date it.
func at(fact Fact) string {
	if fact.At.IsZero() {
		return "?          "
	}
	return fact.At.Format("15:04:05.000")
}

// summarize renders one fact as a line: the payload's own vocabulary when a
// decoder claimed it, the raw bytes otherwise.
func summarize(fact Fact, width int) string {
	switch value := fact.Value.(type) {
	case agent.LLMCallRecord:
		return summarizeCall(value, width)
	case agent.EffectRecord:
		return fmt.Sprintf("%-16s %s %-7s %4dms  args=%s",
			"effect", value.ToolName, value.Status, value.DurationMs, clip(value.ArgsHash, 14))
	case graph.StateWrite:
		return fmt.Sprintf("%-16s %-10s %s", "state", value.Key, summarizeValue(value.Value, width))
	case graph.BaseRecord:
		return fmt.Sprintf("%-16s step %d  blob %s", "base", value.Step, clip(value.Blob, 16))
	case script.RunRecord:
		mode := "live"
		if value.Replayed {
			mode = "replayed"
		}
		return fmt.Sprintf("%-16s %s  v=%s  %s", "script.run", clip(value.Ref, 24), clip(value.Version, 12), mode)
	case script.ToolRecord:
		status := "ok"
		if !value.OK {
			status = "failed"
		}
		return fmt.Sprintf("%-16s %s#%d %s  %s  args=%s",
			"script.tool", value.Name, value.Index, status,
			clip(oneLine(value.Text), width/2), clip(value.ArgumentsHash, 14))
	case message.Message:
		return fmt.Sprintf("%-16s %-9s %s", "message", value.Role, clip(oneLine(value.Content.Text()), width/2))
	}
	return fmt.Sprintf("%-16s %s", fact.Kind, clip(oneLine(string(fact.Payload)), width))
}

// summarizeCall renders one model call: what answered, what it cost, and the
// request's identity, which is what a replay looks it up by.
func summarizeCall(call agent.LLMCallRecord, width int) string {
	model := call.Model.ID.Name
	if resolved := call.Metadata.Model.Name; resolved != "" && resolved != model {
		model += " → " + resolved
	}
	line := fmt.Sprintf("%-16s %-9s %s  in=%d out=%d  %dms",
		"llm.call", call.Model.ID.Provider, clip(model, 24),
		call.Usage.InputTokens, call.Usage.OutputTokens, call.LatencyMs)
	if call.ErrorKind != "" {
		line += "  " + call.ErrorKind
	}
	if call.ResponseRef != "" || len(call.Response) > 0 {
		line += "  response"
	}
	if call.RequestHash != "" {
		line += "  " + clip(call.RequestHash, 20)
	}
	if call.Metadata.RequestID != "" {
		line += "  req=" + clip(call.Metadata.RequestID, 16)
	}
	return line
}

// summarizeValue renders one state value: the shape the key held, not the whole
// document, because a long run's transcript would otherwise fill the page.
func summarizeValue(value graph.EncodedValue, width int) string {
	switch value.Kind {
	case graph.KindText:
		return clip(oneLine(value.Text), width/2)
	case graph.KindBool:
		return fmt.Sprintf("%t", value.Bool)
	case graph.KindMessages:
		if len(value.Messages) == 0 {
			return "messages(0)"
		}
		last := value.Messages[len(value.Messages)-1]
		return fmt.Sprintf("messages(%d)  %s: %s", len(value.Messages), last.Role,
			clip(oneLine(last.Content.Text()), width/2))
	case graph.KindImage, graph.KindAudio, graph.KindVideo, graph.KindFile:
		return fmt.Sprintf("%s(%s) %s", value.Kind, value.MediaType, clip(value.Blob, 16))
	}
	return clip(oneLine(string(value.JSON)), width/2)
}

// oneLine folds a multi-line payload so one fact stays one line.
func oneLine(text string) string {
	replaced := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(replaced.Replace(text))
}

// clip shortens a string to n characters, marking that it did.
func clip(text string, n int) string {
	if n <= 0 || len(text) <= n {
		return text
	}
	if n <= 1 {
		return text[:n]
	}
	return text[:n-1] + "…"
}
