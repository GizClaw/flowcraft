package deploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The fixtures are deliberately tiny: what these tests are about is the loader's shape — what
// it decodes, what it refuses, and what it leaves to the host — not the behaviour of a real
// provider or backend.

type providerSettings struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

type backendSettings struct {
	Root string `json:"root"`
}

type programSettings struct {
	Graph string `json:"graph"`
}

type middlewareSettings struct {
	Seconds int `json:"seconds"`
}

type hookSettings struct {
	Dir string `json:"dir"`
}

type fakeSource struct{}

func (f *fakeSource) Tools() []tool.Tool         { return nil }
func (f *fakeSource) LazyTools() []tool.LazyTool { return nil }

// builds records what the factories saw, so a test can assert decoding without deploy handing
// the decoded value back: the plan carries implementations and closures, and nothing else.
type builds struct {
	providers      int
	backends       int
	tools          int
	middleware     int
	hooks          int
	programs       int
	seenProvider   providerSettings
	seenProgram    programSettings
	seenMiddleware middlewareSettings
	seenHook       hookSettings
}

func impls(b *builds) *deploy.Impls {
	return deploy.NewImpls().
		Provider(deploy.Impl("openai", func(s providerSettings) (*inference.Provider, error) {
			b.providers++
			b.seenProvider = s
			return &inference.Provider{}, nil
		})).
		Backend(deploy.BackendImpl("local", func(s backendSettings, _ sandbox.Confine) (sandbox.Runner, error) {
			b.backends++
			return nil, nil
		})).
		ToolSource(deploy.Impl("files", func(s backendSettings) (tool.Source, error) {
			b.tools++
			return &fakeSource{}, nil
		})).
		Middleware(deploy.MiddlewareImpl("timeout", func(s middlewareSettings, _ tool.Catalog) (tool.Middleware, error) {
			b.middleware++
			b.seenMiddleware = s
			return func(next tool.Dispatch) tool.Dispatch { return next }, nil
		})).
		// The telemetry link is only worth building when the process records tool calls: below
		// that level it would emit the very spans the level promised to skip.
		Middleware(deploy.MiddlewareImpl("telemetry", func(_ middlewareSettings, _ tool.Catalog) (tool.Middleware, error) {
			b.middleware++
			return func(next tool.Dispatch) tool.Dispatch { return next }, nil
		}).AtTrace(telemetry.TraceCall)).
		Hook(deploy.Impl("audit", func(s hookSettings) (agent.Hooks, error) {
			b.hooks++
			b.seenHook = s
			return agent.Hooks{}, nil
		})).
		Program(deploy.Impl("graph", func(s programSettings) (agent.Program, error) {
			b.programs++
			b.seenProgram = s
			return nil, nil
		}))
}

const processDoc = `
version: 1
sandbox:
  - name: local
    impl: local
    settings: { root: "/srv" }
inference:
  providers:
    - name: gpt
      impl: openai
      settings: { base_url: "https://api.openai.com", model: "gpt-5" }
tools:
  sources:
    - name: workspace
      impl: files
      settings: { root: "/srv" }
skills:
  roots: ["/srv/skills"]
observability: { trace: call, bodies: false }
`

// TestProcessResolvesWithoutBuilding is the package's central promise: a plan holds settings
// and closures, never an object, so a document can be validated where nothing can be dialled.
func TestProcessResolvesWithoutBuilding(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))

	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if built.providers+built.backends+built.tools+built.programs != 0 {
		t.Fatalf("loading built something: %+v", built)
	}
	if len(plan.Providers) != 1 || plan.Providers[0].Name != "gpt" {
		t.Fatalf("providers = %+v", plan.Providers)
	}
	if len(plan.SkillRoots) != 1 || plan.SkillRoots[0] != pathtest.Abs("srv/skills") {
		t.Fatalf("skill roots = %v", plan.SkillRoots)
	}

	// Building is the host's step, and it is where the decoded settings surface: the plan does
	// not hand them back, so the factory is the only reader.
	if _, err := plan.Providers[0].New(); err != nil {
		t.Fatalf("New: %v", err)
	}
	if built.providers != 1 {
		t.Fatalf("New did not build: %+v", built)
	}
	if built.seenProvider.Model != "gpt-5" {
		t.Fatalf("factory saw %+v, want the decoded block", built.seenProvider)
	}
}

