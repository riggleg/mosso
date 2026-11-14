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
      in prompts, URLs, process listings, or logs. Partly done: neither token
      is in prompts, URLs, arguments or logs, and the GitHub token stays on the
      server except during a private clone. The Claude token remains readable
      by code in the sandbox while Claude runs; see
      [github.md](github.md#security-model-and-limitations) and
      [claude-code.md](claude-code.md#security-model-and-limitations).
- [x] Clone a selected repository in the sandbox.
- [ ] Start Pi and send an instruction via its RPC mode.
- [ ] Stream agent responses, status and tool events to Mosso. Done for
      Claude Code: events stream to the web UI while Claude runs (see
      [web-ui.md](web-ui.md)); Pi is not integrated yet.
- [ ] Support follow-up messages and steering while Pi is active. With Claude
      Code: follow-ups continue the conversation and a running turn can be
      stopped; steering a turn while it runs is not supported.
- [x] Commit, push and open a PR; show the PR URL (with Claude Code for now).
- [ ] Persist session metadata and chat events across UI reloads. Partly
      done: the server keeps them in memory, so they survive reloads and
      reconnects but not a Mosso restart or the end of the sandbox.
- [x] Show sessions and offer a terminate action (web UI).
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
`internal/github`: clones a GitHub repository into a sandbox on a `mosso/*`
branch, keeps the sandbox's `origin/*` current and pushes its commits from
the Mosso server (Git bundles, `--force-with-lease`), and opens pull
requests through the GitHub API (see [github.md](github.md)).
`internal/keychain`: macOS Keychain storage shared by the Claude and GitHub
tokens.

`internal/server/web`: the browser UI (plain HTML/CSS/JS embedded in the
binary), with streaming chat over server-sent events
(see [web-ui.md](web-ui.md)).

There is no persistence yet: workspaces and chats live in memory.
