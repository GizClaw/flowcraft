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
	specs, err := Loader{Kinds: BuiltinKinds().WithProtocols(a2aKind())}.LoadFile(context.Background(), writeDocument(t))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "translator" {
		t.Fatalf("specs = %+v, want the document's agent", specs)
	}
}

func TestLoadFileReportsAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Loader{Kinds: BuiltinKinds()}.LoadFile(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the path named", err)
	}
}

// A host prints its startup checklist before loading: RequiredEnv reports every
// reference in one pass instead of failing at the first one.
func TestRequiredEnvNamesEveryVariable(t *testing.T) {
	loader := Loader{Kinds: BuiltinKinds()}
	requirement, err := loader.RequiredEnv([]byte(envDocument))
	if err != nil {
		t.Fatalf("RequiredEnv: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "PEER_URL" {
		t.Errorf("required = %v, want PEER_URL", requirement.Required)
	}
}

func TestRequiredEnvFileReadsTheDocument(t *testing.T) {
	loader := Loader{Kinds: BuiltinKinds()}
	requirement, err := loader.RequiredEnvFile(writeDocument(t))
	if err != nil {
		t.Fatalf("RequiredEnvFile: %v", err)
	}
	if len(requirement.Required) != 1 || requirement.Required[0] != "PEER_URL" {
		t.Errorf("required = %v, want PEER_URL", requirement.Required)
	}
}

func TestRequiredEnvFileReportsAnUnreadableFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Loader{Kinds: BuiltinKinds()}.RequiredEnvFile(missing)
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the path named", err)
	}
}
