package yaml_test

import (
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// settingsNode parses a YAML literal into a *yamlv3.Node, the
// shape deploy.DecodeSettings expects on
// [deploy.ResourceInput.Settings].
func settingsNode(t *testing.T, body string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("settings yaml: %v", err)
	}
	return &node
}
