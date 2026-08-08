package deploy_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	graphconfig "github.com/GizClaw/flowcraft/sdk/graph/config"
	"github.com/GizClaw/flowcraft/sdkx/agent/jsrt"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

func TestGraphFactoryJavaScriptDeployment(t *testing.T) {
	engines := agent.NewRegistry()
	engines.MustRegister(graphconfig.NewFactory())
	builder := deploy.NewBuilder(engines)
	builder.MustRegisterResource(jsrt.NewDeployFactory())

	document, err := deploy.Parse([]byte(`
version: v1
resources:
  js:
    kind: agent.ScriptRuntime
    impl: js
    settings:
      pool_size: 1
agents:
  scripted:
    engine:
      kind: graph
      settings:
        graph: |
          name: deployed-script
          entry: run
          nodes:
            - id: run
              type: script
              config:
                runtime: js
                source: 'board.setVar("executed", true); signal.done();'
          edges: []
    deps:
      script_runtime: js
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	result, err := builder.Build(context.Background(), document)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	instance, ok := result.Instance("scripted")
	if !ok {
		t.Fatal("scripted agent was not built")
	}
	runResult, err := instance.Execute(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runResult.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want %q", runResult.Status, agent.StatusCompleted)
	}
	executed, ok := runResult.LastBoard.GetVar("executed")
	if !ok || executed != true {
		t.Fatalf("executed = %#v, present=%v", executed, ok)
	}
}
