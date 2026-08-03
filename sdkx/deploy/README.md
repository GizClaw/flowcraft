# Deploy

`sdkx/deploy` assembles shared runtime resources and named agents from a
strict YAML document. It is an assembly layer, not a session runtime:

- resources are built in dependency order;
- engine factories build `agent.Engine` values;
- resources and engines are bound to named agent instances;
- `Result` owns resources created by the document;
- the application supplies the execution-time `agent.Host`.

Deploy does not own request loops, sessions, conversation persistence,
interrupt delivery, or application startup.

## End-to-end flow

An application using deploy performs four steps:

1. Register engine, resource, hook, and source factories.
2. Parse and build a deployment document.
3. Retrieve exported application resources, such as `event.Bus`.
4. Execute a named instance with an application-owned `agent.Host`.

### Registration and build

Registration is explicit. `sdkx/deploy` does not import module-specific
configuration packages and has no global registry.

```go
engines := agent.NewRegistry()
engines.MustRegister(graphagent.NewFactory(
	graphagent.WithBaseDir(configDir),
))

toolRegistry := tool.NewRegistry()
toolBuilder := toolconfig.NewBuilder(toolRegistry, toolconfig.Deps{
	Approver:  approver,  // application-owned; may be nil
	AuditSink: auditSink, // application-owned; may be nil
})

builder := deploy.NewBuilder(engines)
builder.MustRegisterResource(workspaceconfig.NewDeployFactory())
builder.MustRegisterResource(sandboxconfig.NewDeployFactory())
builder.MustRegisterResource(inferenceyaml.NewDeployFactory(
	providerFactories,
	secretResolvers,
))
builder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))
builder.MustRegisterResource(jsrt.NewDeployFactory())
builder.MustRegisterResource(luart.NewDeployFactory())
builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())

data, err := os.ReadFile("deploy.yaml")
if err != nil {
	return err
}
document, err := deploy.Parse(data)
if err != nil {
	return err
}
result, err := builder.Build(ctx, document)
if err != nil {
	return err
}
defer result.Close()
```

`providerFactories`, `secretResolvers`, tool registrations, approvers,
and audit sinks are application-owned Go values. YAML selects configured
capabilities; it does not instantiate credentials, callbacks, or provider
drivers.

## Complete deployment document

The following document demonstrates every deploy-owned top-level agent
field and all currently supported first-party resource categories.

```yaml
version: v1

resources:
  workspaces:
    kind: workspace.Registry
    impl: yaml
    settings:
      file: ./workspaces.yaml
      base_dir: .

  sandboxes:
    kind: sandbox.Registry
    impl: yaml
    deps:
      workspaces: workspaces
    settings:
      file: ./sandboxes.yaml

  inference:
    kind: inference.Assembly
    impl: yaml
    settings:
      file: ./inference.yaml

  tools:
    kind: tool.Assembly
    impl: yaml
    settings:
      file: ./tools.yaml

  javascript:
    kind: agent.ScriptRuntime
    impl: js
    settings:
      pool_size: 8
      max_call_stack_size: 1024
      max_exec_time: 30s

  events:
    kind: event.Bus
    impl: memory
    export: true
    settings:
      route_cache_size: 1024

agents:
  researcher:
    card:
      name: Researcher
      description: Performs research workflows

    # Agent-level tool allow-list.
    tools:
      - search
      - fetch

    engine:
      kind: graph
      settings:
        graph:
          file: ./graphs/research.json
        script_runtime_name: js
        build:
          max_iterations: 100
          timeout: 5m
          max_node_retries: 2
          parallel:
            enabled: true
            branch_timeout: 1m
            max_concurrency: 8
            max_branches: 32
            merge_strategy: last_write_wins

    deps:
      inference: inference
      tools: tools
      workspace: workspaces/project
      sandbox: sandboxes/coding
      script_runtime: javascript

    # prepare and observe kinds are application-registered.
    prepare:
      - type: recall
        deps:
          store:
            source: host.memory
            ref: default
        settings:
          max_hits: 8

    observe:
      - type: transcript
        deps:
          store:
            source: host.memory
            ref: default
        settings:
          channel: main

    referees:
      # The only referee built into sdkx/deploy.
      - type: discard_on_interrupt
        settings:
          reason: barge-in
          causes:
            - user_input
            - user_cancel

    policy:
      max_revise: 2
      artifact_channels:
        - drafts
        - artifacts
```

