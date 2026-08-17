package plugin

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestParsePluginsSection(t *testing.T) {
	config, err := ParsePluginsSection([]byte(`
dirs:
  - ./plugins
  - ./vendor/plugins
enabled:
  - acme.notion-tools@^1.0.0
  - acme.base
`))
	if err != nil {
		t.Fatalf("ParsePluginsSection: %v", err)
	}
	if len(config.Dirs) != 2 || config.Dirs[0] != "./plugins" {
		t.Fatalf("dirs = %v", config.Dirs)
	}
	if len(config.Enabled) != 2 {
		t.Fatalf("enabled = %v", config.Enabled)
	}
}

func TestParsePluginsSectionValidation(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"empty dirs", "dirs: []\n"},
		{"missing dirs", "{}\n"},
		{"empty dir entry", "dirs:\n  - \"\"\n"},
		{"bad enabled entry", "dirs: [./plugins]\nenabled:\n  - \"@^1.0.0\"\n"},
		{"unknown field", "dirs: [./plugins]\nbogus: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePluginsSection([]byte(tt.data))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("err = %v, want Validation", err)
			}
		})
	}
}
