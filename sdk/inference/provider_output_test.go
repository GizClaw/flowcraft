package inference

import (
	"encoding/json"
	"strings"
	"testing"
)

type testProviderOutput struct {
	provider string
	id       string
	value    string
}

func (o testProviderOutput) ProviderID() string  { return o.provider }
func (o testProviderOutput) ExtensionID() string { return o.id }
func (o testProviderOutput) Validate() error     { return nil }
func (o testProviderOutput) Clone() ProviderOutput {
	return testProviderOutput{provider: o.provider, id: o.id, value: o.value}
}

func TestProviderOutputsCloneAndValidate(t *testing.T) {
	outputs := ProviderOutputs{
		testProviderOutput{provider: "openai", id: "web_search", value: "a"},
		testProviderOutput{provider: "bytedance", id: "web_search", value: "b"},
	}
	cloned := outputs.Clone()
	cloned[0] = testProviderOutput{provider: "openai", id: "web_search", value: "mutated"}
	if outputs[0].(testProviderOutput).value != "a" {
		t.Fatalf("clone mutated source: %#v", outputs[0])
	}
	if err := cloned.Validate(); err != nil {
		t.Fatalf("cloned Validate: %v", err)
	}
	if err := (ProviderOutputs{nil}).Validate(); err == nil {
		t.Fatal("nil output unexpectedly validated")
	}
	if err := (ProviderOutputs{testProviderOutput{id: "bad.id", value: "x"}}).Validate(); err == nil {
		t.Fatal("invalid provider identity unexpectedly validated")
	}
}

func TestProviderOutputsReplace(t *testing.T) {
	var outputs ProviderOutputs
	outputs.Replace(testProviderOutput{provider: "openai", id: "web_search", value: "first"})
	outputs.Replace(testProviderOutput{provider: "openai", id: "web_search", value: "second"})
	outputs.Replace(testProviderOutput{provider: "bytedance", id: "web_search", value: "ark"})
	if len(outputs) != 2 {
		t.Fatalf("outputs = %#v, want 2", outputs)
	}
	if outputs[0].(testProviderOutput).value != "second" {
		t.Fatalf("openai output was not replaced: %#v", outputs[0])
	}
}

func TestProviderOutputsMarshalIncludesIdentity(t *testing.T) {
	raw, err := json.Marshal(ProviderOutputs{
		testProviderOutput{provider: "openai", id: "web_search", value: "x"},
	})
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	body := string(raw)
	for _, want := range []string{`"provider":"openai"`, `"extension":"web_search"`, `"value"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s does not contain %s", body, want)
		}
	}
}