The example requires the application to register the `recall`,
`transcript`, and `host.memory` extensions. Remove those sections when
the application does not provide them.

## Resource fields

Every resource has this deploy-owned shape:

```yaml
resources:
  resource_name:
    kind: resource.Category
    impl: implementation
    export: false
    deps: {}
    settings: {}
```

- `kind` and `impl` select a registered `ResourceFactory`.
- `deps` binds names declared by the factory's `ResourceSpec`.
- `settings` is decoded strictly by that factory.
- `export: true` allows an otherwise unused resource to be retrieved by
  the application.

An unexported resource must be consumed by another resource, an agent
engine, or a lifecycle hook. Otherwise build fails as dead configuration.

### First-party resource implementations

| Kind | Impl | Settings | Result |
| --- | --- | --- | --- |
| `workspace.Registry` | `yaml` | `file` or `inline`, optional `base_dir` | Workspace container |
| `sandbox.Registry` | `yaml` | `file` or `inline` | Sandbox container |
| `inference.Assembly` | `yaml` | `file` or `inline` | Runtime and optional Router |
| `tool.Assembly` | `yaml` | `file` or `inline` | Tool Catalog and Executor |
| `agent.ScriptRuntime` | `js` | `pool_size`, `max_call_stack_size`, `max_exec_time` | JavaScript runtime |
| `agent.ScriptRuntime` | `lua` | `pool_size`, `max_exec_time` | Lua runtime |
| `event.Bus` | `memory` | `route_cache_size` | In-process event bus |

JavaScript and Lua use the same resource kind but different
implementations. A graph agent currently binds one `script_runtime`.
Different agents may bind different runtimes.

## Resource sub-documents

Workspace, sandbox, inference, and tool resources wrap their modules'
own versioned YAML formats. Their settings accept exactly one source.

File form:

```yaml
settings:
  file: ./workspaces.yaml
  base_dir: .
```

Inline form:

```yaml
settings:
  base_dir: .
  inline:
    version: v1
    workspaces:
      project:
        driver: local
        settings:
          root: ./workspace
```

`base_dir` belongs only to the workspace resource. The nested document
is parsed by the owning module and retains that module's strictness and
versioning rules.

## Dependency references

Dependencies can bind a whole resource, one item from a container, or a
host-owned source.

### Whole resource

```yaml
deps:
  inference: inference
```

The referenced resource kind must match the dependency type declared by
the consumer.

### Container item

```yaml
deps:
  workspace: workspaces/project
  sandbox: sandboxes/coding
```

The resource must declare an item type and implement
`deploy.ItemResolver`. Workspace and sandbox registries are containers;
inference and tool assemblies are not.

### Host-owned source

```yaml
deps:
  store:
    source: host.memory
    ref: default
```

A source is a value resolved by application code:

```go
builder.RegisterSource("host.memory", func(
	ctx context.Context,
	ref string,
) (any, error) {
	store, ok := memoryStores[ref]
	if !ok {
		return nil, errdefs.NotFoundf("memory store %q is not registered", ref)
	}
	return store, nil
})
```

`source` is the registered resolver name. `ref` is an opaque selector
passed to that resolver. Source values are borrowed and are never
closed by `Result.Close`.

Use a source for process-wide clients, shared stores, callbacks, or
other objects whose lifecycle belongs to the application. Use a
resource when the document should construct and own the value.

## Graph engine settings

Graph definitions may use a scalar file path, an explicit file object,
or an inline definition.

