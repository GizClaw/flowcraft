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

Results are printed as the server's JSON text, including source URLs and
excerpts. The optional fetch reads the URL you supply; it does not automatically
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
This example is opt-in and does not change other deployments or providers.

Anonymous access is free at lower rate limits and uses server-managed `fast`
search settings. It is intended for exploration and light use; free access is
not unlimited. The service may return rate-limit or extraction errors. Model
inference, if you add an LLM to your application, has separate costs.

The fixture tests run offline. The live check explicitly opts into network calls
and consumes anonymous limits:

```sh
go test ./tool/mcp/examples/parallel
FLOWCRAFT_PARALLEL_LIVE=1 go test ./tool/mcp/examples/parallel -run TestLiveParallel -v -count=1
```
