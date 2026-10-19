package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The skill dump is committed and compared byte for byte, so the harness has to
// be a function of the rows and of core -- nothing else. These checks fail on the
// ways that goes wrong quietly:
//
//   - a mask that stopped matching the strings it claims to cover, which looks
//     exactly like a mask with nothing to do: the directory mask, both operating
//     system verbs, and the two sentences the frontmatter mask lets through;
//   - a row whose path has no parent directory, so the fallback it is supposed to
//     exercise comes from the harness's own temporary directory instead of from
//     the row;
//   - a row that is not named, or a name that repeats, which makes a mismatch
//     unattributable;
//   - a row that carries both spellings of its bytes, or an answer with two
//     halves, either of which would make the document describe two files.

func generateSkill(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeSkillContract(&buffer); err != nil {
		t.Fatalf("writeSkillContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeSkill(t *testing.T) skillContract {
	t.Helper()
	var document skillContract
	if err := json.Unmarshal(generateSkill(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestSkillContractIsReproducible(t *testing.T) {
	first, second := generateSkill(t), generateSkill(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same rows differ")
	}
}

// TestSkillMaskKeepsWhatCoreWrote: the masked half of a string is somebody
// else's words, and the kept half is core's. A mask that swallowed the whole
// sentence would pass every replay while pinning nothing.
func TestSkillMaskKeepsWhatCoreWrote(t *testing.T) {
	const directory = "/var/folders/xy/skill-contract-123"
	cases := []struct{ name, text, want string }{
		{
			name: "the file's own path",
			text: directory + "/review/SKILL.md",
			want: "review/SKILL.md",
		},
		{
			name: "a stat failure keeps the verb and the path",
			text: "stat " + directory + "/absent/SKILL.md: no such file or directory",
			want: "stat absent/SKILL.md: <os error>",
		},
		{
			name: "a read failure keeps the verb and the path",
			text: "read " + directory + "/dir-skill/SKILL.md: is a directory",
			want: "read dir-skill/SKILL.md: <os error>",
		},
		{
			name: "a library's decode error is masked after the classification",
			text: "skill: parse frontmatter: error converting YAML to JSON: yaml: line 1: did not find expected ',' or ']'",
			want: "skill: parse frontmatter: <decode error>",
		},
		{
			name: "core's own key sentence is kept whole",
			text: "skill: parse frontmatter: YAML key !!null null cannot be spelled as a JSON key",
			want: "skill: parse frontmatter: YAML key !!null null cannot be spelled as a JSON key",
		},
		{
			name: "a refusal with no third-party wording is untouched",
			text: "skill: description is required",
			want: "skill: description is required",
		},
	}
	for _, c := range cases {
		if got := maskSkillText(c.text, directory); got != c.want {
			t.Errorf("%s: masked to %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSkillRowsAreNamedAndUsable: a row has to be attributable in a mismatch, and
// it has to describe one file of its own -- with a parent directory, since that
// parent is the fallback its missing name reads.
func TestSkillRowsAreNamedAndUsable(t *testing.T) {
	seen := make(map[string]bool)
	for _, row := range skillRows() {
		if row.Name == "" {
			t.Fatalf("a row is unnamed: %+v", row)
		}
		if seen[row.Name] {
			t.Fatalf("row %s appears twice", row.Name)
		}
		seen[row.Name] = true
		if row.Why == "" {
			t.Fatalf("row %s does not say what it is evidence of", row.Name)
		}
		if parent := filepath.Dir(row.Path); parent == "." || parent == string(filepath.Separator) {
			t.Fatalf("row %s has no parent directory: %s", row.Name, row.Path)
		}
		if row.Kind == "dir" && (row.Data != "" || row.DataHex != "") {
			t.Fatalf("row %s is a directory and a document", row.Name)
		}
		if row.Data != "" && row.DataHex != "" {
			t.Fatalf("row %s carries its bytes twice", row.Name)
		}
		// A missing-file row is the one shape with no bytes at all; every other
		// row has to write something for the parser to read.
		if row.Kind == "" && row.Data == "" && row.DataHex == "" && row.Name != "missing-file" {
			t.Fatalf("row %s writes nothing", row.Name)
		}
	}
}

// TestSkillAnswersAreSingle: a row either produced metadata, or it refused. A
// document that recorded both would compare an answer against itself.
func TestSkillAnswersAreSingle(t *testing.T) {
	document := decodeSkill(t)
	if len(document.Rows) < 40 {
		t.Fatalf("the table has %d rows; the scalar coercions alone are eighteen", len(document.Rows))
	}
	ok, refused := 0, 0
	for _, row := range document.Rows {
		switch {
		case row.Metadata != nil && row.Error != nil:
			t.Fatalf("row %s both answered and refused: %q", row.Name, *row.Error)
		case row.Metadata != nil:
			ok++
			if row.Metadata.Description == "" {
				t.Fatalf("row %s answered with an empty description", row.Name)
			}
			if row.Metadata.Path == "" || strings.HasPrefix(row.Metadata.Path, "/") {
				t.Fatalf("row %s carries an unmasked path: %q", row.Name, row.Metadata.Path)
			}
		case row.Error != nil:
			refused++
			if !strings.HasPrefix(*row.Error, "skill: ") && !strings.HasPrefix(*row.Error, "stat ") && !strings.HasPrefix(*row.Error, "read ") {
				t.Fatalf("row %s refused with %q, which is neither a skill sentence nor a stat/read one", row.Name, *row.Error)
			}
		default:
			t.Fatalf("row %s neither answered nor refused", row.Name)
		}
	}
	// A table that drifted towards one answer would still replay; the contract's
	// value is that both halves of the parser are in it.
	if ok < 20 || refused < 10 {
		t.Fatalf("the table is lopsided: %d answered, %d refused", ok, refused)
	}
}