```yaml
# Scalar compatibility form.
graph: ./graphs/research.json
```

```yaml
graph:
  file: ./graphs/research.json
```

```yaml
graph:
  inline:
    name: simple
    entry: start
    nodes:
      - id: start
        type: script
        config:
          runtime: js
          source: |
            board.setVar("done", true);
            signal.done();
    edges: []
```

Exactly one of `file` and `inline` is required. File definitions are
limited to 1 MiB. Unknown settings and graph definition fields are
rejected.

The complete graph build settings are:

```yaml
engine:
  kind: graph
  settings:
    graph:
      file: ./graph.json
    script_runtime_name: js
    build:
      max_iterations: 100
      timeout: 5m
      max_node_retries: 2
      parallel:
        enabled: true
        branch_timeout: 1m
        max_concurrency: 8
        max_branches: 32
        merge_strategy: last_write_wins
```

Built-in merge strategies are `first_write_wins` and
`last_write_wins`.

The graph factory declares these dependency names:

| Name | Type | Requirement |
| --- | --- | --- |
| `inference` | `inference.Assembly` | Required when the graph contains an inference node |
| `tools` | `tool.Assembly` | Required for tool nodes or inference tools |
| `workspace` | `workspace.Workspace` | Optional script workspace capability |
| `sandbox` | `sandbox.Runner` | Optional script command-execution capability |
| `script_runtime` | `agent.ScriptRuntime` | Required when the graph contains a script node |

Inference nodes without an explicit model require an inference
Assembly containing a Router. Static inference tool names are validated
at build time. Board references remain dynamic and are resolved during
execution.

## Exporting EventBus and executing an instance

`event.Bus` is not a graph build dependency. It is an execution-time
Host capability, so the resource is exported to the application:

```yaml
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
```

The application retrieves the bus and exposes the same event surface
for publishing and subscribing:

```go
type runtimeHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h runtimeHost) Publish(
	ctx context.Context,
	envelope event.Envelope,
) error {
	return h.bus.Publish(ctx, envelope)
}

func (h runtimeHost) EventBus() event.Bus {
	return h.bus
}

bus, err := deploy.ResourceAs[event.Bus](result, "events")
if err != nil {
	return err
}
instance, ok := result.Instance("researcher")
if !ok {
	return fmt.Errorf("researcher instance is not built")
}

host := runtimeHost{bus: bus}
response, err := instance.Execute(
	ctx,
	agent.Request{Message: userMessage},
	agent.WithHost(host),
)
```

The Host owns execution behavior; `Result` owns the bus lifecycle.
Consumers may borrow the EventBus but must not close it directly.

## Lifecycle hooks

`prepare`, `observe`, and `referees` share the same document shape:

```yaml
- type: registered_factory_name
  deps: {}
  settings: {}
```

- `prepare` builds the initial board in declaration order.
- `observe` receives read-only lifecycle notifications.
- `referees` returns decisions merged by the execution harness.
- `policy` is data consumed by the harness and is not a hook.

Only `discard_on_interrupt` is built in. Every other hook type must be
registered on the Builder.

## Ownership and shutdown

`Builder.Build` constructs resources in topological dependency order.
On failure, already-built resources are closed before the error is
returned.

`Result.Close`:

- is idempotent;
- closes document-owned `io.Closer` resources;
- closes resources in reverse construction order;
- joins all close errors;
- never closes values returned by a Source.

Do not close resources returned by `ResourceAs` separately. Stop
request/session loops, then close the deployment Result during
application shutdown.

## Validation rules

Parsing and build reject:

- unsupported document versions;
- unknown deploy fields;
- missing resource `kind` or `impl`;
- unknown factories, sources, engines, or hooks;
- unknown or missing declared dependencies;
- dependency type mismatches;
- item references on non-container resources;
- resource dependency cycles;
- unused resources without `export: true`;
- unknown resource or engine settings;
- typed-nil resources and dependencies;
- invalid graph settings and unsupported graph node dependencies.

