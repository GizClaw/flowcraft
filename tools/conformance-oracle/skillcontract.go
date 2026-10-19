package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/skill"
)

// The skill contract: what one SKILL.md parses to.
//
// core/skill is the third package in the port whose behavior is a parser with
// opinions, and the opinions are the interesting part: the agentskills.io
// frontmatter rules are lenient where a third-party skill needs them to be
// (extension fields, a name that does not match its directory, a BOM) and strict
// where a model reads the result (one closed vocabulary of nothing, a required
// description, a cap on every string that reaches the context). Every one of
// those decisions is a row here, because every one of them is something a port can
// get quietly wrong in the direction of accepting more.
//
// A row is a file and its answer:
//
//	path       -- where the harness writes it, relative to its own directory
//	data       -- the bytes, exact whenever they are UTF-8
//	data_hex   -- the bytes, when they are not: JSON cannot carry an invalid
//	              sequence, and the YAML reader's refusal of one is behavior
//	metadata   -- the parse's answer: name, description, short_description, and
//	              the file's path
//	warnings   -- what it accepted with a note
//	error      -- what it refused
//
// The file's path is the one thing about the answer that cannot be compared
// as-is: the harness writes into a temporary directory, so its absolute path
// differs per run and per implementation. It is masked out of every string --
// metadata.path included -- the same way `-hooks-contract` masks its own.
//
// Two more strings are masked, for the same reason a library's wording is: an
// operating system's sentence (`no such file or directory` here, `No such file
// or directory (os error 2)` there) and a YAML or JSON library's (`yaml: line 1:
// did not find expected ',' or ']'` here, whatever the port's parser says). What
// stays pinned is the part core wrote: the verb and path of a failed stat or
// read, and the sentence that classifies a decode failure.
//
// Deliberately not rows, because they are one constant each rather than a
// behavior: the 256 KiB file cap (pinned by the port's own test, which names the
// limit once), and a file that cannot be read at all (a permission bit is not a
// contract, and `open ...: permission denied` is the operator's sentence, not the
// port's).

// skillContract is the committed document.
type skillContract struct {
	// Note says what the table is for.
	Note []string `json:"note"`
	// Limits are the caps the parser applies, from core itself.
	Limits skillContractLimits `json:"limits"`
	// Masked names what a reader must not expect to compare.
	Masked []string `json:"masked"`
	// Rows are the files and their answers.
	Rows []skillContractRow `json:"rows"`
}

// skillContractLimits is every cap a parse applies.
type skillContractLimits struct {
	// Name is the maximum name length in runes.
	Name int `json:"name"`
	// Description is the maximum description length in runes.
	Description int `json:"description"`
	// File is the maximum SKILL.md size in bytes.
	File int `json:"file"`
}

// skillContractRow is one SKILL.md and the parse's answer.
type skillContractRow struct {
	// Name identifies the row in a mismatch.
	Name string `json:"name"`
	// Why says what the row is evidence of.
	Why string `json:"why"`
	// Path is where the file is written, relative to the harness's directory.
	// Its parent directory is what a missing name falls back to.
	Path string `json:"path"`
	// Kind is "dir" when Path is a directory rather than a document.
	Kind string `json:"kind,omitempty"`
	// Data is the file's bytes when they are valid UTF-8.
	Data string `json:"data,omitempty"`
	// DataHex is the file's bytes when they are not.
	DataHex string `json:"data_hex,omitempty"`
	// Metadata is the parse's answer, when it produced one.
	Metadata *skillContractMetadata `json:"metadata,omitempty"`
	// Warnings are the shape issues the parse accepted.
	Warnings []string `json:"warnings,omitempty"`
	// Error is the refusal, when it refused.
	Error *string `json:"error,omitempty"`
}

// skillContractMetadata is one skill's metadata, with the path masked.
type skillContractMetadata struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	ShortDescription string `json:"short_description,omitempty"`
	Path             string `json:"path"`
}

