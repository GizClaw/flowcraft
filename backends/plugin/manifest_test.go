package plugin

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestParseManifestValid(t *testing.T) {
	data := []byte(`
name: acme.notion-tools
version: 1.2.0
description: Notion integration tools
requires:
  core: ">=0.4.0"
  plugins:
    - acme.base@^1.0.0
provides:
  - kind: inference.Provider
    impl: acme.notion
artifacts:
  - type: layer
    path: layers/10-notion.yaml
    priority: 100
  - type: service
    transport: stdio
    command: python
    args: ["-m", "acme_plugin", "--stdio"]
    env:
      NOTION_TOKEN: ${env:NOTION_TOKEN}
    protocol_version: 1
    capabilities:
      - kind: inference.Provider
        impl: acme.notion
`)
	manifest, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.Name != "acme.notion-tools" || manifest.Version != "1.2.0" {
		t.Fatalf("manifest name/version = %s/%s", manifest.Name, manifest.Version)
	}
	if manifest.Requires.Core != ">=0.4.0" {
		t.Fatalf("requires.core = %q", manifest.Requires.Core)
	}
	if len(manifest.Provides) != 1 || string(manifest.Provides[0].Kind) != "inference.Provider" {
		t.Fatalf("provides = %+v", manifest.Provides)
	}
	if len(manifest.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(manifest.Artifacts))
	}
	if manifest.Artifacts[1].Command != "python" {
		t.Fatalf("service command = %q", manifest.Artifacts[1].Command)
	}
	if len(manifest.Artifacts[1].Capabilities) != 1 {
		t.Fatalf("service capabilities = %d, want 1", len(manifest.Artifacts[1].Capabilities))
	}
}

func TestParseManifestRejectsUnknownField(t *testing.T) {
	_, err := ParseManifest([]byte("name: acme.x\nversion: 1.0.0\nbogus: 1\n"))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for unknown field", err)
	}
}

func TestParseManifestInvalidName(t *testing.T) {
	for _, name := range []string{
		"acme",       // no dot
		"Acme.Tools", // uppercase
		"acme..tools",
		"acme.-tools",
		"",
	} {
		_, err := ParseManifest([]byte(
			"name: " + name + "\nversion: 1.0.0\n"))
		if err == nil || !errdefs.IsValidation(err) {
			t.Errorf("name %q: err = %v, want Validation", name, err)
		}
	}
}

func TestParseManifestInvalidVersion(t *testing.T) {
	for _, version := range []string{"1.0", "1", "abc", ""} {
		_, err := ParseManifest([]byte(
			"name: acme.x\nversion: " + version + "\n"))
		if err == nil || !errdefs.IsValidation(err) {
			t.Errorf("version %q: err = %v, want Validation", version, err)
		}
	}
}

func TestParseManifestInvalidRequires(t *testing.T) {
	_, err := ParseManifest([]byte(`
name: acme.x
version: 1.0.0
requires:
  core: "bogus"
`))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for bad requires.core", err)
	}
}

func TestParseManifestArtifactValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring of the error message
	}{
		{
			name: "unknown artifact type",
			body: "artifacts:\n  - type: binary\n",
			want: "unknown artifact type",
		},
		{
			name: "reserved wasm",
			body: "artifacts:\n  - type: wasm\n",
			want: "reserved",
		},
		{
			name: "layer without path",
			body: "artifacts:\n  - type: layer\n",
			want: "path is required",
		},
		{
			name: "service without transport",
			body: "artifacts:\n  - type: service\n",
			want: "transport is required",
		},
		{
			name: "stdio without command",
			body: "artifacts:\n  - type: service\n    transport: stdio\n",
			want: "requires command",
		},
		{
			name: "http without url",
			body: "artifacts:\n  - type: service\n    transport: http\n",
			want: "requires url",
		},
		{
			name: "unknown transport",
			body: "artifacts:\n  - type: service\n    transport: tcp\n",
			want: "unknown transport",
		},
		{
			name: "unsupported protocol version",
			body: "artifacts:\n  - type: service\n    transport: http\n    url: http://localhost:1\n    protocol_version: 2\n",
			want: "protocol_version 2 is not supported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(
				"name: acme.x\nversion: 1.0.0\n" + tt.body + "\n"))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("err = %v, want Validation", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseManifestDuplicateProvides(t *testing.T) {
	_, err := ParseManifest([]byte(`
name: acme.x
version: 1.0.0
provides:
  - kind: inference.Provider
    impl: acme.x
  - kind: inference.Provider
    impl: acme.x
`))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for duplicate provides", err)
	}
}

func TestParseManifestDuplicateCapabilities(t *testing.T) {
	_, err := ParseManifest([]byte(`
name: acme.x
version: 1.0.0
artifacts:
  - type: service
    transport: http
    url: http://localhost:1
    capabilities:
      - kind: inference.Provider
        impl: acme.x
  - type: service
    transport: http
    url: http://localhost:2
    capabilities:
      - kind: inference.Provider
        impl: acme.x
`))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for duplicate capabilities", err)
	}
}
