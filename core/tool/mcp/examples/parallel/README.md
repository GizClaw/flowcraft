# Anonymous web search and fetch

This runnable example connects FlowCraft's existing HTTP MCP source to
[Parallel Search MCP](https://docs.parallel.ai/integrations/mcp/search-mcp).
It discovers the server's tools, publishes them into `tool.Registry`, and
calls `parallel__web_search` and optionally `parallel__web_fetch` through
`tool.Executor`. It needs neither a Parallel API key nor a model provider:
no LLM inference is involved.

From the repository root:

```sh
cd core
go run ./tool/mcp/examples/parallel -query "FlowCraft Go MCP tool bridge"
go run ./tool/mcp/examples/parallel -query "Go language documentation" -fetch https://go.dev/doc/
```

Text results are printed as the server's JSON, including source URLs and
excerpts. Empty or non-text-only results produce an error rather than blank
successful output; this CLI does not render images or structured-only content.
The optional fetch reads the URL you supply; it does not automatically
select a search result. Search and fetch in one invocation share a randomly
generated session identifier. Ctrl-C cancels the operation, and `-timeout 90s`
bounds discovery and all calls together. Connection failures and MCP tool errors
produce a nonzero exit code.

`servers.json` is the embedded settings subtree for a `tool.Source/mcp`
resource. It uses the anonymous `https://search.parallel.ai/mcp` endpoint and
sets `User-Agent: FlowCraft/parallel-mcp-example` on every request. The example
loads this spec with `mcp.NewFactory`; it does not read environment API keys or
saved credentials. To use it in an application, copy the settings into your
MCP source resource and expose the discovered tools in your existing registry.
Set a truthful deployment-specific `User-Agent` when copying it. `required: true`
is a host policy flag: core records it, and the host must call `Source.WaitReady`
to enforce startup readiness. Drop it unless your host must refuse to start
without Parallel. This CLI always waits for discovery regardless of that flag.
The spec's `http_timeout: 60s` bounds individual HTTP client requests when using
the default client; `-timeout` bounds the whole invocation. Direct factory loading
does not expand `${env:...}` placeholders; the deployment loader does.

For applications that feed results to a model, set the middleware's
`result_limit.max` text budget and `result_limit.part_budget_bytes` media budget
for your workload (see the [tool guide](https://github.com/GizClaw/flowcraft/blob/main/docs/guides/tool.md#middleware-chain)).
Apply an exposure policy's `MaxDefinitions` and `MaxBytes` when injecting
third-party tool definitions into prompts; `Registry.Definitions()` is uncapped.
This CLI prints the server's text without imposing a model-context budget.
This example is opt-in and does not change other deployments or providers.

The [Parallel documentation](https://docs.parallel.ai/integrations/mcp/search-mcp)
currently describes anonymous access as free at lower rate limits with
server-managed `fast` search settings, and supports `session_id` on both tools.
It is intended for exploration and light use; free access is
not unlimited. The service may return rate-limit or extraction errors. Model
inference, if you add an LLM to your application, has separate costs.

The fixture tests run offline through the factory, registry and executor,
checking ordered JSON results, request identity, empty/non-text results, search
and fetch errors, discovery deadlines and cancellation. The live check
explicitly opts into network calls
and consumes anonymous limits:

```sh
go test ./tool/mcp/examples/parallel
FLOWCRAFT_PARALLEL_LIVE=1 go test ./tool/mcp/examples/parallel -run TestLiveParallel -v -count=1
```
