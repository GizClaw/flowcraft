package deploy

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// benchSpec is one agent with a runtime, the shape a real deployment has.
func benchSpec(b *testing.B, name, kind string) Spec {
	b.Helper()
	root := b.TempDir()
	return Spec{Name: name, Engine: EngineSpec{Kind: kind}, Runtime: runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
		Tools: runtime.ToolsConfig{Enable: []string{"files"}},
	}}
}

func benchHost() Host {
	return Host{RuntimeOptions: []runtime.Option{driver(&scripted{})}}
}

// benchManager deploys one local agent with no runtime: the layer's own costs,
// without the substrate a real deployment builds.
func benchManager(b *testing.B) *Manager {
	b.Helper()
	manager := New(BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hi"}
	})), benchHost())
	b.Cleanup(func() { _ = manager.Close() })
	spec := benchSpec(b, "a", "echo")
	if err := manager.Add(context.Background(), spec); err != nil {
		b.Fatalf("Add: %v", err)
	}
	return manager
}

// BenchmarkManagerSession is the lookup a host does per request.
func BenchmarkManagerSession(b *testing.B) {
	manager := benchManager(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, ok := manager.Session("a", "conv-1"); !ok {
			b.Fatal("the deployed agent has no session")
		}
	}
}

// BenchmarkSessionRun is the whole deployed path: manager handle, session, agent
// slot, engine, events.
func BenchmarkSessionRun(b *testing.B) {
	manager := benchManager(b)
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		b.Fatal("the deployed agent has no session")
	}
	msg := message.NewTextMessage(message.RoleUser, "hi")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, err := range session.Run(context.Background(), msg) {
			if err != nil {
				b.Fatalf("run: %v", err)
			}
		}
	}
}

// BenchmarkManagerAdd measures deploying an agent: validation, digest, table and
// no runtime.
func BenchmarkManagerAdd(b *testing.B) {
	manager := New(BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hi"}
	})), benchHost())
	b.Cleanup(func() { _ = manager.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		spec := benchSpec(b, fmt.Sprintf("a%d", i), "echo")
		if err := manager.Add(context.Background(), spec); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
}

// BenchmarkManagerReplace measures the swap: a fresh deployment built, then the
// old one closed.
func BenchmarkManagerReplace(b *testing.B) {
	manager := benchManager(b)
	spec := benchSpec(b, "a", "echo")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := manager.Replace(context.Background(), spec); err != nil {
			b.Fatalf("Replace: %v", err)
		}
	}
}
