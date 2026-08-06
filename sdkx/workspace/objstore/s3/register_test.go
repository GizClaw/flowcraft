package s3

import (
	"context"
	"testing"

	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
)

func TestRegisterBuildsObjectStoreWorkspace(t *testing.T) {
	builder := workspaceconfig.NewBuilder(workspaceconfig.Deps{})
	client := newMockClient()
	if err := Register(builder, client); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc, err := workspaceconfig.Parse([]byte(`
version: v1
workspaces:
  project:
    driver: objstore.s3
    settings:
      bucket: my-bucket
      prefix: workspace/prod
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ws, ok := registry.Get("project")
	if !ok {
		t.Fatal("project workspace missing")
	}
	if err := ws.Write(context.Background(), "a.txt", []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ws.Read(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Read = %q, want hello", got)
	}
	client.mu.RLock()
	_, ok = client.data["workspace/prod/a.txt"]
	client.mu.RUnlock()
	if !ok {
		t.Fatal("object key did not include the configured prefix")
	}
}

func TestRegisterRejectsMissingBucketAndUnknownSettings(t *testing.T) {
	builder := workspaceconfig.NewBuilder(workspaceconfig.Deps{})
	if err := Register(builder, newMockClient()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, body := range map[string]string{
		"missing bucket": "version: v1\nworkspaces:\n  x: {driver: objstore.s3}\n",
		"unknown field":  "version: v1\nworkspaces:\n  x:\n    driver: objstore.s3\n    settings: {bucket: b, bogus: true}\n",
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := workspaceconfig.Parse([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := builder.Build(context.Background(), doc); err == nil {
				t.Fatal("Build accepted invalid settings")
			}
		})
	}
}
