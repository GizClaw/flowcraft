package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The secret dump is committed and compared byte for byte, so the harness has to
// be a function of the cases and of core -- nothing else. These checks fail on
// the ways that goes wrong quietly:
//
//   - a reference answer that names a different path than the one asked about,
//     so a reordering of `paths` would silently compare the wrong rows;
//   - a row whose `why` claims something the answers do not show: the dotted-name
//     rule with no dotted witness, a named-store row that never resolves through
//     the default, a refusal row with no refusal;
//   - a rendering that stops being `<secret>` for one verb but not another, which
//     is how a secret leaks: through the one spelling nobody compared;
//   - the two decode refusals collapsing into one sentence, which would let a port
//     pass with either branch missing;
//   - a `null` document that stops being the empty literal, because that quirk is
//     `encoding/json`'s and a port that treats null as a bad shape is wrong;
//   - two dumps of the same cases that differ.

func generateSecret(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeSecretContract(&buffer); err != nil {
		t.Fatalf("writeSecretContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeSecret(t *testing.T) secretContract {
	t.Helper()
	var document secretContract
	if err := json.Unmarshal(generateSecret(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

// secretRefsRow finds one refs row by name.
func secretRefsRow(t *testing.T, name string) secretRefsCase {
	t.Helper()
	for _, row := range decodeSecret(t).Refs {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("no refs row named %q", name)
	return secretRefsCase{}
}

// secretAnswer finds one path's answer in a row.
func secretAnswer(t *testing.T, row secretRefsCase, path string) secretRefAnswer {
	t.Helper()
	for _, answer := range row.Answers {
		if answer.Path == path {
			return answer
		}
	}
	t.Fatalf("row %q has no answer for %q", row.Name, path)
	return secretRefAnswer{}
}

// secretValueRow finds one values row by name.
func secretValueRow(t *testing.T, name string) secretValueCase {
	t.Helper()
	for _, row := range decodeSecret(t).Values {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("no values row named %q", name)
	return secretValueCase{}
}

func TestSecretContractIsReproducible(t *testing.T) {
	if first, second := generateSecret(t), generateSecret(t); !bytes.Equal(first, second) {
		t.Fatalf("two dumps of the same cases differ:\n%s\n%s", first, second)
	}
}

func TestSecretNothingIsMasked(t *testing.T) {
	// The stores are canned answer tables, so every sentence is a function of
	// the document. A `masked` entry would mean the harness grew a dependency
	// (a path, a clock) that the replay cannot reproduce.
	document := decodeSecret(t)
	if len(document.Masked) != 2 {
		t.Fatalf("masked = %v, want the `nothing` entry and the wire's escaping", document.Masked)
	}
	if !strings.HasPrefix(document.Masked[0], "nothing runtime-dependent:") {
		t.Fatalf("masked = %v, want the `nothing` entry first", document.Masked)
	}
	if !strings.Contains(document.Masked[1], "escaping spelling of `wire`") {
		t.Fatalf("masked = %v, want the wire's escaping named", document.Masked)
	}
}

func TestSecretRowsAreNamedAndExplained(t *testing.T) {
	document := decodeSecret(t)
	seen := map[string]string{}
	check := func(table, name, why string) {
		key := table + "/" + name
		if name == "" || why == "" {
			t.Fatalf("%s row has an empty name or why: %+v", table, name)
		}
		if previous, exists := seen[key]; exists {
			t.Fatalf("%s row %q repeats %q", table, key, previous)
		}
		seen[key] = why
	}
	for _, row := range document.Refs {
		check("refs", row.Name, row.Why)
	}
	for _, row := range document.Resolves {
		check("resolves", row.Name, row.Why)
	}
	for _, row := range document.Values {
		check("values", row.Name, row.Why)
	}
	if len(seen) == 0 {
		t.Fatal("the document has no rows")
	}
}

func TestSecretRefsAnswerThePathTheyWereGiven(t *testing.T) {
	for _, row := range decodeSecret(t).Refs {
		if len(row.Answers) != len(row.Paths) {
			t.Fatalf("row %q: %d answers for %d paths",
				row.Name, len(row.Answers), len(row.Paths))
		}
		for i, answer := range row.Answers {
			if answer.Path != row.Paths[i] {
				t.Fatalf("row %q: answer %d is about %q, want %q",
					row.Name, i, answer.Path, row.Paths[i])
			}
			if (answer.Error == "") == (answer.Store == "") {
				t.Fatalf("row %q: answer for %q carries both a store and a refusal",
					row.Name, answer.Path)
			}
		}
	}
}

func TestSecretRefsPinTheDottedNameRule(t *testing.T) {
	row := secretRefsRow(t, "default-and-named")
	// A dotted path whose prefix is not a store is a name in the default
	// store, whole.
	if answer := secretAnswer(t, row, "my.app.key"); answer.Store != "env" ||
		answer.Name != "my.app.key" {
		t.Fatalf("my.app.key = %+v, want the whole path in the default store", answer)
	}
	// A dotted path whose prefix is a store is an explicit reference.
	if answer := secretAnswer(t, row, "files.token"); answer.Store != "files" ||
		answer.Name != "token" {
		t.Fatalf("files.token = %+v, want files/token", answer)
	}
	// A name that merely contains the default store's id is not a reference.
	if answer := secretAnswer(t, row, "env .key"); answer.Store != "env" ||
		answer.Name != "env .key" {
		t.Fatalf("env .key = %+v, want the whole path: the prefix is not trimmed", answer)
	}
	// A leading or trailing dot is a name, not a prefix.
	for _, path := range []string{".TOKEN", "TOKEN."} {
		if answer := secretAnswer(t, row, path); answer.Store != "env" || answer.Name != path {
			t.Fatalf("%s = %+v, want the whole path in the default store", path, answer)
		}
	}
}

func TestSecretRefsRefuseWhenNoDefaultIsConfigured(t *testing.T) {
	row := secretRefsRow(t, "no-default")
	if answer := secretAnswer(t, row, "files.token"); answer.Error != "" {
		t.Fatalf("files.token = %+v, want a named store to resolve without a default", answer)
	}
	for _, path := range []string{"TOKEN", ".TOKEN"} {
		answer := secretAnswer(t, row, path)
		if !strings.Contains(answer.Error, "needs an explicit store or a default secret.Store") {
			t.Fatalf("%s = %+v, want the missing-default refusal", path, answer)
		}
		if !strings.Contains(answer.Error, "${secret:"+path+"}") {
			t.Fatalf("%s = %+v, want the refusal to quote the reference", path, answer)
		}
	}
}

func TestSecretRefsCheckTheNameOnlyOnceTheStoreMatches(t *testing.T) {
	row := secretRefsRow(t, "empty-name")
	if answer := secretAnswer(t, row, "files."); !strings.Contains(answer.Error, "secret name is empty") {
		t.Fatalf("files. = %+v, want the empty-name refusal", answer)
	}
	// The same shape without a store prefix is a name in the default store.
	if answer := secretAnswer(t, row, "."); answer.Store != "files" || answer.Name != "." {
		t.Fatalf(". = %+v, want a name in the default store", answer)
	}
}

func TestSecretResolvesKeepTheThreeFamiliesApart(t *testing.T) {
	document := decodeSecret(t)
	refusals := map[string]string{}
	for _, row := range document.Resolves {
		switch row.Name {
		case "value":
			if row.Error != "" || row.Value != "tok-123" {
				t.Fatalf("value = %+v", row)
			}
			continue
		case "no-resolver":
			if !row.NoResolver {
				t.Fatalf("row %q asks a resolver", row.Name)
			}
		}
		if row.Error == "" {
			t.Fatalf("row %q has no refusal", row.Name)
		}
		refusals[row.Name] = row.Error
	}
	if len(refusals) != 4 {
		t.Fatalf("refusal rows = %v, want four", refusals)
	}
	// A backend failure is wrapped, not replaced: the store's own sentence
	// survives inside core's.
	if !strings.Contains(refusals["store-failure"], "backend is down") {
		t.Fatalf("store-failure = %q, want the backend's sentence inside it", refusals["store-failure"])
	}
	// The use-time family and the scheme family do not share a prefix.
	for _, row := range document.Refs {
		for _, answer := range row.Answers {
			if answer.Error == "" {
				continue
			}
			if !strings.HasPrefix(answer.Error, "resource settings expand: ") {
				t.Fatalf("row %q: %q is not a scheme refusal", row.Name, answer.Error)
			}
		}
	}
	for name, refusal := range refusals {
		if !strings.HasPrefix(refusal, "resource secret: ") {
			t.Fatalf("row %q: %q is not a use-time refusal", name, refusal)
		}
	}
}

func TestSecretValuesAlwaysRenderRedacted(t *testing.T) {
	for _, row := range decodeSecret(t).Values {
		if !row.OK {
			continue
		}
		for name, rendering := range map[string]string{
			"wire":      row.Wire,
			"display":   row.Display,
			"go_syntax": row.GoSyntax,
		} {
			want := "<secret>"
			if name == "wire" {
				// The wire is JSON text; its escaping is the encoder's
				// spelling, so compare what it decodes to.
				var decoded string
				if err := json.Unmarshal([]byte(rendering), &decoded); err != nil {
					t.Fatalf("row %q: wire %q does not decode: %v", row.Name, rendering, err)
				}
				if decoded != want {
					t.Fatalf("row %q: wire decodes to %q, want %q", row.Name, decoded, want)
				}
				continue
			}
			if rendering != want {
				t.Fatalf("row %q: %s = %q, want %q", row.Name, name, rendering, want)
			}
		}
	}
}

func TestSecretValuesKeepTheTwoDecodeRefusalsApart(t *testing.T) {
	document := decodeSecret(t)
	blank, wrongShape := 0, 0
	for _, row := range document.Values {
		if row.OK {
			continue
		}
		switch {
		case strings.HasSuffix(row.Error, "reference must carry store and name"):
			blank++
		case strings.Contains(row.Error, `want string or {"store":...,"name":...}, got `):
			wrongShape++
			// The refusal quotes the document as written.
			quoted := strings.TrimPrefix(row.Error, `resource secret: want string or {"store":...,"name":...}, got `)
			if quoted != row.Doc {
				t.Fatalf("row %q: refusal quotes %q, want %q", row.Name, quoted, row.Doc)
			}
		default:
			t.Fatalf("row %q: unexpected refusal %q", row.Name, row.Error)
		}
	}
	if blank == 0 || wrongShape == 0 {
		t.Fatalf("blank = %d, wrong-shape = %d, want both branches witnessed", blank, wrongShape)
	}
}

func TestSecretNullIsTheEmptyLiteral(t *testing.T) {
	// `encoding/json` unmarshals null into a string target as a no-op, so the
	// string branch wins and the value is the empty literal. A port that reads
	// null as a bad shape would refuse a document core accepts.
	row := secretValueRow(t, "null")
	if !row.OK || row.Ref || row.Error != "" {
		t.Fatalf("null = %+v, want the empty literal", row)
	}
	if row.Resolved != "" || row.ResolveError != "" {
		t.Fatalf("null = %+v, want it to resolve to the empty string", row)
	}
	// A literal is free to be blank; only a reference is not.
	blank := secretValueRow(t, "literal-blank")
	if !blank.OK || blank.Resolved != "  " {
		t.Fatalf("literal-blank = %+v, want the spaces kept", blank)
	}
}

func TestSecretLiteralsNeverNeedAResolver(t *testing.T) {
	document := decodeSecret(t)
	literals := 0
	for _, row := range document.Values {
		if !row.OK || row.Ref {
			continue
		}
		literals++
		if row.NoResolver && row.ResolveError != "" {
			t.Fatalf("row %q: a literal needs no resolver, got %q", row.Name, row.ResolveError)
		}
	}
	if literals < 2 {
		t.Fatalf("literal rows = %d, want at least the empty one and a value", literals)
	}
	paths := make([]string, 0, len(document.Refs))
	for _, row := range document.Refs {
		paths = append(paths, row.Paths...)
	}
	// The dotted-name rows are what makes the grammar worth a table: a reader
	// can see all three shapes (named store, default store, name with a dot)
	// in one place.
	for _, want := range []string{"files.token", "my.app.key", "TOKEN"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("the refs table lost %q", want)
		}
	}
}
