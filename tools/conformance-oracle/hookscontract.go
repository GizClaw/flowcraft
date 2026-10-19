package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/GizClaw/flowcraft/core/hooks"
)

// The hooks contract: what a hooks.json loads as, and what firing an event does
// to the commands it names.
//
// core/hooks has two halves. The load half is a parser with opinions -- one
// vocabulary, keys read in sorted order, groups that survive only if a hook in
// them survives, and one error sentence per way of getting it wrong. The run half
// is a transport: one JSON object on stdin, `sh -c` in the source's directory, a
// timeout with a process group behind it, and a warning instead of an error when
// any of that fails.
//
// Both halves are rows here, because both are things a port can get wrong
// quietly. A row records:
//
//	config         -- the primary hooks.json, verbatim; empty means "no file"
//	extras         -- the sources a caller contributed, each with its own file
//	fires          -- event + payload, then the commands that ran and the body
//	                  each was given, read back from a trace file the commands
//	                  themselves append to
//	error, empty   -- the load's answer; exactly one of them on a row that loaded
//	load_warnings  -- what the runner logged while loading (a contributed source
//	                  that did not parse is skipped, not raised)
//
// Four normalizations make rows comparable, and each one is the difference
// between a contract and a transcript:
//
//   - The harness's own directory is masked out of every string. Rows run in a
//     temporary directory (the primary file's commands run in the process's
//     working directory, which is why the harness changes into it), so the
//     absolute path differs on every run and in every implementation. What is
//     left is the logical name the row spells: `hooks.json`, `a.json`,
//     `trace.part`.
//   - A regular expression's error text is masked after the sentence in front of
//     it. Go's `regexp` and Rust's `regex` are the same engine family -- both
//     RE2, both linear time, which is what keeps a user-supplied matcher from
//     backtracking -- but they word a syntax error differently, and pinning the
//     wording would pin the library instead of the behavior.
//   - A JSON parse error's tail is masked for the same reason: `encoding/json`
//     and `serde_json` both say what is wrong and where, in their own words.
//   - A warning's `error` attribute is masked. Everything else a warning carries
//     -- the event, the command, the output -- is the runner's own text and is
//     compared exactly, because that is what an operator reads.
//
// The timeout row is the only one with a clock in it. Its command spawns a
// descendant that would write a file after the hook's timeout has passed, and the
// row records whether that file appeared: the evidence that the *group* was
// killed rather than the shell alone.

// hooksContract is the committed document.
type hooksContract struct {
	// Note says what the table is for, in the file a reader opens first.
	Note []string `json:"note"`
	// Events is the vocabulary in canonical order, from core itself.
	Events []string `json:"events"`
	// Masked names what a reader must not expect to compare.
	Masked []string `json:"masked"`
	// Rows are the scripted loads and fires.
	Rows []hooksContractRow `json:"rows"`
}

// hooksContractRow is one hooks.json, the sources beside it, and the events fired
// at it.
type hooksContractRow struct {
	// Name identifies the row in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Config is the primary hooks.json's text. Empty means the file is absent.
	Config string `json:"config,omitempty"`
	// Extras are the contributed sources, loaded after the primary.
	Extras []hooksContractExtra `json:"extras,omitempty"`
	// Fires are the occurrences, in order.
	Fires []hooksContractFire `json:"fires,omitempty"`
	// Error is the load's refusal, when it refused.
	Error *string `json:"error,omitempty"`
	// Empty is the load's answer when it did not refuse. A pointer because
	// "loaded nothing" and "did not load" are different answers.
	Empty *bool `json:"empty,omitempty"`
	// LoadWarnings are what the runner logged while loading.
	LoadWarnings []hooksContractWarning `json:"load_warnings,omitempty"`
	// DescendantKilled is set only by the timeout row: whether a process the
	// hook left behind survived the kill, judged by a file it would have written.
	DescendantKilled *bool `json:"descendant_killed,omitempty"`

	// descendant marks the row that checks a killed process group.
	descendant bool
}

// hooksContractExtra is one contributed source.
type hooksContractExtra struct {
	// Path is the file's name, relative to the harness's directory.
	Path string `json:"path"`
	// Dir is the working directory its commands run in, as a logical name: "."
	// is the harness's directory, "" is the process's own.
	Dir string `json:"dir"`
	// Config is the file's text.
	Config string `json:"config"`
	// Trusted says whether its commands may see the content-bearing fields.
	Trusted bool `json:"trusted"`
}

