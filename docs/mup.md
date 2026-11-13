# Minimum Usable Product (MUP)

Mosso is being built for daily use. Optimize for completing an entire coding
task rather than building generic abstractions. Vercel Sandbox and Pi-specific
implementations are acceptable for this first version.

## User journey

1. Launch Mosso locally.
2. Select a GitHub repository and give Pi a task.
3. Mosso provisions a Vercel Sandbox and starts a Mosso worker and Pi.
4. Watch events stream into a web chat and send follow-up steering instructions.
5. Pi changes code, runs tests, pushes a branch, and opens a pull request.
6. Refresh the browser and see the ongoing session and history.
7. End the session and clean up the sandbox.

## P0 acceptance criteria

- [x] Provision and destroy a Vercel Sandbox; prevent abandoned chargeable resources.
- [ ] Securely provide runtime GitHub and model credentials without exposing them
      in prompts, URLs, process listings, or logs.
- [ ] Clone a selected repository in the sandbox.
- [ ] Start Pi and send an instruction via its RPC mode.
- [ ] Stream agent responses, status and tool events to Mosso.
- [ ] Support follow-up messages and steering while Pi is active.
- [ ] Commit, push and open a PR; show the PR URL.
- [ ] Persist session metadata and chat events across UI reloads.
- [ ] Show sessions and offer a terminate action.
- [ ] Run Go CI and PR secret scanning.

## P1 / defer

- Worker offline event replay and server reconnection.
- Restarting stopped environments with Pi conversation history.
- Cost dashboards and additional VM providers or agent harnesses.
- Multi-user auth, native desktop client, distributed architecture.

## First vertical slice

Go CLI → Vercel Sandbox → Pi → GitHub PR. Add the interactive RPC relay and
web interface once the end-to-end path works.

## Current skeleton

`cmd/mosso`: localhost-only HTTP server with `GET /healthz` and Vercel Sandbox
endpoints (see [vercel-sandbox.md](vercel-sandbox.md)).
`cmd/mosso-worker`: placeholder binary, **not yet connected to Pi**.
`internal/server`: HTTP API and tests.
`internal/vercel`: minimal Vercel Sandbox REST client (create, exec, destroy).
`internal/claude`: runs Claude Code headlessly in a sandbox with a
subscription token from the macOS Keychain, and resumes conversations by
session ID (see [claude-code.md](claude-code.md)).

No persistence, streaming chat, repository cloning or PR creation exists yet.
