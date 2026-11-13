# Agent Guidelines

Mosso is an experimental, self-hosted Go coding-agent orchestrator. The goal is a **Minimum Usable Product (MUP)** that the maintainer can use daily, not a generic platform.

## Priorities

- Ship the smallest working end-to-end workflow and learn from real use.
- Keep the implementation simple, idiomatic, and easy to change.
- Vercel Sandbox is the first compute backend; Pi is the first agent harness. Direct integration is fine for the MUP. **Do not** introduce provider/harness abstractions or plugin systems until needed.
- One Go monorepo, not microservices. Prefer minimal dependencies.
- Build incrementally; include focused tests and update documentation when behavior changes.

## Repository structure

- `cmd/mosso/` — main server entry point.
- `cmd/mosso-worker/` — worker running alongside Pi in the sandbox; only modify when the task requires it.
- `internal/server/` — HTTP API and server-side coordination; `internal/server/web/` is the embedded browser UI (plain HTML/CSS/JS, no build step).
- `internal/vercel/` — Vercel Sandbox integration.
- `internal/claude/` — Claude Code CLI integration (headless runs in a sandbox, subscription token handling).
- `internal/github/` — GitHub integration (clone into a sandbox, publish changes as a pull request via the REST API, token handling).
- `internal/keychain/` — macOS Keychain storage for Mosso's tokens.
- `docs/` — architecture notes, MUP scope, and setup instructions.

Follow existing conventions and avoid creating unrelated packages or reorganizing the repository without a concrete need.

## MUP user journey

Provision a sandbox → clone a GitHub repository → run Pi → stream an interactive chat and steering commands → commit, push and create a PR → reconnect to the session → terminate the sandbox.

Implement only the requested slice of that journey.

## Security: public repository

- Never commit credentials, tokens, private keys, populated `.env` files, or sensitive runtime data.
- Read secrets from environment variables or another runtime-only mechanism. Never embed them in prompts, command arguments, URLs, git remotes, logs, or error responses.
- Scope GitHub credentials to the minimum necessary permissions and repositories.
- Bind the unauthenticated server to loopback; do not expose it on the internet without authentication.
- Ensure sandbox cleanup on failures and termination to avoid runaway charges.
- Keep CI checks and Gitleaks secret scanning intact. AI review does **not** replace secret scanning.
- If any real secret is exposed, report it and recommend immediate revocation or rotation; do not merely delete it from a later commit.

## Working agreement

- Use a feature branch and open a PR; do not commit directly to `main` unless explicitly asked.
- Run `go test ./...` and `go vet ./...` when applicable; report what was and was not verified.
- Avoid speculative features, unnecessary interfaces, and premature refactoring.
- Explain any new dependencies or operational requirements briefly in the PR.
