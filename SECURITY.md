# Security Policy

## Supported Versions

FlowCraft is a multi-module monorepo and is currently in pre-1.0 development.
Security fixes are issued against the latest release of each module.

| Module     | Supported tag stream |
| ---------- | -------------------- |
| `sdk`      | latest `sdk/v0.x`    |
| `memory`   | latest `memory/v0.x` |
| `sdkx`     | latest `sdkx/v0.x`   |
| `voice`    | latest `voice/v0.x`  |

Older minor versions are not patched; please upgrade before reporting issues
that only reproduce on outdated tags.

## Reporting a Vulnerability

**Please do not file public GitHub issues for security problems.**

Use one of the following private channels:

1. **GitHub Security Advisories** — preferred. Open a draft advisory at
   <https://github.com/GizClaw/flowcraft/security/advisories/new>. This keeps
   the report private and lets us coordinate a fix and CVE assignment with you.
2. **Email** — `security@gizclaw.dev` (PGP key on request).

Please include:

- Affected module(s) and version/tag (e.g. `sdk/v0.4.0`).
- A minimal reproduction (config, command, request, or code snippet).
- Impact assessment (what a malicious actor could do).
- Any suggested mitigation, if you have one.

## Response Process

- We acknowledge new reports within **3 business days**.
- We aim to provide a triage assessment (severity, affected versions, ETA)
  within **7 business days**.
- For confirmed high-severity issues we coordinate a fix, a CVE (when
  applicable), and a coordinated release window with the reporter. Reporters
  are credited in the advisory unless they request otherwise.

## Scope

In scope:

- Code in this repository (`sdk/`, `memory/`, `sdkx/`, `voice/`,
  `examples/`, `eval/`).
- Default configurations shipped with the SDK, adapters, memory substrates,
  voice pipeline, and example deployments.

Out of scope:

- Third-party LLM, STT, or TTS providers — please report those upstream.
- Vulnerabilities that require an attacker to already control the host
  running the application or the operator's developer machine.
- Best-practice hardening suggestions without a concrete attack — those are
  welcome as regular GitHub issues or pull requests.