// hooksContractFire is one occurrence and everything it did.
type hooksContractFire struct {
	// Event is the name fired.
	Event string `json:"event"`
	// Payload is the producer's map, strings only: every field core documents for
	// an event is a string, and a nested value would only test JSON.
	Payload map[string]string `json:"payload"`
	// Trace is every command that ran, in order, with the body it received.
	Trace []hooksContractCall `json:"trace"`
	// Warnings are what the runner logged during this fire.
	Warnings []hooksContractWarning `json:"warnings,omitempty"`
}

// hooksContractCall is one hook invocation, as the command itself recorded it.
type hooksContractCall struct {
	// Label is the name the command wrote before its stdin.
	Label string `json:"label"`
	// Body is the JSON object on stdin, parsed, so the dump compares fields
	// rather than key order (which both encoders already sort).
	Body map[string]any `json:"body"`
}

// hooksContractWarning is one log line: a message and its attributes.
type hooksContractWarning struct {
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// hooksTraceFile is where every recorder command appends.
const hooksTraceFile = "trace.part"

// writeHooksContract dumps the contract to w.
func writeHooksContract(w io.Writer) error {
	document, err := hooksDocument()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// hooksDocument runs every row and returns the document.
func hooksDocument() (hooksContract, error) {
	rows, err := runHooksRows(hooksRows())
	if err != nil {
		return hooksContract{}, err
	}
	return hooksContract{
		Note: []string{
			"core/hooks, as a table: what a hooks.json loads as, and what firing an event does to the commands it names.",
			"Each row runs in a temporary directory of the harness's own, so its absolute path is masked out of every string; what is left is the logical name the row spells.",
			"A regular expression's or a JSON document's own error text is masked: Go and Rust word both differently, and a contract that pinned the wording would pin the library.",
			"A warning's error attribute is masked for the same reason; its message, event, command and output are compared exactly.",
			"The timeout row is the only one with a clock in it: it records whether a process the hook left behind survived the kill.",
		},
		Events: hooks.EventNames(),
		Masked: []string{
			"the harness's directory, in every string",
			"a matcher's or a hooks.json's parse error, after the sentence that classifies it",
			"a warning's error attribute",
		},
		Rows: rows,
	}, nil
}

// hooksRecorder is the command every firing row uses: it appends its own label and
// then its stdin to the trace file, one `label:{json}` line per invocation.
//
// The label is what makes order observable without a clock; the body is what
// makes trust observable without reading the runner's memory.
func hooksRecorder(label string) string {
	return fmt.Sprintf("printf '%s:' >> %s; cat >> %s; printf '\\n' >> %s",
		label, hooksTraceFile, hooksTraceFile, hooksTraceFile)
}

// config builds a hooks.json whose event has one group per (matcher, command)
// pair, in order.
func config(event string, groups ...[2]string) string {
	entries := make([]string, 0, len(groups))
	for _, group := range groups {
		entries = append(entries, fmt.Sprintf(`{"matcher":%q,"hooks":[{"command":%q}]}`, group[0], group[1]))
	}
	return fmt.Sprintf(`{"hooks":{%q:[%s]}}`, event, strings.Join(entries, ","))
}

// hooksRows is the table. Every row spells its own files, because a row is a
// directory of scripts rather than a string.
func hooksRows() []hooksContractRow {
	return []hooksContractRow{
		{
			Name:   "load-empty-file",
			Why:    "an empty hooks.json is a runner with no groups, not a refusal",
			Config: `{}`,
		},
		{
			Name: "load-missing-file",
			Why:  "a deployment with no hooks.json still starts",
		},
		{
			Name:   "unknown-event",
			Why:    "the vocabulary is closed: an unknown key is a validation failure whose sentence lists what is known",
			Config: `{"hooks":{"PreToolUse":[],"Bogus":[]}}`,
		},
		{
			Name:   "unknown-event-sorted",
			Why:    "the key named is the first in sorted order, so two faults report the same one every time",
			Config: `{"hooks":{"Zed":[],"Aardvark":[]}}`,
		},
		{
			Name:   "bad-matcher",
			Why:    "a matcher that does not compile is refused with its group's coordinates",
			Config: `{"hooks":{"PreToolUse":[{"matcher":"[","hooks":[{"command":"true"}]}]}}`,
		},
		{
			Name:   "empty-command",
			Why:    "a command hook with no command is refused rather than loaded as a no-op",
			Config: `{"hooks":{"PreToolUse":[{"hooks":[{"command":"   "}]}]}}`,
		},
		{
			Name:   "unknown-kind-only",
			Why:    "a hook kind this runner does not run is dropped, and a group left with no hooks is not a group",
			Config: `{"hooks":{"PreToolUse":[{"hooks":[{"type":"webhook","command":"true"}]}]}}`,
		},
		{
			Name:   "unknown-kind-mixed",
			Why:    "the same group keeps the command hook beside the kind the runner does not know",
			Config: `{"hooks":{"TurnEnd":[{"hooks":[{"type":"webhook","command":"true"},{"command":"` + hooksRecorder("kept") + `"}]}]}}`,
			Fires:  []hooksContractFire{{Event: "TurnEnd", Payload: map[string]string{}}},
		},
		{
			Name: "matcher-wildcard",
			Why:  "empty and * both match everything, and every matching group runs in load order",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("PreToolUse", [2]string{"", hooksRecorder("empty")}, [2]string{"*", hooksRecorder("star")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{
				{Event: "PreToolUse", Payload: map[string]string{"tool": "Bash"}},
				{Event: "PreToolUse", Payload: map[string]string{"tool": "Read"}},
			},
		},
		{
			Name: "matcher-regex",
			Why:  "a matcher is a regular expression over the occurrence's match value",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("PreToolUse", [2]string{"^(WebFetch|WebSearch)$", hooksRecorder("web")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{
				{Event: "PreToolUse", Payload: map[string]string{"tool": "WebFetch"}},
				{Event: "PreToolUse", Payload: map[string]string{"tool": "WebSearch"}},
				{Event: "PreToolUse", Payload: map[string]string{"tool": "Read"}},
			},
		},
		{
			Name: "match-value-skips-empty",
			Why:  "the match value is the first non-empty of tool, source, reason, subagent",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("UserPromptSubmit", [2]string{"^cli$", hooksRecorder("cli")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{{
				Event:   "UserPromptSubmit",
				Payload: map[string]string{"tool": "", "source": "cli", "prompt": "hello"},
			}},
		},
		{
			Name: "match-value-empty-declines",
			Why:  "an occurrence with no match field matches only a group that names no matcher",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("TurnEnd", [2]string{".+", hooksRecorder("wants-a-value")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{{Event: "TurnEnd", Payload: map[string]string{"status": "done"}}},
		},
		{
			Name:   "primary-parse-error",
			Why:    "a primary hooks.json that does not parse is refused, rather than loading as nothing",
			Config: `{"hooks":{"TurnEnd":`,
		},
		{
			Name: "broken-source-is-skipped",
			Why:  "a contributed source that does not parse is skipped with a warning, and the source after it still loads",
			Extras: []hooksContractExtra{
				{Path: "broken.json", Dir: ".", Config: `{"hooks":{"TurnEnd":`, Trusted: true},
				{Path: "good.json", Dir: ".", Config: config("TurnEnd", [2]string{"", hooksRecorder("kept")}), Trusted: true},
			},
			Fires: []hooksContractFire{{Event: "TurnEnd", Payload: map[string]string{}}},
		},
		{
			Name: "untrusted-strips-content",
			Why:  "a plugin's command sees that an event happened, never the content it carried",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("PostToolUse", [2]string{"", hooksRecorder("plugin")}),
				Trusted: false,
			}},
			Fires: []hooksContractFire{{
				Event: "PostToolUse",
				Payload: map[string]string{
					"tool": "Bash", "tool_input": "secret", "tool_result": "secret",
					"prompt": "secret", "command": "secret", "error": "secret",
					"message": "secret", "target": "secret", "status": "ok",
				},
			}},
		},
		{
			Name: "trusted-keeps-content",
			Why:  "the same payload reaches a source the host marked trusted",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("PostToolUse", [2]string{"", hooksRecorder("host")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{{
				Event: "PostToolUse",
				Payload: map[string]string{
					"tool": "Bash", "tool_input": "secret", "tool_result": "secret",
					"prompt": "secret", "command": "secret", "error": "secret",
					"message": "secret", "target": "secret", "status": "ok",
				},
			}},
		},
		{
			Name: "stamped-event",
			Why:  "the runner stamps the event, so a producer cannot mislabel the object a command reads",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("TurnEnd", [2]string{"", hooksRecorder("stamped")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{{Event: "TurnEnd", Payload: map[string]string{"event": "Bogus"}}},
		},
		{
			Name: "two-sources-order",
			Why:  "sources append in load order, and a source's hooks run in declaration order",
			Extras: []hooksContractExtra{
				{Path: "a.json", Dir: ".", Config: config("SessionStart", [2]string{"", hooksRecorder("first")}), Trusted: true},
				{Path: "b.json", Dir: ".", Config: config("SessionStart", [2]string{"", hooksRecorder("second")}), Trusted: true},
			},
			Fires: []hooksContractFire{{Event: "SessionStart", Payload: map[string]string{"source": "cli"}}},
		},
		{
			Name: "failing-hook-continues",
			Why:  "a hook that exits non-zero is logged and skipped, and the next one still runs",
			Extras: []hooksContractExtra{{
				Path: "plugin.json",
				Dir:  ".",
				Config: config("TurnEnd",
					[2]string{"", hooksRecorder("failing") + "; echo boom >&2; exit 3"},
					[2]string{"", hooksRecorder("next")},
				),
				Trusted: true,
			}},
			Fires: []hooksContractFire{{Event: "TurnEnd", Payload: map[string]string{}}},
		},
		{
			Name: "timeout-kills-group",
			Why:  "a hook that overruns is killed with the group it started, because sh -c is a wrapper",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  `{"hooks":{"SessionStart":[{"hooks":[{"command":"(sleep 2; printf late > late.part) & sleep 5","timeout":1}]}]}}`,
				Trusted: true,
			}},
			Fires:      []hooksContractFire{{Event: "SessionStart", Payload: map[string]string{}}},
			descendant: true,
		},
		{
			Name: "repeated-fire",
			Why:  "firing the same event twice runs the hooks twice: the runner keeps no state",
			Extras: []hooksContractExtra{{
				Path:    "plugin.json",
				Dir:     ".",
				Config:  config("SessionEnd", [2]string{"", hooksRecorder("again")}),
				Trusted: true,
			}},
			Fires: []hooksContractFire{
				{Event: "SessionEnd", Payload: map[string]string{}},
				{Event: "SessionEnd", Payload: map[string]string{}},
			},
		},
	}
}

// runHooksRows executes every row in one temporary directory, changing into it so
// that a primary source's commands -- whose working directory is the process's,
// not a source's -- write next to the extras'.
func runHooksRows(rows []hooksContractRow) ([]hooksContractRow, error) {
	directory, err := os.MkdirTemp("", "hooks-contract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)

	previous, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(directory); err != nil {
		return nil, err
	}
	defer os.Chdir(previous)

	out := make([]hooksContractRow, 0, len(rows))
	for _, row := range rows {
		ran, err := runHooksRow(row, directory)
		if err != nil {
			return nil, fmt.Errorf("row %s: %w", row.Name, err)
		}
		out = append(out, ran)
	}
	return out, nil
}

// runHooksRow materializes one row's files, loads them, and fires what it says.
func runHooksRow(row hooksContractRow, directory string) (hooksContractRow, error) {
	if err := clearHooksTrace(directory); err != nil {
		return row, err
	}
	primary := filepath.Join(directory, "hooks.json")
	if row.Config == "" {
		primary = filepath.Join(directory, "absent.json")
	} else if err := os.WriteFile(primary, []byte(row.Config), 0o600); err != nil {
		return row, err
	}

	extras := make([]hooks.ExtraSource, 0, len(row.Extras))
	for _, extra := range row.Extras {
		path := filepath.Join(directory, extra.Path)
		if err := os.WriteFile(path, []byte(extra.Config), 0o600); err != nil {
			return row, err
		}
		dir := ""
		switch extra.Dir {
		case ".":
			dir = directory
		case "":
		default:
			dir = filepath.Join(directory, extra.Dir)
		}
		extras = append(extras, hooks.ExtraSource{Path: path, Dir: dir, Trusted: extra.Trusted})
	}

	logs := &hooksLogRecorder{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	previous := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(provider)
	defer func() {
		logglobal.SetLoggerProvider(previous)
		_ = provider.Shutdown(context.Background())
	}()

	ctx := context.Background()
	runner, err := hooks.LoadWithSources(ctx, primary, extras)
	row.LoadWarnings = logs.take(directory)
	if err != nil {
		message := maskHooksText(err.Error(), directory)
		row.Error = &message
		return row, nil
	}
	empty := runner.Empty()
	row.Empty = &empty

	for index := range row.Fires {
		if err := clearHooksTrace(directory); err != nil {
			return row, err
		}
		logs.take(directory)
		payload := make(map[string]any, len(row.Fires[index].Payload))
		for key, value := range row.Fires[index].Payload {
			payload[key] = value
		}
		runner.Fire(ctx, row.Fires[index].Event, payload)
		row.Fires[index].Trace, err = readHooksTrace(directory)
		if err != nil {
			return row, err
		}
		row.Fires[index].Warnings = logs.take(directory)
	}

	if row.descendant {
		// The descendant would write after the hook's one-second timeout; give it
		// the time to, then look. If the group was killed it never will.
		time.Sleep(1400 * time.Millisecond)
		_, err := os.Stat(filepath.Join(directory, "late.part"))
		killed := os.IsNotExist(err)
		row.DescendantKilled = &killed
	}
	return row, nil
}

// clearHooksTrace empties the trace file between fires, so a row's trace is one
// fire's.
func clearHooksTrace(directory string) error {
	err := os.Remove(filepath.Join(directory, hooksTraceFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readHooksTrace parses the trace file into calls, in the order they were
// appended.
func readHooksTrace(directory string) ([]hooksContractCall, error) {
	data, err := os.ReadFile(filepath.Join(directory, hooksTraceFile))
	if os.IsNotExist(err) {
		// An empty list, not a missing one: the document says "no hook ran", and
		// a null would make the reader tell absent from empty to learn that.
		return []hooksContractCall{}, nil
	}
	if err != nil {
		return nil, err
	}
	calls := make([]hooksContractCall, 0, 4)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		at := strings.Index(line, ":")
		if at < 0 {
			return nil, fmt.Errorf("the trace line has no label: %q", line)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(line[at+1:]), &body); err != nil {
			return nil, fmt.Errorf("the trace line has no body: %q: %w", line, err)
		}
		calls = append(calls, hooksContractCall{Label: line[:at], Body: body})
	}
	return calls, nil
}

// maskHooksText applies the three text masks the document lists: the harness's
// directory, a regular expression's error, and a parse error's tail.
func maskHooksText(text, directory string) string {
	text = strings.ReplaceAll(text, directory+string(filepath.Separator), "")
	text = strings.ReplaceAll(text, directory, ".")
	if at := strings.Index(text, ": matcher: "); at >= 0 {
		return text[:at] + ": matcher: <regexp>"
	}
	if at := strings.Index(text, "hooks: parse "); at >= 0 {
		prefix := text[:at] + "hooks: parse "
		rest := text[len(prefix):]
		if colon := strings.Index(rest, ": "); colon >= 0 {
			return prefix + rest[:colon] + ": <parse error>"
		}
	}
	return text
}

// hooksLogRecorder is the sink the runner's warnings land in: an OTel log
// exporter that keeps each record's message and attributes.
//
// An exporter rather than a processor because that is the smaller interface, and
// `NewSimpleProcessor` is the SDK's synchronous wrapper around it: a warning is
// in the dump by the time `Fire` returns.
type hooksLogRecorder struct {
	mu      sync.Mutex
	records []hooksContractWarning
}

// Export implements sdklog.Exporter.
func (r *hooksLogRecorder) Export(_ context.Context, records []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range records {
		record := records[index]
		fields := map[string]string{}
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			fields[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		r.records = append(r.records, hooksContractWarning{
			Message: record.Body().AsString(),
			Fields:  fields,
		})
	}
	return nil
}

// Shutdown implements sdklog.Exporter.
func (r *hooksLogRecorder) Shutdown(context.Context) error { return nil }

// ForceFlush implements sdklog.Exporter.
func (r *hooksLogRecorder) ForceFlush(context.Context) error { return nil }

// take hands back the warnings recorded since the last call, with the harness's
// directory and a warning's error attribute masked.
func (r *hooksLogRecorder) take(directory string) []hooksContractWarning {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		return nil
	}
	out := make([]hooksContractWarning, 0, len(r.records))
	for _, record := range r.records {
		fields := make(map[string]string, len(record.Fields))
		for key, value := range record.Fields {
			if key == "error" {
				value = "<error>"
			}
			fields[key] = maskHooksText(value, directory)
		}
		if len(fields) == 0 {
			fields = nil
		}
		out = append(out, hooksContractWarning{Message: record.Message, Fields: fields})
	}
	r.records = nil
	return out
}
