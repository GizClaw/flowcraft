package host

import (
	"context"
	"fmt"
	"iter"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// The benchmarks here measure the three costs a deployment pays: once per process, once per
// conversation, and once per turn. The turn is the hot one — a conversation that runs twenty turns pays
// the assembly twenty times — and it is the wrapper this package adds that the agent benchmarks cannot
// see.

const benchTargets = 4

// benchPlan is a process with several model targets and one backend, which is what the router and the
// tool table are built over.
func benchPlan() deploy.ProcessPlan {
	plan := deploy.ProcessPlan{}
	for i := range benchTargets {
		name := string(rune('a' + i))
		plan.Providers = append(plan.Providers, deploy.PlannedProvider{Name: name, Impl: "fake",
			New: func() (*inference.Provider, error) {
				return &inference.Provider{Generator: generator{}, Catalog: catalog{
					models: []model.ModelDescriptor{modelRef("openai", "gpt-5")},
				}}, nil
			}})
	}
	backend := &boundary{log: &journal{}, filesystemBounds: true}
	plan.Backends = []deploy.PlannedBackend{boundBackend("local", backend)}
	plan.Tools = []deploy.PlannedToolSource{plannedSource("workspace")}
	plan.Observability = deploy.Observability{Trace: telemetry.TraceNone}
	return plan
}

// BenchmarkOpenProcess is the once-per-boot cost: the plan's closures, the router, and the tool table.
func BenchmarkOpenProcess(b *testing.B) {
	plan := benchPlan()
	b.ReportAllocs()
	for b.Loop() {
		res, err := Open(context.Background(), plan, benchOptions())
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		_ = res.Close()
	}
}

// benchAgents returns a table with one deployment and no sessions.
// benchDeployment is sessionDeployment for a benchmark.
func benchDeployment(b *testing.B, name string, grant policy.Grant, log *journal, backend *boundary) deploy.Deployment {
	b.Helper()
	return deploy.Deployment{
		Name:    name,
		Grant:   grant,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &sessionProgram{log: log}, nil }},
	}
}

func benchAgents(b *testing.B) *Agents {
	b.Helper()
	res, err := Open(context.Background(), benchPlan(), benchOptions())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = res.Close() })
	agents := NewAgents(res)
	dep := benchDeployment(b, "referee", permissiveGrant("local"), &journal{}, &boundary{filesystemBounds: true})
	if err := agents.Add(dep); err != nil {
		b.Fatalf("Add: %v", err)
	}
	return agents
}

// BenchmarkOpenSession is the per-conversation cost: a backend instance, the file handles, the view and
// the driver.
func BenchmarkOpenSession(b *testing.B) {
	agents := benchAgents(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		id := "s" + string(rune('a'+i%26))
		if _, err := agents.Open(context.Background(), id, "referee"); err != nil {
			b.Fatalf("Open: %v", err)
		}
		if err := agents.CloseSession(context.Background(), id); err != nil {
			b.Fatalf("CloseSession: %v", err)
		}
	}
}

// BenchmarkEvictIdle is the idle sweep over a large table: ten thousand open sessions with two
// hundred of them past the horizon. It reports ns/sweep, the wall time EvictIdle spends across its
// locked scan (the expired sessions carry no driver or boundary, so closing one is a nil check).
// That scan is what every Open, Run and Steer waits behind. The setup outside the timer keeps the
// measurement on the sweep; StopTimer would read the process's memory stats on every iteration and
// drown the number in stop-the-world pauses.
func BenchmarkEvictIdle(b *testing.B) {
	const (
		total   = 10_000
		expired = 200
	)
	agents := benchAgents(b)
	deploy := agents.entries["referee"]
	now := time.Now()
	old := now.Add(-2 * time.Minute)

	// Sessions are seeded into the table directly rather than opened: ten thousand real
	// boundaries would measure the sandbox, not the age index.
	for i := range total {
		session := &Session{id: fmt.Sprintf("s%d", i), agent: "referee", deploy: deploy}
		session.touchNow(now)
		agents.sessions[session.id] = session
		agents.byAge = append(agents.byAge, session)
	}
	live := make([]*Session, 0, total)
	aged := make([]*Session, 0, expired)

	b.ReportAllocs()
	var swept time.Duration
	for range b.N {
		// Age a batch scattered across the table, the shape a sweep finds after a period of
		// opens. The swept sessions go back in as the next batch, so the table keeps both its
		// size and its shape without allocating a session per iteration.
		live = live[:0]
		for _, session := range agents.byAge {
			if session != nil {
				live = append(live, session)
			}
		}
		aged = aged[:0]
		for k := range expired {
			session := live[k*len(live)/expired]
			session.touchNow(old)
			aged = append(aged, session)
		}

		start := time.Now()
		ids, err := agents.EvictIdle(context.Background(), "referee", now)
		swept += time.Since(start)

		if err != nil {
			b.Fatalf("EvictIdle: %v", err)
		}
		if len(ids) != expired {
			b.Fatalf("evicted %d sessions, want %d", len(ids), expired)
		}
		for _, session := range aged {
			session.closing = false
			session.touchNow(now)
			agents.sessions[session.id] = session
			agents.byAge = append(agents.byAge, session)
		}
	}
	b.ReportMetric(float64(swept.Nanoseconds())/float64(b.N), "ns/sweep")
}

