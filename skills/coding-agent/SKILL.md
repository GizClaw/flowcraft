---
name: coding-agent
description: >-
  Best practices for writing, reviewing, and debugging code. Use when: user asks
  to write code, fix bugs, refactor, or review code quality. NOT for:
  non-programming tasks. No external dependencies needed.
tags:
  - coding
  - development
  - review
  - debug
---
# Coding Agent Skill

Guidelines and techniques for writing, reviewing, and debugging code.

## When to Use

- "Write a function that ..."
- "Fix this bug"
- "Refactor this code"
- "Review this code"
- "Why is this failing?"
- "Optimize this query"

## When NOT to Use

- Non-programming tasks (document editing, spreadsheets)
- Infrastructure provisioning (use IaC tools directly)
- Database administration (use DB-specific tools)

## Code Writing Guidelines

### Before Writing

1. **Clarify requirements** — ask if anything is ambiguous
2. **Identify constraints** — language, framework, performance
3. **Plan the approach** — outline before implementing

### During Writing

1. **Start simple** — get it working, then optimize
2. **Use meaningful names** — variables, functions, types
3. **Handle errors** — don't ignore error returns
4. **Write tests** — cover happy path and edge cases

### After Writing

1. **Review your own code** — read it fresh
2. **Run linters** — catch style issues early
3. **Test edge cases** — empty inputs, large inputs, concurrency

## Code Review Checklist

- [ ] Does it solve the stated problem?
- [ ] Are edge cases handled?
- [ ] Are errors handled gracefully?
- [ ] Is the code readable and maintainable?
- [ ] Are there any security concerns?
- [ ] Are tests included?
- [ ] Does it follow project conventions?

## Debugging Approach

1. **Reproduce** — create a minimal failing case
2. **Isolate** — narrow down to the smallest failing unit
3. **Hypothesize** — form a theory about the cause
4. **Verify** — test the hypothesis
5. **Fix** — apply the minimal change
6. **Validate** — ensure the fix works and nothing else broke

## Refactoring Principles

- **Single Responsibility** — one function, one purpose
- **DRY** — extract repeated logic
- **Naming** — rename for clarity over brevity
- **Simplify** — remove dead code and unnecessary complexity
- **Small steps** — refactor incrementally with tests passing

## Language-Specific Tips

### Go

- Use `gofmt`/`goimports`
- Prefer returning errors over panicking
- Use context for cancellation

### TypeScript

- Enable strict mode
- Prefer `const` over `let`
- Use typed returns, avoid `any`

### Python

- Use type hints
- Follow PEP 8
- Use virtual environments

