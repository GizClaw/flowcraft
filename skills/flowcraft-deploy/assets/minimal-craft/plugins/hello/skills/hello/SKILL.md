---
name: hello
description: Minimal plugin-provided skill. Replace with real guidance.
---

# Hello

A plugin contributed this skill directory under `skills:provide`. The
host exposes it through the plugin host's `SkillRoots`; an application
capability reads those roots at build time and turns them into skills.

Add an MCP server to the plugin to contribute tools, and declare
`nodes` plus `nodes:provide` (and `mcp:provide` for the tool the node
calls) to contribute graph nodes.
