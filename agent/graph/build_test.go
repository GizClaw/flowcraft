// Document validation: what Build refuses and what it accepts.
package graph

import (
	"strings"
	"testing"
)

func TestBuildRejectsBrokenWiring(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown kind", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: nope}\n", "not registered"},
		{"missing entry", "version: 1\nnodes:\n  - {id: a, kind: const}\n", "entry is required"},
		{"entry not a node", "version: 1\nentry: x\nnodes:\n  - {id: a, kind: const}\n", "not a declared node"},
		{"unknown edge", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: const}\nedges:\n  - {from: a, to: b}\n", "unknown target"},
		{"unreachable", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: const}\n  - {id: b, kind: const}\nedges:\n  - {from: a, to: end}\n", "unreachable"},
		{"bad condition", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: const}\nedges:\n  - {from: a, to: end, when: \"!!!\"}\n", "invalid condition"},
		{"duplicate id", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: const}\n  - {id: a, kind: const}\n", "declared twice"},
		{"output not declared", "version: 1\nentry: a\nnodes:\n  - {id: a, kind: const}\noutput: {key: nope}\n", "not a declared state key"},
		{"append on text", "version: 1\nentry: a\nstate: {x: {type: text, merge: append}}\nnodes:\n  - {id: a, kind: const}\n", "merge: append"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildErr(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNegativeLimitsRejected(t *testing.T) {
	for _, body := range []string{
		`version: 1
entry: a
limits: {max_steps: -1}
nodes:
  - {id: a, kind: const}
`,
		`version: 1
entry: a
limits: {max_nodes: -1}
nodes:
  - {id: a, kind: const}
`,
		`version: 1
entry: a
limits: {concurrency: -1}
nodes:
  - {id: a, kind: const}
`,
		`version: 1
entry: a
limits: {timeout: nope}
nodes:
  - {id: a, kind: const}
`,
	} {
		if _, err := buildErr(t, body); err == nil {
			t.Errorf("Build = nil for %q, want an error", body)
		}
	}
}

// The documented default spelling is accepted.
func TestMergeSingleLiteralIsAccepted(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  flag: {type: text, merge: single}
nodes:
  - {id: a, kind: const, config: {key: flag, value: x}}
edges:
  - {from: a, to: end}
output: {key: flag}
`)
	if _, err := drain(t, g); err != nil {
		t.Fatalf("run: %v", err)
	}
}