// writeSkillContract dumps the contract to w.
func writeSkillContract(w io.Writer) error {
	document, err := skillDocument()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// skillDocument runs every row and returns the document.
func skillDocument() (skillContract, error) {
	rows, err := runSkillRows(skillRows())
	if err != nil {
		return skillContract{}, err
	}
	return skillContract{
		Note: []string{
			"core/skill, as a table: every SKILL.md the package's parser is asked about, and the answer it gives.",
			"A row is a file written into a temporary directory of the harness's own; the file's absolute path is masked out of every string, so what is left is the logical name the row spells (review/SKILL.md, My_Skill/SKILL.md).",
			"The parser is deliberately lenient where a third-party skill needs it and strict where a model reads the result: both halves are rows here, because a port that accepts more than core is the failure this table exists to catch.",
			"A YAML library's own error text is masked after the sentence that classifies it: Go and Rust word a syntax error differently, and a contract that pinned the wording would pin the library rather than the behavior.",
			"Not rows: the 256 KiB file cap and an unreadable file. Each is one constant or one operator sentence, not a behavior, and each is pinned by the port's own test where the constant is named.",
		},
		Limits: skillContractLimits{
			Name:        64,
			Description: 1024,
			File:        256 << 10,
		},
		Masked: []string{
			"the harness's directory, in every string (metadata.path included)",
			"an operating system's sentence after `stat <path>: ` or `read <path>: `",
			"a YAML library's error text, after the sentence that classifies it",
		},
		Rows: rows,
	}, nil
}

// runSkillRows writes every row's file into one temporary directory and parses it.
func runSkillRows(rows []skillContractRow) ([]skillContractRow, error) {
	directory, err := os.MkdirTemp("", "skill-contract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)

	out := make([]skillContractRow, 0, len(rows))
	for _, row := range rows {
		ran, err := runSkillRow(row, directory)
		if err != nil {
			return nil, fmt.Errorf("row %s: %w", row.Name, err)
		}
		out = append(out, ran)
	}
	return out, nil
}

// runSkillRow materializes one file and records what the parser said about it.
func runSkillRow(row skillContractRow, directory string) (skillContractRow, error) {
	path := filepath.Join(directory, filepath.FromSlash(row.Path))
	data := []byte(row.Data)
	if row.DataHex != "" {
		decoded, err := hex.DecodeString(row.DataHex)
		if err != nil {
			return row, err
		}
		data = decoded
	}
	// The parent directory is the fallback a missing name uses, so it has to
	// exist even for a row whose file is absent.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return row, err
	}
	switch {
	case row.Kind == "dir":
		if err := os.MkdirAll(path, 0o755); err != nil {
			return row, err
		}
	case row.Data != "" || row.DataHex != "":
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return row, err
		}
	}

	// Every string in the answer goes through the mask, and the answers are
	// read before the mask is applied so a diagnostic cannot be rewritten by a
	// second pass.
	res, err := skill.ParseFile(path)
	if err != nil {
		message := maskSkillText(err.Error(), directory)
		row.Error = &message
		return row, nil
	}
	row.Metadata = &skillContractMetadata{
		Name:             res.Metadata.Name,
		Description:      res.Metadata.Description,
		ShortDescription: res.Metadata.ShortDescription,
		Path:             maskSkillText(res.Metadata.Path, directory),
	}
	for _, warning := range res.Warnings {
		row.Warnings = append(row.Warnings, maskSkillText(warning, directory))
	}
	return row, nil
}

// maskSkillText applies the two masks the document lists: the harness's directory
// and a YAML library's own error text.
func maskSkillText(text, directory string) string {
	text = strings.ReplaceAll(text, directory+string(filepath.Separator), "")
	text = strings.ReplaceAll(text, directory, ".")
	// An operating system's sentence is the platform's, not core's: what core
	// picked is the verb (`stat`, `read`) and the path it names, and those are
	// compared exactly.
	for _, verb := range []string{"stat ", "read "} {
		if rest, ok := strings.CutPrefix(text, verb); ok {
			if at := strings.Index(rest, ": "); at >= 0 {
				return verb + rest[:at+2] + "<os error>"
			}
		}
	}
	const prefix = "skill: parse frontmatter: "
	if strings.HasPrefix(text, prefix) {
		// The one sentence under this prefix that core writes itself: the keys
		// no JSON key can spell. Everything else is go-yaml's or
		// encoding/json's wording, which the port's own libraries word
		// differently.
		if strings.HasPrefix(text, prefix+"YAML key") {
			return text
		}
		return prefix + "<decode error>"
	}
	return text
}

