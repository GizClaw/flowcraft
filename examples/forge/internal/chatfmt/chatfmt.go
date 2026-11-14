// Package chatfmt turns one turn's event stream into the blocks a person reads:
// speech, a tool call, and its result. TUI and scripted test logs share it, so a
// transcript written by `forge test` is the same text the interactive session
// showed.
//
// A block records the graph node that produced it, because the engine brackets every
// invocation on the same stream: that is how a scheduler's control line is told from
// a line someone said. Speaker attribution still belongs to agents - a seat's words
// arrive under its own name.
package chatfmt

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
)

// BlockKind names what one block is.
type BlockKind string

const (
	// Speech is streamed assistant text.
	Speech BlockKind = "speech"
	// ToolCall is a tool the model asked to run.
	ToolCall BlockKind = "tool_call"
	// ToolResult is what that tool answered.
	ToolResult BlockKind = "tool_result"
)

// Block is one output segment of a turn.
type Block struct {
	Kind BlockKind
	// Label names the agent that produced the block, when the caller knows it: a
	// seat's line is shown under its label, the turn's own agent under none.
	Label string
	// Name is the tool name; empty for speech.
	Name string
	// Text is the speech, the call arguments, or the tool's answer.
	Text strings.Builder
	// Node is the graph node that produced the block, when the engine reported it.
	Node string
	// Phase is the game phase the block belonged to, when the caller knows it.
	Phase string
	// Private marks a line only the role acting in its phase could hear.
	Private bool
}

// Collector folds a turn's events into blocks.
type Collector struct {
	blocks  []*Block
	current *Block
	calls   map[string]string
	tokens  int
	label   string
	phase   string
	private bool
	// node is the graph node the engine is running, as its own event reports it.
	node string
	// silent reports a node whose text is not for the reader.
	silent func(node string) bool
}

// SetSpeaker labels the blocks that follow. The caller sets it when it switches to
// another agent's stream; an empty label is the turn's own agent.
func (c *Collector) SetSpeaker(label string) {
	if c.label == label {
		return
	}
	c.label = label
	c.current = nil
}

// SetSilent names the nodes whose text stays off the transcript: the engine reports
// which node is running, and a node the deployment calls silent is a decision, not a
// line someone said.
func (c *Collector) SetSilent(silent func(node string) bool) {
	c.silent = silent
}

// SetPhase tells the collector which phase the following lines belong to, and
// whether only the role acting in it could hear them. Both are the state the graph
// last reported; the collector decides nothing about a game.
func (c *Collector) SetPhase(phase string, private bool) {
	if c.phase == phase && c.private == private {
		return
	}
	c.phase, c.private = phase, private
	c.current = nil
}

// NewCollector returns an empty collector.
func NewCollector() *Collector {
	return &Collector{calls: make(map[string]string)}
}

// Observe folds one event in. Events that carry no renderable content (status,
// usage, artifacts) are ignored: they are the caller's accounting, not speech.
func (c *Collector) Observe(event agent.Event) {
	switch typed := event.(type) {
	case agent.EventExtension[graph.NodeEvent]:
		// The engine brackets every invocation, so the text that follows it belongs
		// to that node - which is how a control line can be told from a speech act.
		c.node = typed.Value.Node
		return
	case agent.Text:
		if c.silent != nil && c.silent(c.node) {
			return
		}
		c.tokens++
		if c.current == nil || c.current.Kind != Speech {
			c.current = &Block{Kind: Speech, Label: c.label, Node: c.node, Phase: c.phase, Private: c.private}
			c.blocks = append(c.blocks, c.current)
		}
		c.current.Text.WriteString(typed.Text)
	case agent.ToolCallStart:
		c.calls[typed.Call.ID] = typed.Call.Name
		block := &Block{Kind: ToolCall, Label: c.label, Name: typed.Call.Name, Phase: c.phase, Private: c.private}
		block.Text.WriteString(typed.Call.Arguments.String())
		c.blocks = append(c.blocks, block)
		// A tool block never merges with the speech around it: the answer that
		// follows runs the tool for a reason the tool's own output explains.
		c.current = nil
	case agent.ToolResult:
		name := c.calls[typed.Result.CallID]
		if name == "" {
			name = typed.Result.CallID
		}
		block := &Block{Kind: ToolResult, Label: c.label, Name: name, Phase: c.phase, Private: c.private}
		block.Text.WriteString(typed.Result.Content.Text())
		c.blocks = append(c.blocks, block)
		c.current = nil
	}
}

// Blocks returns the collected blocks.
func (c *Collector) Blocks() []*Block { return c.blocks }

// Tokens is how many text increments the turn streamed. It is a liveness
// signal for tests, not a token count.
func (c *Collector) Tokens() int { return c.tokens }

// Text is the turn's speech with tool blocks dropped, which is what a
// simulation feeds to the other agent.
func (c *Collector) Text() string {
	var out strings.Builder
	for _, block := range c.blocks {
		if block.Kind == Speech {
			out.WriteString(block.Text.String())
		}
	}
	return out.String()
}

// Render writes the turn as transcript text.
func Render(blocks []*Block) string {
	lines := make([]string, 0, len(blocks))
	for _, block := range blocks {
		text := strings.TrimSpace(block.Text.String())
		switch block.Kind {
		case Speech:
			if text != "" {
				if block.Label != "" {
					text = "[" + block.Label + "] " + text
				}
				// The badge stays outermost: a reader scanning the transcript sees
				// visibility before the name.
				if block.Private {
					text = "[私密] " + text
				}
				lines = append(lines, text)
			}
		case ToolCall:
			lines = append(lines, "[工具调用] "+block.Name+" "+text)
		case ToolResult:
			lines = append(lines, "[工具结果] "+block.Name+": "+text)
		default:
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

// ResultText is the final assistant message a result carries, or "" when the
// turn produced none: a failed or paused task has no answer.
func ResultText(result agent.Result) string {
	if result.Content.Parts == nil {
		return ""
	}
	return strings.TrimSpace(result.Content.Text())
}

// CallName resolves the tool name of a call id the collector has seen.
func (c *Collector) CallName(id string) string { return c.calls[id] }
