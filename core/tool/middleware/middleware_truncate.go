package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/utils/fsatomic"
	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

// TruncateSettings configures [Truncate]. Zero values disable it.
type TruncateSettings struct {
	// Enabled turns truncation on.
	Enabled bool `json:"enabled"`
	// MaxChars is the in-context cap measured in Unicode code points.
	MaxChars int `json:"max_chars,omitempty"`
	// Dir receives the full outputs of oversized results
	// (<dir>/<call_id>.output).
	Dir string `json:"dir,omitempty"`
	// WorkDir anchors the relative pointer written into the result.
	// Empty keeps the pointer absolute.
	WorkDir string `json:"work_dir,omitempty"`
}

// MarkerPrefix starts the pointer a spilled result carries
// ("\n…[truncated; full output: " plus the path and a closing "]"), and
// MarkerNoOutput is the marker when nothing was persisted, so a
// consumer can recognize a truncation without matching a format
// string.
const (
	MarkerPrefix   = "\n…[truncated; full output: "
	MarkerNoOutput = "\n…[truncated]"
)

// Truncate caps oversized tool results recoverably: the full output is
// persisted under <dir>/<call_id>.output (a call id that is not a safe
// file name falls back to a digest of it) and the in-context content is
// replaced with a head+tail excerpt plus a pointer to the file, so the
// model can read the rest on demand. Results under the cap are
// returned as-is and never spill; error results pass through
// untouched.
//
// Truncate and [ResultLimiter] split the work, and the chain order
// carries the promise: Truncate is the recoverable policy — nothing is
// lost, the context carries an excerpt — and ResultLimiter is the hard
// backstop that drops whatever still exceeds its budget. Place
// Truncate inside (after) ResultLimiter, both inside Recover and
// Telemetry: the spill file then holds the tool's full output, and the
// limiter only ever sees the excerpt. Spill failures do not fail the
// call: they are logged through telemetry and the result still leaves
// as an excerpt — without a pointer, since there is no file to read.
//
// Structured results stay structured: JSON envelopes are excerpted
// inside their own top-level string fields and re-encoded, so tools
// that answer with a JSON payload (read_file, exec_command, web_fetch,
// ...) remain parseable for both the model and the UI instead of
// degrading into invalid JSON; a JSON value with no shrinkable string
// field becomes a small pointer envelope instead.
//
// A nil middleware is returned when the feature is disabled or
// misconfigured (no MaxChars or Dir); the chain skips nil entries.
func Truncate(settings TruncateSettings) tool.Middleware {
	if !settings.Enabled || settings.MaxChars <= 0 || settings.Dir == "" {
		return nil
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			if res.IsError {
				return res
			}
			// The cap counts the text projection: non-text parts
			// (images, audio, structured data) are carried through
			// untouched instead of being flattened away.
			full := res.Content.Text()
			if utf8.RuneCountInString(full) <= settings.MaxChars {
				return res
			}
			path := spillPath(settings.Dir, res.CallID)
			spilled := false
			if err := os.MkdirAll(settings.Dir, 0o700); err == nil {
				telemetry.WarnErr(ctx,
					"tool middleware: secure truncation directory failed",
					os.Chmod(settings.Dir, 0o700))
				if err := fsatomic.Write(path, []byte(full), fsatomic.Options{
					Perm:       0o600,
					TempPrefix: ".truncate-*.tmp",
				}); err != nil {
					telemetry.WarnErr(ctx,
						"tool middleware: persist truncated output failed", err)
				} else {
					spilled = true
				}
			} else {
				telemetry.WarnErr(ctx,
					"tool middleware: create truncation directory failed", err)
			}
			// The marker only carries a pointer once the payload is
			// really on disk: a failed spill must not send the model
			// after a file that is not there.
			marker := MarkerNoOutput
			ref := ""
			if spilled {
				ref = path
				if settings.WorkDir != "" {
					if rel, err := filepath.Rel(settings.WorkDir, path); err == nil {
						ref = rel
					} else {
						telemetry.WarnErr(ctx,
							"tool middleware: resolve relative truncation path failed",
							err)
					}
				}
				marker = MarkerPrefix + ref + "]"
			}
			if out, ok := truncateJSONResult(full, settings.MaxChars, ref, marker); ok {
				res.Content = replaceTextParts(res.Content, out)
				return res
			}
			// Plain text keeps the historical head+tail excerpt. A
			// valid JSON result only reaches this branch when even the
			// pointer envelope cannot fit the budget (a degenerate
			// max_chars) or when the payload is not JSON at all; the
			// envelope is the promised shape for realistic budgets.
			markerRunes := []rune(marker)
			if len(markerRunes) > settings.MaxChars {
				markerRunes = markerRunes[:settings.MaxChars]
			}
			keep := settings.MaxChars - len(markerRunes)
			res.Content = replaceTextParts(res.Content,
				headTailString(full, keep, markerRunes))
			return res
		}
	}
}

