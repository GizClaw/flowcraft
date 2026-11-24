package tool_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

type readArgs struct {
	Path  string `json:"path"`
	Limit int    `json:"limit,omitempty"`
}

// TestDecodeArgsFastPath is the shape every builtin tool decodes through: one object,
// straight into the target struct.
func TestDecodeArgsFastPath(t *testing.T) {
	var args readArgs
	if err := tool.DecodeArgs("read_file", `{"path":"/a","limit":3}`, nil, &args); err != nil {
		t.Fatalf("DecodeArgs: %v", err)
	}
	if args.Path != "/a" || args.Limit != 3 {
		t.Fatalf("args = %+v", args)
	}
}

// TestDecodeArgsAcceptsAnAliasOnlyWhenTheCanonicalKeyIsAbsent keeps the schema
// authoritative: a payload carrying both spellings must not let the alias win.
func TestDecodeArgsAcceptsAnAliasOnlyWhenTheCanonicalKeyIsAbsent(t *testing.T) {
	aliases := map[string]string{"file_path": "path"}

	var args readArgs
	if err := tool.DecodeArgs("read_file", `{"file_path":"/alias"}`, aliases, &args); err != nil {
		t.Fatalf("alias: %v", err)
	}
	if args.Path != "/alias" {
		t.Fatalf("alias did not map: %+v", args)
	}

	args = readArgs{}
	if err := tool.DecodeArgs("read_file", `{"file_path":"/alias","path":"/canonical"}`, aliases, &args); err != nil {
		t.Fatalf("both keys: %v", err)
	}
	if args.Path != "/canonical" {
		t.Fatalf("the alias overrode the canonical key: %+v", args)
	}
}

// TestDecodeArgsRejectsUnknownKeysByName: silently ignoring a typo turns it into a
// "required argument missing" far from the mistake, so the error names the key and lists
// what was accepted.
func TestDecodeArgsRejectsUnknownKeysByName(t *testing.T) {
	var args readArgs
	err := tool.DecodeArgs("read_file", `{"pth":"/a"}`, nil, &args)
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want validation", err)
	}
	for _, want := range []string{`"pth"`, "path", "limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestDecodeArgsNamesAliasesInTheAcceptedList is what lets a model map its habit onto the
// schema after one refusal.
func TestDecodeArgsNamesAliasesInTheAcceptedList(t *testing.T) {
	var args readArgs
	err := tool.DecodeArgs("read_file", `{"nope":1}`, map[string]string{"file_path": "path"}, &args)
	if err == nil || !strings.Contains(err.Error(), "alias: file_path") {
		t.Fatalf("error = %v, want the alias named", err)
	}
}

func TestDecodeArgsRejectsNonObjectShapes(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":  "",
		"blank":  "   ",
		"string": `"/a"`,
		"array":  `[1,2]`,
		"junk":   `{oops`,
	} {
		var args readArgs
		if err := tool.DecodeArgs("read_file", payload, nil, &args); err == nil {
			t.Fatalf("%s: accepted %q", name, payload)
		} else if !errdefs.IsValidation(err) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
}

// TestDecodeArgsRejectsTrailingData pins the guarantee the fast path had to keep: a
// decoder that reads one value would otherwise accept two concatenated objects.
func TestDecodeArgsRejectsTrailingData(t *testing.T) {
	var args readArgs
	if err := tool.DecodeArgs("read_file", `{"path":"/a"}{"path":"/b"}`, nil, &args); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

func TestDecodeArgsRequiresAStructPointer(t *testing.T) {
	var notAPointer readArgs
	if err := tool.DecodeArgs("read_file", `{"path":"/a"}`, nil, notAPointer); err == nil {
		t.Fatal("a non-pointer target was accepted")
	}
	if err := tool.DecodeArgs("read_file", `{"path":"/a"}`, nil, &struct{}{}); err == nil {
		t.Fatal("a struct with no json fields was accepted")
	}
}

func TestDecodeArgsReportsAFieldTypeMismatch(t *testing.T) {
	var args readArgs
	err := tool.DecodeArgs("read_file", `{"path":"/a","limit":"three"}`, nil, &args)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a validation error", err)
	}
}
