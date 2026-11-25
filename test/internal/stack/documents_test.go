package stack

import (
	"strings"
	"testing"
)

// TestTheDelegationGraphIsTheBaseGraphWidened keeps the second document honest.
//
// It exists because the two files are one graph: the delegation case needs the same model/tool
// wiring with two tools more in the request and a sentence of prompt. A copy that is edited on one
// side only is the drift this test is here to catch, and it is cheaper than generating the file.
func TestTheDelegationGraphIsTheBaseGraphWidened(t *testing.T) {
	base, err := documents.ReadFile("testdata/graph.yaml")
	if err != nil {
		t.Fatalf("read graph.yaml: %v", err)
	}
	widened, err := documents.ReadFile("testdata/graph-delegation.yaml")
	if err != nil {
		t.Fatalf("read graph-delegation.yaml: %v", err)
	}
	want := strings.ReplaceAll(string(base), basePrompt, delegationPrompt)
	want = strings.ReplaceAll(want, baseTools, delegationTools)
	if string(widened) != want {
		t.Fatalf("graph-delegation.yaml drifted from graph.yaml; it must be graph.yaml with only "+
			"the prompt and the tool list changed.\n--- want\n%s\n--- got\n%s", want, widened)
	}
}

// The two substitutions, kept beside the test so a change to either document is visible here.
const (
	basePrompt       = "Write the note the user asked for, then answer."
	delegationPrompt = "Do the work you are asked for. You may delegate to writer, answer a delegated " +
		"agent, or run a command."
	baseTools       = "tools: {names: [write_file]}"
	delegationTools = "tools: {names: [write_file, bash, delegate, delegation_answer, delegation_status, " +
		"ask_user]}"
)