// streamingDriver yields a handful of events per turn, which is the shape a real turn has before any
// model call.
type streamingDriver struct{}

func (streamingDriver) Run(context.Context, agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Text{Text: "hello"}, nil) {
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (streamingDriver) Close() error { return nil }

type streamingProgram struct{}

func (streamingProgram) Open(agent.SessionInfo) (agent.Driver, error) { return streamingDriver{}, nil }
func (streamingProgram) Close() error                                 { return nil }

// BenchmarkRunTurn is the hot path: the request assembly, the decorator, the driver's stream and the
// host's own span. It is what a conversation pays per round.
func BenchmarkRunTurn(b *testing.B) {
	agents := benchRunAgents(b)
	msg := message.NewTextMessage(message.RoleUser, "hi")
	b.ReportAllocs()
	for b.Loop() {
		for _, err := range agents.Run(context.Background(), "s1", msg) {
			if err != nil {
				b.Fatalf("Run: %v", err)
			}
		}
	}
}

func benchRunAgents(b *testing.B) *Agents {
	b.Helper()
	res, err := Open(context.Background(), benchPlan(), benchOptions())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = res.Close() })
	agents := NewAgents(res)
	dep := deploy.Deployment{
		Name:    "referee",
		Grant:   permissiveGrant("local"),
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return streamingProgram{}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		b.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(context.Background(), "s1", "referee"); err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = agents.Close(context.Background()) })
	return agents
}

// BenchmarkLoaderProcess is the once-per-boot document cost: strict decoding through the factories and
// the plan it builds. config.Load is benchmarked on its own; the per-entry plan loop is not.
func BenchmarkLoaderProcess(b *testing.B) {
	impls := deploy.NewImpls().
		Provider(deploy.Impl("openai", func(map[string]any) (*inference.Provider, error) {
			return &inference.Provider{}, nil
		})).
		Backend(deploy.BackendImpl("local", func(map[string]any, sandbox.Confine) (sandbox.Runner, error) {
			return nil, nil
		})).
		ToolSource(deploy.Impl("files", func(map[string]any) (tool.Source, error) {
			return tooltest.Source(), nil
		}))
	loader := deploy.NewLoader(impls)
	layer := sources.Bytes("runtime.yaml", []byte(`
version: 1
sandbox:
  - {name: local, impl: local}
inference:
  providers:
    - name: gpt
      impl: openai
      settings: {base_url: "https://api.openai.com/v1"}
tools:
  sources:
    - {name: workspace, impl: files}
observability: {trace: none, bodies: false}
`))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := loader.Process(context.Background(), layer); err != nil {
			b.Fatalf("Process: %v", err)
		}
	}
}

// benchOptions opens a process the way a benchmark wants: no chain, and the stub engine, so the number
// measures the process build rather than an executor.
func benchOptions() Options {
	o := Options{}
	o.Dispatcher = func(catalog tool.Catalog, chain []tool.Middleware) tool.Dispatcher {
		return stubDispatch{catalog: catalog, chain: chain}
	}
	o.View = func(catalog tool.Catalog, allowed policy.ToolSet) (tool.ViewFactory, error) {
		return statelessFactory(catalog), nil
	}
	return o
}
