---
name: hello
description: Minimal plugin-provided skill. Replace with real guidance.
---

# Hello

A plugin contributed this skill directory under `skills:provide`. The
host exposes it through the plugin host's `SkillRoots`; an application
capability reads those roots at build time and turns them into skills.

Add a `run` section and a plugin process speaking the plugin wire to
contribute tools (declared over the connection, gated by `tools:provide`),
and declare `nodes` plus `nodes:provide` (and `tools:provide` for the tool
the node calls) to contribute graph nodes.