// scalarRows is the coercion table: what a YAML scalar becomes when the field it
// lands in is a string.
//
// This is `sigs.k8s.io/yaml`'s rule rather than `yaml.v2`'s — the frontmatter is
// converted to JSON against the target struct, and during that conversion a
// number or a boolean meeting a string field is coerced to its spelling. The
// spellings are Go's: an integer in decimal after YAML 1.1 resolution (`007` is
// octal, `0x10` is hex, `1_000` drops its underscores), a float at *float32*
// precision (`%g` with 32-bit digits, which is why `0.000001` is `1e-06` and
// `1e21` keeps its exponent), the YAML specials as Go's `+Inf`/`NaN`, and a null
// as the empty string.
//
// A port that coerces with its own language's float formatting gets `1e-6`,
// `1000000` and `1000000000000000000000` instead, which is why every row here is
// one scalar rather than a sampler.
func scalarRows() []skillContractRow {
	cases := []struct{ label, scalar, why string }{
		{"int", "1", "an integer is its decimal spelling"},
		{"octal", "007", "YAML 1.1 octal: the value is 7, not the digits 007"},
		{"hex", "0x10", "YAML 1.1 hex: the value is 16"},
		{"underscored", "1_000", "YAML 1.1 underscores are separators, not characters"},
		{"uint64", "18446744073709551615", "an unsigned value past int64 is still an integer, in decimal"},
		{"overflowing", "123456789012345678901234567890", "a value past uint64 becomes a float, and is spelled at float32 precision"},
		{"float", "1.5", "a plain float"},
		{"tenth", "0.1", "the shortest spelling that round-trips, not a full-precision one"},
		{"exponent-folds", "1e3", "%g drops an exponent that fits the digit count: this is 1000, not 1e+03"},
		{"exponent-kept", "1e21", "%g keeps the exponent once it outgrows the digits"},
		{"small", "0.000001", "a small float takes the exponent form with Go's two-digit exponent"},
		{"infinity", ".inf", "a special float is Go's +Inf, not YAML's .inf"},
		{"nan", ".nan", "a special float is Go's NaN"},
		{"bool", "true", "a boolean is true, not True or 1"},
		{"yaml11-bool", "yes", "YAML 1.1 resolves yes to a boolean, which is then spelled true"},
		{"null", "null", "a null in a string field is the empty string"},
		{"quoted-number", `"123"`, "a quoted scalar is a string and stays one"},
		{"quoted-octal", "'007'", "quoting keeps the digits YAML would otherwise resolve"},
	}
	rows := make([]skillContractRow, 0, len(cases))
	for _, c := range cases {
		rows = append(rows, skillContractRow{
			Name: "scalar-" + c.label,
			Why:  "a YAML scalar in a string field is coerced to its spelling: " + c.why,
			Path: "review/SKILL.md",
			Data: "---\nname: review\ndescription: " + c.scalar + "\n---\n\nbody\n",
		})
	}
	// The same rule on a field with a validator behind it: `007` reaches the name
	// check as the string "7" -- a *valid* name, so the row warns about the
	// mismatch and not about the spelling. Either way the warning quotes the
	// coerced value, which is what a reader has to see.
	rows = append(rows, skillContractRow{
		Name: "scalar-name",
		Why:  "a coerced name reaches the validator as the spelling: 007 is the valid name 7, and the mismatch warning quotes 7",
		Path: "review/SKILL.md",
		Data: "---\nname: 007\ndescription: d\n---\n\nbody\n",
	})
	return rows
}

