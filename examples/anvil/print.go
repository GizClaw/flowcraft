package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/event"
)

// printer serializes the tour's output. It has to be safe for concurrent
// use because craft-plane events arrive on the craft's own goroutines
// while the tour prints on its own.
type printer struct {
	mu  sync.Mutex
	out io.Writer
}

func newPrinter(out io.Writer) *printer { return &printer{out: out} }

// step starts one tour section.
func (p *printer) step(format string, args ...any) {
	p.printf("\n== "+format+"\n", args...)
}

// line prints one indented body line.
func (p *printer) line(format string, args ...any) {
	p.printf("   "+format+"\n", args...)
}

// event prints one craft-plane event as it arrives.
func (p *printer) event(envelope event.Envelope) {
	if payloadIsEmpty(envelope.Payload) {
		p.printf("   event  %s\n", envelope.Subject)
		return
	}
	p.printf("   event  %s  %s\n", envelope.Subject, compactJSON(envelope.Payload))
}

func (p *printer) printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A failed write is not actionable: the tour narrates to stdout (or
	// to a test's buffer) and has nothing to do about it.
	_, _ = fmt.Fprintf(p.out, format, args...)
}

// compactJSON renders a raw payload on one line, or "" when there is
// nothing worth showing.
func compactJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buffer.String()
}

// payloadIsEmpty reports a payload with nothing to say: {} or an object
// whose values are all empty. craft.started carries an empty
// RuntimeEvent, whose only field renders as {"key":""}.
func payloadIsEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	var decoded map[string]any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	for _, value := range decoded {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		return false
	}
	return true
}
