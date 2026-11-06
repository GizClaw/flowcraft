package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestAttrsHaveOneHome is the anti-drift gate for the namespace split: every LLM
// fact is emitted under exactly one key. A fact the OpenTelemetry GenAI
// conventions define is emitted as gen_ai.*; a fact they do not define stays in
// llm.*. semconv.go records the decision per fact, and this test makes "add a
// constant without deciding" a build failure.
//
// It parses the source rather than keeping a second hand-written list: a list
// would drift from the constants it is supposed to guard, which is the exact
// failure mode being prevented.
func TestAttrsHaveOneHome(t *testing.T) {
	attrs := attrConstants(t, "attrs.go")
	genAI := attrConstants(t, "semconv.go")
	if len(attrs) == 0 || len(genAI) == 0 {
		t.Fatalf("parsed %d attrs.* and %d gen_ai.* constants", len(attrs), len(genAI))
	}

	genAIByValue := make(map[string]string, len(genAI))
	declared := make(map[string]string, len(attrs)+len(genAI))
	for name, value := range attrs {
		declared[value] = name
	}
	for name, value := range genAI {
		if previous, dup := declared[value]; dup {
			t.Errorf("%s and %s declare the same key %q", previous, name, value)
		}
		declared[value] = name
		genAIByValue[value] = name
	}

	// 1. Every llm.* constant is classified, with a reason either way.
	for name, value := range attrs {
		if !strings.HasPrefix(name, "AttrLLM") {
			continue
		}
		home, ok := llmFacts[value]
		if !ok {
			t.Errorf("%s (%q) has no home in llmFacts: decide gen_ai.* or llm.*", name, value)
			continue
		}
		if home.GenAI == "" && home.Note == "" {
			t.Errorf("%s: home needs either a GenAI target or a note", name)
		}
	}

	// 2. No stale entries: llmFacts must not name a key nothing declares.
	for value := range llmFacts {
		if _, ok := declared[value]; !ok {
			t.Errorf("llmFacts names %q, which no constant declares", value)
		}
		if !strings.HasPrefix(value, "llm.") {
			t.Errorf("llmFacts names %q, which is not an llm.* key", value)
		}
	}

	// 3. Every gen_ai.* constant is reachable: a replacement target for some
	// llm.* key, or declared conventions-only vocabulary.
	targets := make(map[string]string, len(llmFacts))
	for llmValue, home := range llmFacts {
		if home.GenAI != "" {
			targets[home.GenAI] = llmValue
		}
	}
	for name, value := range genAI {
		_, replaces := targets[value]
		_, conventionsOnly := genAIWithoutPredecessor[value]
		switch {
		case replaces && conventionsOnly:
			t.Errorf("%s is both a replacement target and conventions-only", name)
		case !replaces && !conventionsOnly:
			t.Errorf("%s (%q) is unreachable: add it to llmFacts or genAIWithoutPredecessor", name, value)
		}
	}

	// 4. Every replacement target is a declared gen_ai.* constant.
	for value, llmValue := range targets {
		if _, ok := genAIByValue[value]; !ok {
			t.Errorf("llmFacts maps %q to %q, which no gen_ai.* constant declares", llmValue, value)
		}
	}

	// 5. The conventions-only list does not name stale keys either.
	for value := range genAIWithoutPredecessor {
		if _, ok := genAIByValue[value]; !ok {
			t.Errorf("genAIWithoutPredecessor names %q, which no gen_ai.* constant declares", value)
		}
	}

	// 6. The pinned version is a version, so a rename stays a reviewable diff.
	parts := strings.Split(SemconvGenAIVersion, ".")
	if len(parts) < 2 {
		t.Fatalf("SemconvGenAIVersion %q is not a version", SemconvGenAIVersion)
	}
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			t.Fatalf("SemconvGenAIVersion %q is not numeric", SemconvGenAIVersion)
		}
	}
}

// attrConstants returns name -> string value for every `Attr...` constant
// declared in file.
func attrConstants(t *testing.T, file string) map[string]string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := make(map[string]string)
	ast.Inspect(parsed, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if !strings.HasPrefix(name.Name, "Attr") || i >= len(valueSpec.Values) {
					continue
				}
				lit, ok := valueSpec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", name.Name, lit.Value, err)
				}
				out[name.Name] = value
			}
		}
		return true
	})
	return out
}