// spillPath names the spill file for one call. The call id comes off
// the wire, so it is used as a file name only when it is a single safe
// path element; a separator, "." or "..", or a Windows reserved name
// falls back to a digest of the id, which keeps the payload recoverable
// without letting an attacker-influenced name escape dir.
func spillPath(dir, callID string) string {
	name := callID
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || !pathsafe.RelRef(name) {
		sum := sha256.Sum256([]byte(callID))
		name = hex.EncodeToString(sum[:16])
	}
	return filepath.Join(dir, name+".output")
}

// truncateJSONResult shrinks an oversized JSON result while keeping it
// parseable. Oversized top-level string fields (read_file's content,
// exec_command's stdout/stderr, web_fetch's content) are excerpted in
// place and the object is re-encoded. A valid JSON value with no
// shrinkable string field — a long array, or a non-object value —
// becomes a small pointer envelope instead of a broken text excerpt.
// The second return value is false when the input is not JSON, or when
// even the pointer envelope cannot fit maxChars; the caller then falls
// back to the plain-text excerpt.
func truncateJSONResult(text string, maxChars int, ref, marker string) (string, bool) {
	if out, ok := truncateJSONFields(text, maxChars, marker); ok {
		return out, true
	}
	if !json.Valid([]byte(text)) {
		return "", false
	}
	return jsonPointerResult(text, maxChars, ref, marker)
}

// truncateJSONFields shortens the oversized top-level string fields of
// a JSON object until the re-encoded object fits maxChars. Each field
// gets a share of the free budget proportional to its current encoded
// size; short fields (paths, ids, enums) stay byte-identical. It
// reports false when the object has no field that can donate more than
// the marker itself.
func truncateJSONFields(text string, maxChars int, marker string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil || len(obj) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(obj))
	for key, raw := range obj {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", false
	}
	sort.Strings(keys)

	markerRunes := []rune(marker)
	truncated := false
	// Exact encoded-budget shares converge in one pass; a couple of
	// extra passes absorb rounding and escape-ratio drift.
	const maxPasses = 4
	for range maxPasses {
		encoded, err := json.Marshal(obj)
		if err != nil {
			return "", false
		}
		if utf8.RuneCount(encoded) <= maxChars {
			if truncated {
				markTruncatedFlags(obj)
				encoded, err = json.Marshal(obj)
				if err != nil || utf8.RuneCount(encoded) > maxChars {
					return "", false
				}
			}
			return string(encoded), true
		}

		type candidate struct {
			key    string
			raw    string
			rawLen int
			encLen int
		}
		candidates := make([]candidate, 0, len(keys))
		totalEnc := 0
		for _, key := range keys {
			var s string
			if err := json.Unmarshal(obj[key], &s); err != nil {
				continue
			}
			rawLen := utf8.RuneCountInString(s)
			// A field that cannot hold the marker plus some of its own
			// text would have to be emptied entirely; leave it alone
			// and let the pointer envelope handle the result instead.
			if rawLen <= len(markerRunes) {
				continue
			}
			encLen := utf8.RuneCount(obj[key])
			candidates = append(candidates,
				candidate{key: key, raw: s, rawLen: rawLen, encLen: encLen})
			totalEnc += encLen
		}
		if len(candidates) == 0 {
			return "", false
		}
		// Budget left for the string contents once the JSON structure
		// and every untouched field are paid for.
		probe := make(map[string]json.RawMessage, len(obj))
		for key, raw := range obj {
			probe[key] = raw
		}
		for _, c := range candidates {
			probe[c.key] = json.RawMessage(`""`)
		}
		probeJSON, err := json.Marshal(probe)
		if err != nil {
			return "", false
		}
		avail := maxChars - utf8.RuneCount(probeJSON)
		if avail <= 0 {
			return "", false
		}
		changed := false
		for _, c := range candidates {
			share := avail * c.encLen / totalEnc
			if share >= c.encLen {
				continue
			}
			next, ok := excerptWithinBudget(c.raw, markerRunes, share)
			if !ok {
				continue
			}
			encodedNext, err := json.Marshal(next)
			if err != nil {
				return "", false
			}
			obj[c.key] = encodedNext
			changed = true
		}
		if !changed {
			return "", false
		}
		truncated = true
	}
	return "", false
}

