# Mosso

Mosso is an experimental, self-hosted coding-agent orchestrator, written in Go. You run it on your own machine. It provisions a [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox), checks out a GitHub repository in it, runs [Claude Code](https://code.claude.com) there with your Claude subscription, and opens a pull request with the result. You drive it through a local HTTP API.

The long-term goal is a daily-use tool with a web chat, steering, and Pi as the agent harness (see [docs/mup.md](docs/mup.md)). Today only the first slice exists.

## What works today, and what does not

Works:

- Create and destroy Vercel Sandboxes, with a Vercel-side timeout, a limit of 5 at once, and cleanup on shutdown.
- Run Claude Code headlessly in a sandbox, and continue a conversation by session ID.
- Create a **workspace** (a GitHub repository cloned onto a new `mosso/*` branch in a sandbox), send Claude tasks in it, and publish the result as a pull request.

Not implemented yet:

- **There is no web interface.** The server exposes a JSON API only. Use `curl` (examples below). `mosso-worker` is a placeholder.
- **No Pi integration**, no streaming chat, and no steering. Each Claude message is a blocking request that returns when the turn finishes.
- **No persistence.** Workspaces and sandboxes are tracked in memory. Restarting Mosso destroys its sandboxes (branches and pull requests stay on GitHub), and a Claude conversation lives only as long as its sandbox.
- **macOS-first**: `mosso auth` stores tokens in the macOS Keychain. On Linux, use environment variables instead.
- **Single user, local only.** The API has no authentication, so Mosso refuses non-loopback listen addresses and non-loopback `Host`/`Origin` headers.
- The Claude token is readable by code running in the sandbox while Claude runs. Only point Mosso at repositories you trust. See [SECURITY.md](SECURITY.md) and the security sections of [docs/claude-code.md](docs/claude-code.md) and [docs/github.md](docs/github.md).

## Quick Start

### 1. Requirements

- **Go 1.24 or newer** (`go.mod` sets the version). There are no other build dependencies.
- A **Vercel account** with a project to own the sandboxes (the Hobby plan works).
- A **Claude Pro, Max, Team or Enterprise** plan, and the [Claude Code CLI](https://code.claude.com) installed locally. It is only needed once, to generate a token.
- A **GitHub account** and a repository you can write to.
- `curl`, and optionally `jq` to read the responses.
- Optional: macOS, for Keychain storage of the Claude and GitHub tokens. On other systems, tokens come from environment variables.

```bash
git clone https://github.com/riggleg/mosso.git
cd mosso
```

### 2. Configure Vercel

Create or pick a Vercel project and note its project ID (`prj_…`) and your team ID (`team_…`). Create an access token at <https://vercel.com/account/tokens> scoped to that team. Then export all three in the shell that will start Mosso:

```bash
export VERCEL_TOKEN=...
export VERCEL_TEAM_ID=team_...
export VERCEL_PROJECT_ID=prj_...
```

Mosso does not load `.env` files on its own. If you keep these in a local `.env`, source it yourself. Never commit it. If any of the three is missing, Mosso still starts but the sandbox and workspace endpoints return `503`. See [docs/vercel-sandbox.md](docs/vercel-sandbox.md).

### 3. Authenticate Claude and GitHub

On macOS, each command stores a token in your login Keychain. They are read per request, so you can run them while Mosso is already running.

```bash
# Claude: runs `claude setup-token` (browser sign-in), then asks you to paste
# the token at two hidden prompts. Clear your terminal scrollback afterwards.
go run ./cmd/mosso auth claude

# GitHub: prints the steps for creating a fine-grained personal access token,
# then asks you to paste it at two hidden prompts.
go run ./cmd/mosso auth github
```

For GitHub, create a **fine-grained** token limited to the repositories Mosso should work on, with *Contents: Read and write* and *Pull requests: Read and write*. Use a short expiration and do not grant *Workflows*.

On Linux (or if you prefer not to use the Keychain), run `claude setup-token` yourself and export the tokens before starting Mosso:

```bash
export CLAUDE_CODE_OAUTH_TOKEN=...   # from `claude setup-token`
export MOSSO_GITHUB_TOKEN=...        # fine-grained PAT; GITHUB_TOKEN is deliberately ignored
```

Details: [docs/claude-code.md](docs/claude-code.md) and [docs/github.md](docs/github.md).

### 4. Start the server

```bash
make run        # same as: go run ./cmd/mosso
```

Mosso listens on `127.0.0.1:8080`. Change it with `-listen` or `MOSSO_LISTEN`; non-loopback addresses are refused. The startup log shows which credentials were found, for example `Vercel Sandbox credentials loaded`, `Claude Code token found (keychain)` and `GitHub token found (keychain)`. Anything not found is reported as disabled.

In another terminal, check it:

```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/claude/auth    # {"configured": true, ...}
curl -s localhost:8080/github/auth    # {"configured": true, "login": "you", ...}
```

Press Ctrl-C to stop. Mosso destroys the sandboxes it created before exiting.

### 5. Using Mosso (there is no web UI yet)

Opening <http://localhost:8080> in a browser will not show an interface; the server only serves the JSON endpoints listed in the docs. Use `curl` as below.

### 6. Create a workspace, run a task, and publish a pull request

Replace `you/project` with a repository your GitHub token can write to. A disposable test repository is a good first choice.

```bash
# Create a workspace: provisions a sandbox, clones the repo, and creates a
# mosso/<random> working branch. Takes a short while.
WS=$(curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces \
  -d '{"repo":"you/project"}' | tee /dev/stderr | jq -r .name)

# Give Claude a task in the checkout. The request blocks until the turn finishes.
# The first message also installs Claude Code in the sandbox, so it is slower.
curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/claude/messages \
  -d '{"prompt":"Fix the typo in README.md and run the tests."}' | jq .result

# Optional follow-up: pass the session_id from the previous response.
#   -d '{"prompt":"...","session_id":"<session_id>"}'

# Publish: commits the changes, pushes them to the mosso/* branch via the
# GitHub API, and opens (or returns) the pull request.
curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/pull-request \
  -d '{"title":"Fix README typo","draft":true}' | jq .url

# Clean up the sandbox. The branch and pull request stay on GitHub.
curl -s -X DELETE localhost:8080/workspaces/$WS
```

Notes:

- `POST` requests must send `Content-Type: application/json`.
- Mosso never pushes to, force-pushes, or merges into the base branch; it only writes the `mosso/*` branch. Review the pull request before merging.
- Calling `pull-request` again after more work adds a commit to the same branch and returns the same pull request.
- Sandboxes are billed while they run. Workspaces last 30 minutes by default (`timeout_seconds`, maximum 2700), after which Vercel stops them. Delete them when you are done. If Mosso is killed with `SIGKILL`, check the Vercel dashboard for leftover `mosso-*` sandboxes.
- Every Claude turn draws on your own subscription's usage limits.

If something fails, the API returns `{"error": "..."}`. Common causes: `503` (a credential is not configured; check the startup log), `404` (the repository does not exist or the token cannot see it), and `429` (5 sandboxes already exist). The full error list is in [docs/github.md](docs/github.md#api).

## Development

```bash
make check    # go fmt, go vet, go test (uses fakes; no real accounts needed)
make build    # builds bin/mosso and bin/mosso-worker
```

Opt-in end-to-end tests against real accounts are described in [docs/claude-code.md](docs/claude-code.md#acceptance-test) and [docs/github.md](docs/github.md#end-to-end-test-real-accounts-opt-in).

## More documentation

- [docs/vercel-sandbox.md](docs/vercel-sandbox.md): sandbox setup, API, and cost safeguards
- [docs/claude-code.md](docs/claude-code.md): Claude Code authentication, API, and limitations
- [docs/github.md](docs/github.md): workspaces, pull requests, API, and security model
- [docs/mup.md](docs/mup.md): the first milestone and what is still to do
- [SECURITY.md](SECURITY.md): credential handling rules
- [docs/claude-review.md](docs/claude-review.md): automated pull request review by Claude Code
