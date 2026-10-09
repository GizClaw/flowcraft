---
name: hello
description: What the hello plugin contributes to anvil; handed to agents by the host.
---

# Hello

This skill travels with the `hello` plugin. The manifest declares
`skills:provide` and lists `skills`, so the plugin host exposes every
listed directory through `PluginHost.SkillRoots()`. An application with
agents turns each root into a skill source; `anvil` has no agent, so it
only prints the roots.

The same plugin runs an MCP server (`hello__greet`) and calls the host
primitives through the endpoint the host injects (`hello__ping_host`).