// TestUnknownSettingIsADocumentError is the reason settings are decoded at load: a misspelt
// key inside a settings block would otherwise be silently ignored.
func TestUnknownSettingIsADocumentError(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))
	doc := strings.Replace(processDoc, `model: "gpt-5"`, `modle: "gpt-5"`, 1)

	_, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if !strings.Contains(err.Error(), "modle") {
		t.Fatalf("error %q does not name the unknown key", err)
	}
}

// TestUnknownKindIsADocumentError: the document names an implementation this binary does not
// carry, which is a document mistake — the registry is fixed at build time.
func TestUnknownKindIsADocumentError(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))
	doc := strings.Replace(processDoc, "impl: local", "impl: bwrap", 1)

	_, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("err = %v, want a validation error naming the kind", err)
	}
}

func TestDocumentVersionIsEnforced(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))
	doc := strings.Replace(processDoc, "version: 1", "version: 2", 1)

	_, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestDuplicateRegistrationIsRefused(t *testing.T) {
	built := &builds{}
	table := impls(built).Provider(deploy.Impl("openai", func(s providerSettings) (*inference.Provider, error) {
		return &inference.Provider{}, nil
	}))
	if table.Err() == nil {
		t.Fatal("a duplicate kind registered silently")
	}
	loader := deploy.NewLoader(table)
	if _, err := loader.Process(context.Background(), sources.Bytes("r.yaml", []byte(platformRoots(processDoc)))); err == nil {
		t.Fatal("a loader ran with a broken kind table")
	}
}

func TestDuplicateEntryNameIsRefused(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))
	doc := `
version: 1
inference:
  providers:
    - name: gpt
      impl: openai
      settings: { model: "gpt-5" }
    - name: gpt
      impl: openai
      settings: { model: "other" }
`
	_, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error for the repeated name", err)
	}
}

const agentDoc = `
version: 1
agents:
  - name: helper
    program: { impl: graph, settings: { graph: "main" } }
    grant:
      sandbox: local
      models: [{ id: { provider: "openai", name: "gpt-5" } }]
      limits: { idle: 30m, max_sessions: 64 }
    hooks: [{ impl: audit }]
`

// capabilities is what a built process reports: the backend instances its plan configured and
// the models its providers serve. Registered kinds are not here — the loader checks those
// against the table it already has.
func capabilities(plan deploy.ProcessPlan) deploy.Capabilities {
	return deploy.Capabilities{
		Backends: plan.BackendNames(),
		Models:   []string{"openai/gpt-5"},
	}
}

// TestDeploymentResolvesAgainstCapabilities is the seam between the two documents: an agent
// selects what the process carries, and every selection is checked here rather than at the
// first session.
func TestDeploymentResolvesAgainstCapabilities(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	deployments, err := loader.Deployments(context.Background(), capabilities(plan),
		sources.Bytes("agent.yaml", []byte(agentDoc)))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 1 {
		t.Fatalf("deployments = %d, want the one agent", len(deployments))
	}
	deployment := deployments[0]
	if deployment.Name != "helper" {
		t.Fatalf("name = %q", deployment.Name)
	}
	if plan.Observability.Trace != telemetry.TraceCall || plan.Observability.Bodies {
		t.Fatalf("observability = %+v, want the process document's", plan.Observability)
	}
	if deployment.Grant.Sandbox != "local" {
		t.Fatalf("grant sandbox = %q", deployment.Grant.Sandbox)
	}
	if built.programs != 0 {
		t.Fatal("resolving a deployment built the program; that is host.Add's job")
	}
}

// TestAnAgentDocumentHasNoPrompt keeps the responsibility where it belongs: the system framing
// is compiled by the program — a graph's nodes carry it — so a deployment carries no prompt, and
// a document that writes one is refused rather than silently ignored.
func TestAnAgentDocumentHasNoPrompt(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := strings.Replace(agentDoc, "name: helper", "name: helper\n    prompt: you are helpful", 1)

	_, err = loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("agent.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error naming the unknown field", err)
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("error %q does not name the field", err)
	}
}

// TestMissingCapabilityIsNotAvailable is the other error class: the document is fine, this
// process cannot serve it.
func TestMissingCapabilityIsNotAvailable(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := capabilities(plan)
	caps.Models = nil // this process serves no models

	_, err = loader.Deployments(context.Background(), caps, sources.Bytes("agent.yaml", []byte(agentDoc)))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want a NotAvailable error", err)
	}
}

