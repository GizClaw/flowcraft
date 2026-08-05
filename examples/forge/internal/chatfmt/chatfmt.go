// Package chatfmt splits a streamed assistant turn into per-node
// blocks using the graph node id stamped on token stream deltas, and
// renders speech and tool blocks with caller-supplied labels. Labels
// come from the scenario (speakers.yaml), never from this package, so
// the formatting stays generic. TUI and scripted test logs share it.
package chatfmt

import "strings"

// Block is one output segment produced by a single graph node: either
// streamed speech (Tool empty) or a tool use (Tool set).
type Block struct {
	NodeID string
	Tool   string // tool name for tool-use blocks
	Result bool   // true for tool-result blocks, false for tool-call blocks
	Text   strings.Builder
}

// Collector accumulates streamed deltas into blocks. Tokens with an
// empty node id join the current speech block.
type Collector struct {
	current *Block
	Blocks  []*Block
}

// Token appends one text increment, starting a new speech block when
// the node id changes.
func (c *Collector) Token(nodeID, text string) {
	if nodeID == "" && c.current != nil {
		c.current.Text.WriteString(text)
		return
	}
	if c.current == nil || c.current.NodeID != nodeID {
		c.current = &Block{NodeID: nodeID}
		c.Blocks = append(c.Blocks, c.current)
	}
	c.current.Text.WriteString(text)
}

// ToolCall records one model-requested tool invocation.
func (c *Collector) ToolCall(name, args string) {
	b := &Block{Tool: name}
	b.Text.WriteString(args)
	c.Blocks = append(c.Blocks, b)
	c.current = nil // a tool block never merges with following speech
}

// ToolResult records one completed tool invocation.
func (c *Collector) ToolResult(name, content string) {
	b := &Block{Tool: name, Result: true}
	b.Text.WriteString(content)
	c.Blocks = append(c.Blocks, b)
	c.current = nil
}

// Text returns the raw concatenated speech text (no labels, no tool
// blocks), for token counts and as dialogue input.
func (c *Collector) Text() string {
	var b strings.Builder
	for _, block := range c.Blocks {
		if block.Tool == "" {
			b.WriteString(block.Text.String())
		}
	}
	return b.String()
}

// Render returns one line per block. Speech blocks are prefixed with
// the label resolved by label; tool blocks use fixed tool markers.
func Render(blocks []*Block, label func(nodeID string) string) string {
	var b strings.Builder
	for i, block := range blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		body := block.Text.String()
		switch {
		case block.Tool != "" && block.Result:
			b.WriteString("[工具结果] " + block.Tool)
			if body != "" {
				b.WriteString(": " + body)
			}
		case block.Tool != "":
			b.WriteString("[工具调用] " + block.Tool)
			if body != "" {
				b.WriteString(" " + body)
			}
		default:
			if l := label(block.NodeID); l != "" {
				b.WriteString("[" + l + "] ")
			}
			b.WriteString(body)
		}
	}
	return b.String()
}