// excerptWithinBudget returns the longest head+marker+tail excerpt of
// raw whose JSON encoding fits budget runes. It reports false when even
// a marker-only excerpt does not fit.
func excerptWithinBudget(raw string, marker []rune, budget int) (string, bool) {
	// Every candidate costs at least its own rune count plus the
	// marker, so a larger probe could never fit; clamping the search
	// keeps it from building slices of the rejected input.
	length := utf8.RuneCountInString(raw)
	high := budget - len(marker)
	if high < 0 {
		high = 0
	}
	if length < high {
		high = length
	}
	low := 0
	best := ""
	for low <= high {
		mid := (low + high) / 2
		candidate := headTailAtMost(raw, length, mid, marker)
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return "", false
		}
		if utf8.RuneCount(encoded) <= budget {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// jsonPointerResult replaces a valid JSON value the field shrinker
// cannot bound with a small, valid envelope that carries a head+tail
// preview of the original text and the pointer to the persisted full
// output.
func jsonPointerResult(text string, maxChars int, ref, marker string) (string, bool) {
	type pointerResult struct {
		Truncated  bool   `json:"truncated"`
		FullOutput string `json:"full_output,omitempty"`
		Preview    string `json:"preview,omitempty"`
	}
	base, err := json.Marshal(pointerResult{Truncated: true, FullOutput: ref})
	if err != nil || utf8.RuneCount(base) > maxChars {
		return "", false
	}
	// Measure the envelope with a placeholder preview to learn exactly
	// how many encoded runes the preview value may spend.
	probe, err := json.Marshal(pointerResult{
		Truncated: true, FullOutput: ref, Preview: "x",
	})
	if err != nil {
		return "", false
	}
	quoted := utf8.RuneCount([]byte(`"x"`))
	room := maxChars - utf8.RuneCount(probe) + quoted
	if room > 0 {
		if preview, ok := excerptWithinBudget(text, []rune(marker), room); ok {
			out, err := json.Marshal(pointerResult{
				Truncated: true, FullOutput: ref, Preview: preview,
			})
			if err == nil && utf8.RuneCount(out) <= maxChars {
				return string(out), true
			}
		}
	}
	return string(base), true
}

// markTruncatedFlags keeps conventional truncation booleans honest:
// an envelope whose content this middleware shortened must not keep
// claiming it was untouched.
func markTruncatedFlags(obj map[string]json.RawMessage) {
	for _, key := range []string{"is_truncated", "truncated"} {
		var flag bool
		if err := json.Unmarshal(obj[key], &flag); err == nil && !flag {
			obj[key] = json.RawMessage("true")
		}
	}
}

// headTailString returns up to keep runes of raw plus the marker: 70%
// from the head and 30% from the tail, so both the start and the end of
// a long value stay visible. It slices at UTF-8 boundaries without
// converting the whole input to runes, so an oversized result pays only
// for the excerpt it keeps.
func headTailString(raw string, keep int, marker []rune) string {
	return headTailAtMost(raw, utf8.RuneCountInString(raw), keep, marker)
}

// headTailAtMost is headTailString with the rune count already known,
// so a binary search over one long field does not recount it per probe.
func headTailAtMost(raw string, length, keep int, marker []rune) string {
	if keep <= 0 {
		return string(marker)
	}
	if keep >= length {
		return raw
	}
	head := keep * 7 / 10
	tail := keep - head
	headEnd := 0
	for i := 0; i < head; i++ {
		_, size := utf8.DecodeRuneInString(raw[headEnd:])
		headEnd += size
	}
	tailStart := len(raw)
	for i := 0; i < tail; i++ {
		if tailStart <= 0 {
			break
		}
		// Step back one rune at a time; a byte that is not part of a
		// valid encoding counts as one rune on the way in (RuneCount)
		// and must do the same here, or the walk leaves the string.
		_, size := utf8.DecodeLastRuneInString(raw[:tailStart])
		tailStart -= size
	}
	if tailStart < headEnd {
		tailStart = headEnd
	}
	return raw[:headEnd] + string(marker) + raw[tailStart:]
}

// replaceTextParts swaps every text part for one truncated text part
// and keeps the remaining parts in order, so a multimodal result loses
// prose and keeps its media.
func replaceTextParts(content message.Content, text string) message.Content {
	out := message.Content{
		Parts: make([]message.Part, 0, len(content.Parts)+1),
	}
	out.Parts = append(out.Parts, message.TextPart{Text: text})
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		if _, isText := normalized.(message.TextPart); isText {
			continue
		}
		out.Parts = append(out.Parts, part)
	}
	return out
}