// TestUnknownProgramKindIsADocumentError: the program table is the application's build-time
// registration, so a document naming a kind nobody registered is a document error — the same
// class as an unknown provider kind, not the same as a process that lacks a resource.
func TestUnknownProgramKindIsADocumentError(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := strings.Replace(agentDoc, "impl: graph", "impl: a2a", 1)

	_, err = loader.Deployments(context.Background(), capabilities(plan),
		sources.Bytes("agent.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error naming the kind", err)
	}
	if !strings.Contains(err.Error(), "a2a") {
		t.Fatalf("error %q does not name the kind", err)
	}
}

func TestMissingBackendIsNotAvailable(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := capabilities(plan)
	caps.Backends = nil

	if _, err := loader.Deployments(context.Background(), caps, sources.Bytes("agent.yaml", []byte(agentDoc))); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
}

// TestLayersMergeByPriority is the reason the loader takes a stack: a deployment describes a
// base and an overlay, and the overlay wins.
func TestLayersMergeByPriority(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	base := sources.Bytes("base.yaml", []byte(`
version: 1
observability: { trace: call, bodies: false }
inference:
  providers:
    - name: gpt
      impl: openai
      settings: { model: "gpt-5" }
`))
	overlay := sources.Bytes("prod.yaml", []byte(`
inference:
  providers:
    - name: gpt
      impl: openai
      settings: { model: "gpt-6" }
`))
	overlay = sources.Over(overlay, 10)

	plan, err := loader.Process(context.Background(), base, overlay)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(plan.Providers) != 1 {
		t.Fatalf("providers = %d, want the overlay to replace the base entry", len(plan.Providers))
	}
	if _, err := plan.Providers[0].New(); err != nil {
		t.Fatalf("New: %v", err)
	}
	if built.seenProvider.Model != "gpt-6" {
		t.Fatalf("factory saw model %q, want the overlay's", built.seenProvider.Model)
	}
}

// TestOneDocumentCarriesManyAgents is the shape a fleet needs: nine seats described once. A
// single-agent document is the same list with one entry, so there is no second variant to keep
// in step.
func TestOneDocumentCarriesManyAgents(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := `
version: 1
agents:
  - name: referee
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
  - name: seat_1
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
  - name: seat_2
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
`
	deployments, err := loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("agents.yaml", []byte(platformRoots(doc))))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 3 {
		t.Fatalf("deployments = %d, want 3", len(deployments))
	}
	if deployments[0].Name != "referee" || deployments[2].Name != "seat_2" {
		t.Fatalf("names = %q..%q, want declaration order", deployments[0].Name, deployments[2].Name)
	}
}

func TestAnAgentDocumentNeedsAtLeastOneAgent(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	_, err = loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("agents.yaml", []byte("version: 1\nagents: []\n")))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestDuplicateAgentNameIsRefused(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := `
version: 1
agents:
  - name: twin
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
  - name: twin
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
`
	_, err = loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("agents.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "twin") {
		t.Fatalf("err = %v, want a validation error naming the agent", err)
	}
}

