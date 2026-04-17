---
name: github
description: >-
  Interact with GitHub via the gh CLI. Use when: user asks to create issues,
  pull requests, check CI status, browse repositories, or manage releases. NOT
  for: GitLab, Bitbucket, or other providers. Requires gh CLI installed and
  authenticated.
homepage: 'https://cli.github.com/manual/'
metadata:
  openclaw:
    emoji: "\U0001F419"
    requires:
      bins:
        - gh
---
# GitHub Skill

Interact with GitHub repositories, issues, pull requests, and more using the `gh` CLI.

## When to Use

- "Create a new issue"
- "Open a pull request"
- "Check CI status"
- "List open PRs"
- "Browse repo releases"
- "View PR review comments"

## When NOT to Use

- GitLab or Bitbucket operations
- Git operations (use `git` directly)
- GitHub Actions workflow authoring (edit YAML files directly)

## Prerequisites

```bash
# Install gh CLI
brew install gh      # macOS
sudo apt install gh  # Debian/Ubuntu

# Authenticate
gh auth login
```

## Commands

### Issues

```bash
# List open issues
gh issue list

# Create an issue
gh issue create --title "Bug: ..." --body "Description"

# View an issue
gh issue view 42

# Close an issue
gh issue close 42
```

### Pull Requests

```bash
# List open PRs
gh pr list

# Create a PR
gh pr create --title "feat: add feature" --body "## Summary\n..."

# View a PR
gh pr view 123

# Check PR status / CI
gh pr checks 123

# Review PR comments
gh api repos/OWNER/REPO/pulls/123/comments

# Merge a PR
gh pr merge 123 --squash
```

### Repositories

```bash
# View repo info
gh repo view

# Clone a repo
gh repo clone OWNER/REPO

# List releases
gh release list

# Create a release
gh release create v1.0.0 --title "v1.0.0" --notes "Release notes"
```

### Workflow Runs

```bash
# List recent workflow runs
gh run list

# View a specific run
gh run view RUN_ID

# Watch a run in progress
gh run watch RUN_ID
```

## Notes

- Requires `gh` CLI installed and authenticated (`gh auth status`)
- Default repo is inferred from current directory's git remote
- Use `--repo OWNER/REPO` to target a different repository