// skillRows is the table.
func skillRows() []skillContractRow {
	const (
		plain    = "---\nname: review\ndescription: Review a change for defects\n---\n\n# Review\n\nRead the diff.\n"
		skillDir = "review"
	)
	rows := []skillContractRow{
		{
			Name: "plain",
			Why:  "one well-formed skill: the three fields a caller reads, and the path it will open later",
			Path: skillDir + "/SKILL.md",
			Data: plain,
		},
		{
			Name: "bom",
			Why:  "an editor's BOM is trimmed rather than refusing a usable skill",
			Path: skillDir + "/SKILL.md",
			Data: "\ufeff" + plain,
		},
		{
			Name: "crlf-after-delimiter",
			Why:  "a CRLF file is split at the same two delimiters as an LF one",
			Path: skillDir + "/SKILL.md",
			Data: "---\r\nname: review\r\ndescription: Review a change for defects\r\n---\r\n\r\n# Review\r\n",
		},
		{
			Name: "no-frontmatter",
			Why:  "a Markdown file without frontmatter is refused, and the refusal names the shape it wanted",
			Path: skillDir + "/SKILL.md",
			Data: "# Review\n\nNo frontmatter here.\n",
		},
		{
			Name: "unterminated",
			Why:  "an opening delimiter with no closing one is refused rather than treated as a body",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: Review a change for defects\n",
		},
		{
			Name: "empty-body",
			Why:  "frontmatter with a blank body is refused: a skill with no instructions is not a skill",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: Review a change for defects\n---\n\n   \n",
		},
		{
			Name: "delimiter-inside-a-block",
			Why:  "the first closing delimiter ends the frontmatter, so a --- line inside the YAML is where the block ends",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: Review a change for defects\n---\n---\n\nbody\n",
		},
		{
			Name: "name-mismatch",
			Why:  "a valid name that differs from its directory is accepted with a warning: third-party skills violate the standard and refusing them would drop usable content",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: something-else\ndescription: tolerated name mismatch\n---\n\nbody\n",
		},
		{
			Name: "name-invalid-falls-back",
			Why:  "an invalid name falls back to the slugified directory name, with a warning",
			Path: "My_Skill/SKILL.md",
			Data: "---\nname: My_Skill\ndescription: bad name\n---\n\nbody\n",
		},
		{
			Name: "name-missing-falls-back",
			Why:  "an absent name falls back to the slugified directory name, with a warning",
			Path: "Some Dir/SKILL.md",
			Data: "---\ndescription: no name at all\n---\n\nbody\n",
		},
		{
			Name: "name-too-long-falls-back",
			Why:  "a name past the rune cap is invalid, so it falls back rather than truncating",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\n\nbody\n",
		},
		{
			Name: "name-at-the-cap",
			Why:  "a name of exactly 64 characters is valid, so the cap is a cap and not an off-by-one",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: " + strings.Repeat("a", 64) + "\ndescription: d\n---\n\nbody\n",
		},
		{
			Name: "fallback-is-unusable",
			Why:  "a missing name whose directory slugifies to nothing is refused: there is no name to fall back to",
			Path: "___/SKILL.md",
			Data: "---\ndescription: nothing to call it\n---\n\nbody\n",
		},
		{
			Name: "description-missing",
			Why:  "the description is required: it is the only thing a per-turn ranking can rank on",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\n---\n\nbody\n",
		},
		{
			Name: "description-folds-whitespace",
			Why:  "a description is folded to one line, because it is rendered into a single bullet",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: |\n  Review   a change\n  for   defects\n---\n\nbody\n",
		},
		{
			Name: "description-truncated",
			Why:  "a description past the cap is truncated in runes, not bytes: a byte slice would cut a three-byte character in half",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: " + strings.Repeat("中", 1030) + "\n---\n\nbody\n",
		},
		{
			Name: "extension-fields",
			Why:  "third-party extension fields are tolerated, and metadata.short-description is read through its hyphenated key",
			Path: "extended/SKILL.md",
			Data: "---\nname: extended\ndescription: carries extensions\nlicense: MIT\nversion: 1.2.3\nmetadata:\n  short-description: short form\n  owner: someone\n---\n\nbody\n",
		},
		{
			Name: "short-description-folds",
			Why:  "the non-standard short description is folded like the description is",
			Path: "short/SKILL.md",
			Data: "---\nname: short\ndescription: d\nmetadata:\n  short-description: \"  two   words \\n and a newline \"\n---\n\nbody\n",
		},
		{
			Name: "capitalized-field-names",
			Why:  "the typed decode matches field names case-insensitively, which is encoding/json's rule: a SkILL.md that spells Name and DESCRIPTION reads the same as one that spells them in lower case",
			Path: skillDir + "/SKILL.md",
			Data: "---\nName: review\nDESCRIPTION: capitalised fields\n---\n\nbody\n",
		},
		{
			Name: "whitespace-around-name",
			Why:  "a name and description are folded before they are validated, so surrounding and interior whitespace never reaches a caller",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: \"  review \"\ndescription: \"  spaced   out  \"\n---\n\nbody\n",
		},
		{
			Name: "yaml-syntax-error",
			Why:  "a frontmatter that is not YAML is refused, and only the library's own wording is masked",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: [review\ndescription: d\n---\n\nbody\n",
		},
		{
			Name: "yaml-not-a-mapping",
			Why:  "frontmatter that decodes to a list is refused by the typed decode rather than read as empty",
			Path: skillDir + "/SKILL.md",
			Data: "---\n- name: review\n- description: d\n---\n\nbody\n",
		},
		{
			Name: "name-is-a-number",
			Why:  "a name that is not a string is refused rather than coerced, so a numeric name never reaches a caller",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: 123\ndescription: d\n---\n\nbody\n",
		},
		{
			Name: "duplicate-keys",
			Why:  "a key written twice keeps the last value, which is what yaml.v2 does and what the typed decode then reads",
			Path: skillDir + "/SKILL.md",
			Data: "---\nname: review\ndescription: first\ndescription: second\n---\n\nbody\n",
		},
		{
			Name: "collapsing-keys",
			Why:  "two distinct keys that spell one JSON key load with a note rather than silently carrying a mapping that means two things",
			Path: "collide/SKILL.md",
			Data: "---\nname: collide\ndescription: d\nmetadata:\n  1: a\n  \"1\": b\n---\n\nbody\n",
		},
		{
			Name: "unusable-key",
			Why:  "a key no JSON key can spell refuses the file by name, instead of by whichever key a map walk met first",
			Path: "unusable/SKILL.md",
			Data: "---\nname: unusable\ndescription: d\nmetadata:\n  ~: c\n---\n\nbody\n",
		},
		{
			Name: "unhashable-key",
			Why:  "a list used as a key is refused by the YAML reader itself, before core's own key pre-check can name it: the two refusals are different sentences and different code",
			Path: "unhashable/SKILL.md",
			Data: "---\nname: unhashable\ndescription: d\nmetadata:\n  ? [a, b]\n  : c\n---\n\nbody\n",
		},
		{
			Name:    "invalid-utf8",
			Why:     "a byte that is not UTF-8 refuses the document in the YAML reader, before any field is read",
			Path:    skillDir + "/SKILL.md",
			DataHex: hex.EncodeToString([]byte("---\nname: review\ndescription: \xff\n---\n\nbody\n")),
		},
		{
			Name: "missing-file",
			Why:  "a path that does not exist is refused by core's own stat, which names the verb and the path; the operating system's half of the sentence is the platform's, so it is masked",
			Path: "absent/SKILL.md",
		},
		{
			Name: "path-is-a-directory",
			Why:  "a directory in place of a file passes the stat and is refused by core's own read, which names the verb and the path",
			Path: "dir-skill/SKILL.md",
			Kind: "dir",
		},
	}
	return append(rows, scalarRows()...)
}