// TestErrorsNameTheAgent is what a document with nine agents needs: a failure must say which
// one, or an operator is reading nine entries to find the broken one.
func TestErrorsNameTheAgent(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := capabilities(plan)
	caps.Models = nil
	doc := `
version: 1
agents:
  - name: referee
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
  - name: seat_1
    program: { impl: graph }
    grant:
      models: [{ id: { provider: "openai", name: "gpt-5" } }]
      limits: { idle: 30m }
`
	_, err = loader.Deployments(context.Background(), caps, sources.Bytes("agents.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if !strings.Contains(err.Error(), "seat_1") {
		t.Fatalf("error %q does not name the agent", err)
	}
}

// TestASequenceIsReplacedWhole pins the merge rule the documents inherit from config: a
// mapping merges key by key, a sequence is replaced. An overlay that writes agents: therefore
// states the fleet it wants, rather than patching one seat — and the same rule applies to
// providers and backends, so there is one answer for every list in every document.
func TestASequenceIsReplacedWhole(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	base := sources.Bytes("base.yaml", []byte(`
version: 1
agents:
  - name: seat_1
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
  - name: seat_2
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
`))
	overlay := sources.Over(sources.Bytes("prod.yaml", []byte(`
agents:
  - name: seat_2
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
`)), 10)

	deployments, err := loader.Deployments(context.Background(), capabilities(plan), base, overlay)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 1 || deployments[0].Name != "seat_2" {
		t.Fatalf("deployments = %d %v, want the overlay's list only", len(deployments), deployments)
	}
	if deployments[0].Program.Impl != "graph" {
		t.Fatalf("program = %q, want the overlay's entry", deployments[0].Program.Impl)
	}
}

// TestAScalarOverlayWins is the other half of the rule: a key that both layers set takes the
// higher-priority value, which is how an operator changes one agent's setting without copying
// the document.
func TestAScalarOverlayWins(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	base := sources.Bytes("base.yaml", []byte(`
version: 1
agents:
  - name: seat_2
    program: { impl: graph }
    grant: { limits: { idle: 30m, max_sessions: 64 } }
`))
	overlay := sources.Over(sources.Bytes("prod.yaml", []byte(`
agents:
  - name: seat_2
    program: { impl: graph }
    grant: { limits: { idle: 30m, max_sessions: 8 } }
`)), 10)

	deployments, err := loader.Deployments(context.Background(), capabilities(plan), base, overlay)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 1 {
		t.Fatalf("deployments = %d, want the one agent both layers name", len(deployments))
	}
	if deployments[0].Grant.Limits.MaxSessions != 8 {
		t.Fatalf("max_sessions = %d, want the overlay's value", deployments[0].Grant.Limits.MaxSessions)
	}
}

// TestProcessResolvesTheWholeDocument covers the sections a process document gained: the tool
// chain, the retry policy and the defaults an agent inherits.
func TestProcessResolvesTheWholeDocument(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	doc := `
version: 1
sandbox:
  - name: local
    impl: local
    settings: { root: "/srv" }
inference:
  providers:
    - name: gpt
      impl: openai
      settings: { base_url: "https://api.openai.com", model: "gpt-5" }
  route:
    attempts_per_target: 2
    max_targets: 3
    base_delay: 250ms
    max_delay: 2s
tools:
  sources:
    - name: workspace
      impl: files
      settings: { root: "/srv" }
  middleware:
    - name: timeout
      impl: timeout
      settings: { seconds: 30 }
skills:
  roots: ["/srv/skills"]
observability: { trace: node, bodies: true }
`
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(plan.Middleware) != 1 || plan.Middleware[0].Impl != "timeout" {
		t.Fatalf("middleware = %+v", plan.Middleware)
	}
	if built.middleware != 0 {
		t.Fatal("loading built the chain; that is host.New's job")
	}
	if plan.Route.AttemptsPerTarget != 2 || plan.Route.MaxTargets != 3 {
		t.Fatalf("route = %+v", plan.Route)
	}
	if plan.Route.BaseDelay.String() != "250ms" || plan.Route.MaxDelay.String() != "2s" {
		t.Fatalf("route delays = %v / %v", plan.Route.BaseDelay, plan.Route.MaxDelay)
	}
	build, err := plan.Middleware[0].New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := build(nil); err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.seenMiddleware.Seconds != 30 {
		t.Fatalf("middleware saw %+v, want the decoded settings", built.seenMiddleware)
	}
}

// TestRouteRefusesAContradiction is the validation that needs both fields: a cap below the
// first delay can only mean the document is wrong.
func TestRouteRefusesAContradiction(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))
	doc := `
version: 1
inference:
  route: { base_delay: 2s, max_delay: 100ms }
`
	_, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

// TestHooksCarryTheirSettings is why hooks are not a name list: forge's seat memory needs a
// directory and a retention count, and a hook without them cannot do anything.
func TestHooksCarryTheirSettings(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := `
version: 1
agents:
  - name: seat_1
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
    hooks:
      - impl: audit
        settings: { dir: /var/memory }
`
	deployments, err := loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("a.yaml", []byte(platformRoots(doc))))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments[0].Hooks) != 1 || deployments[0].Hooks[0].Impl != "audit" {
		t.Fatalf("hooks = %+v", deployments[0].Hooks)
	}
	if _, err := deployments[0].Hooks[0].New(); err != nil {
		t.Fatalf("New: %v", err)
	}
	if built.seenHook.Dir != "/var/memory" {
		t.Fatalf("hook saw %+v, want the decoded settings", built.seenHook)
	}
}

