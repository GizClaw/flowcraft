# Publish an HTML page with deed.page

This runnable example connects FlowCraft's existing HTTP MCP source to the
[deed.page](https://deed.page) MCP endpoint. It discovers the server's tools,
publishes them into `tool.Registry`, and calls `deedpage__publish_site` or
`deedpage__delete_site` through `tool.Executor`. It needs neither an API key,
an account, nor a model provider: no LLM inference is involved.

This standalone example module uses released `core v0.4.9`. Use `GOWORK=off`
to build against that release rather than the repository workspace.

From the repository root:

```sh
cd examples/deedpage
GOWORK=off go run .
GOWORK=off go run . -file report.html -slug my-team-report -ttl 24h
```

The first command publishes a small sample page that expires after one hour
(the default `-ttl 1h`); `-ttl 0` keeps a page with no expiry. Output is the
server's JSON, including the public `url`, the `slug` and, on the first
publish, an owner `token`. The token is shown once: keep it to update or delete
the page later. With `DEEDPAGE_TOKEN` set, a publish sends it so that the page
belongs to the same owner and an existing slug is updated. To delete:

```sh
DEEDPAGE_TOKEN=deed_tok_... GOWORK=off go run . -delete my-team-report
```

Published pages are public to anyone with the URL. Do not publish secrets,
credentials or private data, and do not give this tool to an agent whose
output should stay private. Size limits and the full tool list are in the
[deed.page documentation](https://deed.page/docs).

`servers.json` is the embedded settings subtree for a `tool.Source/mcp`
resource. It uses `https://deed.page/mcp` and sets
`User-Agent: FlowCraft/deedpage-mcp-example` on every request. The example
loads this spec with `mcp.NewFactory`; the only environment value it reads is
`DEEDPAGE_TOKEN`. To use it in an application, copy the settings into your MCP
source resource and expose the discovered tools in your existing registry; the
server also offers tools such as `get_site` and `list_versions`. Set a truthful
deployment-specific `User-Agent` when copying it, and drop `required: true`
unless your host must refuse to start without deed.page. Ctrl-C cancels the
operation, and `-timeout 90s` bounds discovery and the call together. Validation
errors, connection failures, MCP tool errors and `"ok": false` responses
produce a nonzero exit code. This example is opt-in and does not change other
deployments or providers.

The fixture tests run offline through the factory, registry and executor,
checking the publish and delete arguments, request identity, validation before
connecting, tool errors and cancellation. The live test explicitly opts into
network calls; it publishes a page for ten minutes, fetches it and deletes it.

From `examples/deedpage`:

```sh
GOWORK=off go test ./...
FLOWCRAFT_DEEDPAGE_LIVE=1 GOWORK=off go test ./... -run TestLiveDeedPage -v -count=1
```
