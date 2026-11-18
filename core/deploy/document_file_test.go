package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const envDocument = `version: 1
agents:
  - name: translator
    engine:
      kind: a2a
      settings:
        peer: ${env:PEER_URL}
    protocol:
      kind: a2a
      settings: {card: {version: "1.0.0", url: https://peer.example}}
`

func writeDocument(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.yaml")
	if err := os.WriteFile(path, []byte(envDocument), 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	return path
}

func TestLoadFileReadsADocument(t *testing.T) {
	t.Setenv("PEER_URL", "https://peer.example")
	specs, err := Loader{Kinds: builtinKinds().WithProtocols(a2aKind())}.LoadFile(context.Background(), writeDocument(t))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "translator" {
		t.Fatalf("specs = %+v, want the document's agent", specs)
	}
}

func TestLoadFileReportsAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Loader{Kinds: builtinKinds()}.LoadFile(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the path named", err)
	}
}

// A host prints its startup checklist before loading: RequiredPlaceholders reports every
// reference in one pass instead of failing at the first one.
func TestRequiredPlaceholdersNamesEveryVariable(t *testing.T) {
	loader := Loader{Kinds: builtinKinds()}
	requirement, err := loader.RequiredPlaceholders([]byte(envDocument))
	if err != nil {
		t.Fatalf("RequiredPlaceholders: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "PEER_URL" {
		t.Errorf("required = %v, want PEER_URL", requirement.Required)
	}
}

func TestRequiredPlaceholdersFileReadsTheDocument(t *testing.T) {
	loader := Loader{Kinds: builtinKinds()}
	path := writeDocument(t)
	requirement, err := loader.RequiredPlaceholdersFile(context.Background(), path)
	if err != nil {
		t.Fatalf("RequiredPlaceholdersFile: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "PEER_URL" {
		t.Errorf("required = %v, want PEER_URL", requirement.Required)
	}
	// A host prints this map as "edit X in <file>", so the name has to be the
	// document the source read, not a constant.
	if got := requirement.Sources["PEER_URL"]; len(got) != 1 || got[0] != path {
		t.Errorf("sources = %v, want the document's path", got)
	}
}

func TestRequiredPlaceholdersFileReportsAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Loader{Kinds: builtinKinds()}.RequiredPlaceholdersFile(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the path named", err)
	}
}