// TestUnknownHookKindIsADocumentError: like a program kind, the hook table is the binary's.
func TestUnknownHookKindIsADocumentError(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc := `
version: 1
agents:
  - name: seat_1
    program: { impl: graph }
    grant: { limits: { idle: 30m } }
    hooks: [{ impl: nobody-registered-this }]
`
	_, err = loader.Deployments(context.Background(), capabilities(plan), sources.Bytes("a.yaml", []byte(platformRoots(doc))))
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "nobody-registered-this") {
		t.Fatalf("err = %v, want a validation error naming the hook kind", err)
	}
}

// TestModelGrantsNameProviderAndModel: "gpt-5" alone is ambiguous once two providers serve one,
// so the reference a grant writes is compared as provider/name — the same string a provider's
// catalog reports.
func TestModelGrantsNameProviderAndModel(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := capabilities(plan)
	caps.Models = []string{"other/gpt-5"}

	_, err = loader.Deployments(context.Background(), caps, sources.Bytes("agent.yaml", []byte(agentDoc)))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if !strings.Contains(err.Error(), "openai/gpt-5") {
		t.Fatalf("error %q does not name the reference it wanted", err)
	}
}

// TestLimitsAreRequired pins the rule that replaced the defaults section: an agent states how
// its sessions expire. Silence would have had to mean something, and every meaning is a guess
// with a bill attached.
func TestLimitsAreRequired(t *testing.T) {
	built := &builds{}
	loader := deploy.NewLoader(impls(built))
	// The document prints unix roots; what it tests is the plan, so the roots are
	// rebased onto the platform's own spelling before they are validated.
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	caps := capabilities(plan)

	for name, doc := range map[string]string{
		"no limits": `
version: 1
agents:
  - name: a
    program: { impl: graph }
`,
		"zero idle": `
version: 1
agents:
  - name: a
    program: { impl: graph }
    grant: { limits: { idle: 0s } }
`,
	} {
		if _, err := loader.Deployments(context.Background(), caps, sources.Bytes("a.yaml", []byte(platformRoots(doc)))); err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("%s: err = %v, want a validation error", name, err)
		}
	}
}

// TestObservabilityIsRequired is the same rule one document up: the process says how much of a
// turn it records, and says both halves. It cannot be per-agent because nothing below the
// assembly honours a per-agent answer — the tool chain and the model access are built once.
func TestObservabilityIsRequired(t *testing.T) {
	loader := deploy.NewLoader(impls(&builds{}))

	for name, doc := range map[string]string{
		"absent": `
version: 1
inference:
  route: { max_targets: 1 }
`,
		"no trace": `
version: 1
observability: { bodies: false }
`,
		"no bodies": `
version: 1
observability: { trace: none }
`,
		"unknown level": `
version: 1
observability: { trace: everything, bodies: false }
`,
	} {
		if _, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(doc))); err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("%s: err = %v, want a validation error", name, err)
		}
	}

	doc := `
version: 1
observability: { trace: node, bodies: true }
`
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(doc)))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if plan.Observability.Trace != telemetry.TraceNode || !plan.Observability.Bodies {
		t.Fatalf("observability = %+v, want the document's", plan.Observability)
	}
}

// TestAMiddlewareLinkBelowTheTraceLevelIsNotPlanned is the assembly half of the cost model: a
// link whose registration says it is only worth installing at "call" does not enter the chain of
// a process that records "none". Dropping it here is what keeps the tool layer from reading the
// level — it never learns the level exists.
func TestAMiddlewareLinkBelowTheTraceLevelIsNotPlanned(t *testing.T) {
	for level, want := range map[string][]string{
		"none": {"timeout"},
		"call": {"timeout", "telemetry"},
		"node": {"timeout", "telemetry"},
	} {
		loader := deploy.NewLoader(impls(&builds{}))
		doc := `
version: 1
observability: { trace: ` + level + `, bodies: false }
tools:
  middleware:
    - { name: timeout, impl: timeout, settings: { seconds: 30 } }
    - { name: telemetry, impl: telemetry }
`
		plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(doc)))
		if err != nil {
			t.Fatalf("%s: Process: %v", level, err)
		}
		got := make([]string, 0, len(plan.Middleware))
		for _, link := range plan.Middleware {
			got = append(got, link.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: chain = %v, want %v", level, got, want)
		}
	}
}
