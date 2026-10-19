package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The hooks dump is committed and compared byte for byte, so the harness has to
// be a function of the scripts and of core -- nothing else. These checks fail on
// the ways that goes wrong quietly:
//
//   - a command that names a file other than the trace, so a row's evidence
//     depends on where it ran;
//   - a second row with a clock in it, which is the only reason the timeout row
//     is allowed to cost the gate a second and a half;
//   - a mask that stopped matching the strings it claims to cover, which is how
//     the first cut missed `hooks: parse <path>: ...` at the head of a sentence;
//   - a row that is not named, or a name that repeats, which makes a mismatch
//     unattributable.

func generateHooks(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeHooksContract(&buffer); err != nil {
		t.Fatalf("writeHooksContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeHooks(t *testing.T) hooksContract {
	t.Helper()
	var document hooksContract
	if err := json.Unmarshal(generateHooks(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestHooksContractIsReproducible(t *testing.T) {
	first, second := generateHooks(t), generateHooks(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

// TestHooksCommandsOnlyTouchTheTrace: a command writes one file, and the harness
// reads one file. A command that named anything else would make the row depend on
// a path that only exists in one implementation's working directory.
func TestHooksCommandsOnlyTouchTheTrace(t *testing.T) {
	rows, withCommands := 0, 0
	for _, row := range hooksRows() {
		rows++
		configs := []string{row.Config}
		for _, extra := range row.Extras {
			configs = append(configs, extra.Config)
		}
		mentioned := 0
		for _, text := range configs {
			for _, mention := range mentionsOfAFile(text) {
				mentioned++
				if mention != hooksTraceFile && mention != "late.part" {
					t.Errorf("row %s names %q; rows may only write %s (and the timeout row's late.part)", row.Name, mention, hooksTraceFile)
				}
			}
		}
		if mentioned > 0 {
			withCommands++
		}
	}
	// The floor is what keeps this check from passing on a table that stopped
	// naming files at all -- 13 rows write the trace today, and the firing rows
	// are what the replay compares.
	if withCommands < 13 {
		t.Fatalf("%d of %d rows name a file; 13 is the floor: a check that inspects nothing passes", withCommands, rows)
	}
}

// aRelativeFile is any name a command could write: the rows are all in the
// harness's directory, so every one of them ends in `.part` and none of them is a
// path. A pattern rather than a split on quotes, because a command sits inside a
// JSON string and its own quoting is escaped.
var aRelativeFile = regexp.MustCompile(`[A-Za-z0-9_.-]+\.part`)

// mentionsOfAFile finds the file names a JSON document mentions.
func mentionsOfAFile(text string) []string {
	return aRelativeFile.FindAllString(text, -1)
}

// TestHooksHasExactlyOneClock: the timeout row waits out a kill, so it is the only
// row allowed to declare a descendant. A second one would double the gate's cost
// and, worse, invite a row whose answer is a race.
func TestHooksHasExactlyOneClock(t *testing.T) {
	var timed []string
	for _, row := range hooksRows() {
		if row.descendant {
			timed = append(timed, row.Name)
		}
	}
	if len(timed) != 1 || timed[0] != "timeout-kills-group" {
		t.Fatalf("rows with a clock: %v, want exactly [timeout-kills-group]", timed)
	}
}

// TestHooksRowsAreNamedOnce keeps a mismatch attributable: the row's name is what
// the replay prints.
func TestHooksRowsAreNamedOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range hooksRows() {
		if row.Name == "" {
			t.Errorf("a row has no name: %+v", row)
			continue
		}
		if seen[row.Name] {
			t.Errorf("row %q appears twice", row.Name)
		}
		seen[row.Name] = true
		if row.Why == "" {
			t.Errorf("row %q does not say what it is evidence of", row.Name)
		}
	}
}

// TestMaskHooksTextMasksTheShapesItClaims covers the two places core's own
// sentences carry a library's wording. The first cut masked on `": hooks: parse "`
// and so never matched the parse errors, which start the sentence rather than
// follow one: the row that would have caught it (`primary-parse-error`) was
// missing from the table at the time, so a mask that matched nothing looked fine.
func TestMaskHooksTextMasksTheShapesItClaims(t *testing.T) {
	const directory = "/var/folders/ab/T/hooks-contract-123"
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a primary file that does not parse",
			text: "hooks: parse " + directory + "/hooks.json: unexpected end of JSON input",
			want: "hooks: parse hooks.json: <parse error>",
		},
		{
			name: "a warning about a source that does not parse",
			text: "hooks: parse " + directory + "/broken.json: invalid character '}' looking for beginning of value",
			want: "hooks: parse broken.json: <parse error>",
		},
		{
			name: "a matcher that does not compile",
			text: "hooks: " + directory + "/hooks.json: PreToolUse[0]: matcher: error parsing regexp: missing closing ]: `[`",
			want: "hooks: hooks.json: PreToolUse[0]: matcher: <regexp>",
		},
		{
			name: "a sentence with no library text in it",
			text: "hooks: " + directory + "/hooks.json: unknown event \"Bogus\"",
			want: "hooks: hooks.json: unknown event \"Bogus\"",
		},
	}
	for _, test := range cases {
		if got := maskHooksText(test.text, directory); got != test.want {
			t.Errorf("%s:\n got %q\nwant %q", test.name, got, test.want)
		}
	}
}

// TestMaskedWarningErrorIsMaskedWhole: a warning's `error` attribute is a library's
// message, so the harness replaces it rather than masking a tail of it. Everything
// else a warning carries is core's own text.
func TestMaskedWarningErrorIsMaskedWhole(t *testing.T) {
	records := &hooksLogRecorder{}
	records.records = []hooksContractWarning{{
		Message: "hooks: command hook failed",
		Fields: map[string]string{
			"error":  "exit status 3",
			"output": strings.Repeat("x", 8),
		},
	}}
	taken := records.take("/nowhere")
	if len(taken) != 1 {
		t.Fatalf("take returned %d records", len(taken))
	}
	if got := taken[0].Fields["error"]; got != "<error>" {
		t.Errorf("the error attribute is %q, want <error>", got)
	}
	if got := taken[0].Fields["output"]; got != strings.Repeat("x", 8) {
		t.Errorf("the output attribute is %q, want it untouched", got)
	}
	if left := records.take("/nowhere"); len(left) != 0 {
		t.Errorf("take handed the same records back twice: %v", left)
	}
}
