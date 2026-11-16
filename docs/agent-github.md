# Agent-controlled GitHub operations

Claude can push its work and open, read and update pull requests itself,
without you clicking **Create PR**. A workspace's instructions can say, for
example:

> Implement the requested changes, run the relevant tests, and create pull
> requests targeting both `master` and `uat`.

and Claude does all of it in its turn. **Mosso provides the tools and keeps
the credentials; Claude decides how to use them.** Nothing in Mosso knows
about "two pull requests" or about `uat`: that is in the instructions, and
Claude works out the steps (including adapting the change where `uat`
differs from `master`).

The manual **Create PR** / **Update PR** button works as before.

## The tools

Claude Code sees them as the `mosso` MCP server (`mcp__mosso__github_*`).
Every one acts on the workspace's own repository; none takes a repository
argument.

| Tool | What it does |
| --- | --- |
| `github_push` | Pushes `HEAD` (default) or a local branch to one of the workspace's own branches on GitHub, creating it if needed. Fast-forward only. Only committed work. |
| `github_create_pr` | Opens a pull request from one of the workspace's branches (already pushed) into a base branch (default: the workspace's base). If one is already open from that head into that base, returns it (`"created": false`). |
| `github_list_prs` | Lists the pull requests from the workspace's branches (`state`: `open`, `closed`, `all`). |
| `github_get_pr` | Reads one of the workspace's pull requests: title, description, state, draft, merged, mergeable, head and base. |
| `github_update_pr` | Changes the title, description or draft status of one of the workspace's open pull requests. |
| `github_checks` | Reads the CI results (check runs and commit statuses) of one of the workspace's pull requests, or of a branch, with a summary: `success`, `failure`, `pending` or `none`. |
| `github_fetch` | Fetches a branch (for example `uat`) into the sandbox as `origin/<branch>`, and keeps it current before every turn. `git fetch` cannot do this in the sandbox for a private repository. Other workspaces' `mosso/*` branches are refused. |

Errors are written for Claude to act on: each says what was refused or
failed and what to do next (push first, rebase onto `origin/<branch>`, push
to a new branch, ask the user). Arguments are decoded strictly: an unknown
field (such as `repo`) is an error, not ignored.

### Branches

The workspace's **own branches** are its working branch `mosso/<12 hex>`
and branches named after it: `mosso/<12 hex>-<suffix>`, where the suffix is
letters, digits, `.`, `_` and `-` (up to 50), for example
`mosso/0a1b2c3d4e5f-uat`. They are the only branches Mosso pushes for the
agent. The working branch name is random, so one workspace's branches never
match another's.

For several pull requests, Claude uses one head branch per base branch:

1. Commit on the working branch, `github_push`, `github_create_pr` (into the
   workspace's base, say `master`).
2. `github_fetch` `uat`; `git switch -c fix-uat origin/uat`; cherry-pick or
   adapt the change; run the tests there; commit.
3. `github_push` with `branch: mosso/<id>-uat`, `source: fix-uat`;
   `github_create_pr` with `head: mosso/<id>-uat`, `base: uat`.

Mosso never pushes the same commits to a second base by itself: changes
based on `master` may not apply to `uat`, and the agent handles that.

### What the agent cannot do

Not available, by design (no approval system yet):

- **Force-pushing.** `github_push` only fast-forwards. If the branch on
  GitHub has commits the pushed one lacks, the push is refused; Mosso has
  just fetched the branch as `origin/<branch>`, so Claude can rebase onto or
  merge it, or push to a new `-<suffix>` branch. For the working branch, the
  user's **Update PR** can still replace a rebased branch with
  `--force-with-lease` (that click is the approval), as before.
- Pushing to any branch other than the workspace's own: base branches,
  other people's branches, other workspaces' branches. Base branches,
  protected or not, are never updated by Mosso.
- Deleting branches; merging, closing, reopening or retargeting pull
  requests; reviews, comments, issues, releases, workflows.
- Reading or changing pull requests that are not from the workspace's own
  branches in its repository (a fork's branch with the same name does not
  count). `github_fetch` and `github_checks` work on any branch of the
  repository except other workspaces' `mosso/*` branches.
- Any other repository.

The system prompt tells Claude all of this, and to push or open pull
requests only when the user or the workspace instructions ask for it.

## How a call travels

The agent runs in a Vercel Sandbox; Mosso runs on your machine with no
inbound connection. A tool call travels over the channel Mosso already
uses to follow the worker's events, so nothing new is exposed:

```
Claude Code ──stdio──▶ mosso-worker mcp ──HTTP, loopback──▶ worker  ──▶ events.jsonl: tool_call
   (sandbox)            (MCP server)                        (sandbox)        │
                                                                             │ followed by Mosso
Mosso server ◀──────── curl -N …/events, through the Vercel command API ─────┘
   │ POST /tools/calls/{id}/start  (claim; through the Vercel command API)
   │ run it here: Mirror.Push / Fetch, GitHub REST API, with the token
   │ POST /tools/calls/{id}/result (answer; recorded as tool_result)
   ▼
worker ──▶ the waiting MCP request returns ──▶ Claude Code gets the result
```

1. The worker gives Claude Code `--mcp-config` pointing at its own binary
   (`mosso-worker mcp -worker http://127.0.0.1:7777`). That MCP server
   (`internal/worker/mcp.go`, newline-delimited JSON-RPC, no SDK) lists the
   tools and relays each call to the worker's loopback API. It has no
   credentials.
2. `POST /tools/calls` records a `tool_call` event (fsync'd, like every
   event) and holds the request open.
3. Mosso's follower for that workspace sees the event within about a
   second. It claims the call with `POST /tools/calls/{id}/start`, which
   succeeds once: a call that was already claimed, answered or withdrawn
   answers `409`, so a call never runs twice.
4. Mosso runs it on the server (`internal/server/tools.go`) and answers with
   `POST /tools/calls/{id}/result`, recorded as a `tool_result` event. The
   held request returns the result to Claude.

### Lifecycle and failures

- **Mosso not running or disconnected.** A call nobody claims within 5
  minutes is withdrawn: Claude gets "Mosso did not pick up this call …
  nothing was done", and can finish its turn and say what is left.
- **Mosso restarts while a call waits.** The worker lists waiting calls in
  `GET /status` (`pending_calls`); every time Mosso (re)connects to a worker
  it claims them, so a call made while Mosso was stopped runs when it comes
  back (if within the 5 minutes). Replayed `tool_call` events are claimed the
  same way, and refused if already done.
- **Mosso stops in the middle of a call.** The call stays claimed; Claude's
  request ends after 20 minutes with "its outcome is unknown: check with
  github_list_prs". Re-running is safe: pushes of the same commit do
  nothing, and `github_create_pr` returns the open pull request instead of
  opening another.
- **The sandbox session ends** (timeout) during a call. Claude Code ends
  with it. When the worker starts again after a resume, it records a
  `tool_result` *interrupted* for every call left open in its log.
- **Claude is stopped**, or the MCP process dies, before Mosso claims the
  call: the call is withdrawn.
- **Several turns and resumes.** The MCP server is started by Claude Code
  for each turn; the worker, its log and the mirror's tracked branches
  persist with the sandbox, and the extra branches and pull requests are
  saved in Mosso's database.
- **The claim's answer is lost** (a Vercel API error): Mosso answers the call
  with an error saying nothing was done, so Claude can try again instead of
  waiting.
- **Mosso shuts down during a call.** The operation is cancelled; Mosso
  still answers it and saves what it did on GitHub before closing the
  database.
- **Limits.** The worker accepts calls only while Claude works on a message,
  at most 100 per message; results up to 100 KiB (pull request descriptions
  are cut to 16 KiB when read). Mosso runs one call at a time per workspace,
  in the order it claims them. Claude Code's `MCP_TOOL_TIMEOUT` is set above
  the worker's own 20 minutes.
- **An extra branch that can no longer be sent** to the sandbox (for
  example one that grew past the 64 MiB sync limit) does not block syncs,
  Create PR or pushes: Mosso sends the rest and leaves that branch's
  `origin/*` ref as it was. A branch that cannot be sent when first fetched
  is not tracked.

### Workers from an earlier Mosso

The tools need worker protocol 2. A sandbox still running an older worker
(started by an earlier Mosso) gets the new one the next time Mosso connects
while it is idle; its event log is kept. A turn already running on an old
worker has no tools.

## Reusing the publish path

There is one Git implementation, `github.Mirror` (`internal/github/mirror.go`),
now tracking several branches:

- It keeps the sandbox's `origin/*` current for the base branch, the
  working branch, and up to 20 extra branches (fetched with `github_fetch`,
  pushed, or opened a pull request into). They are synced before every
  turn, and restored after a restart.
- `Mirror.Push` (the agent) and `Mirror.Publish` (Create PR) share the same
  steps: fetch, bundle the commits out of the sandbox, `fsck` every object,
  refuse any commit containing the GitHub or Claude token, push from the
  server with `--force-with-lease=<ref>:<expected>`. Push never commits for
  the agent and never rewrites history; Publish commits uncommitted work
  and may replace a rebased working branch, as before.
- Pull requests are opened with `Client.OpenPullRequest`, which returns the
  open pull request for a head and base instead of opening a second one;
  Publish uses it too.

So a pull request the agent opened from the working branch into the base is
the one **Update PR** updates, and a branch the agent pushed is one Mosso
knows it may later replace with `--force-with-lease` on Update PR.

## Security

- **GitHub credentials stay on the Mosso server.** The token never enters
  the sandbox, the worker, the MCP server, tool arguments or results, or
  Claude's environment (unchanged from [github.md](github.md)). Git runs on
  the server with the token in its environment only.
- **The channel is the authentication.** A call reaches Mosso only through
  the Vercel command API of one sandbox session, which Mosso itself opened
  with its Vercel token, and is run for the workspace that owns that
  sandbox. Nothing in a call can name another workspace or repository: the
  repository, working branch and base come from the workspace's saved
  record. Anything in the sandbox (Claude, its tools, repository code) can
  call the worker, so it can use these tools too. The worker only accepts
  calls while Claude works on a message, but code in the sandbox could
  replace the worker and offer Mosso calls at any time: what actually
  limits them is Mosso's checks on the server, described here, not the
  worker. The sandbox is the trust boundary, as for the rest of the
  workspace.
- **Arguments only name branches and pull requests**, and every one is
  checked: branch names against a strict pattern (safe in Git arguments and
  URLs), head branches against the workspace's own, pull requests against
  their head repository and branch. No argument is ever run as a command or
  passed to Git as an option.
- **No new endpoint.** Nothing is published on a port, and Mosso's own
  HTTP API stays on loopback. There is no tunnel and no hosted service.
- **What reaches GitHub is checked as for Create PR**: fsck, the token
  check, the lease, and GitHub's own branch protection and rulesets (a
  rejection is reported). Without the Workflows permission, pushes that
  change `.github/workflows` are still refused by GitHub.
- **Pull requests from the agent run your CI** like any branch in the
  repository (see [github.md](github.md#security-model-and-limitations)).
  Review them before merging, as for any contributor.

### Token permissions

The same fine-grained token as before (Contents and Pull requests: Read
and write). `github_checks` also needs **Checks: Read-only** and **Commit
statuses: Read-only**; the token link in Mosso now preselects them. Without
them, `github_checks` says which permission is missing and the other tools
are unaffected. Changing the draft status uses GitHub's GraphQL API, which
fine-grained tokens with Pull requests: Read and write can do.

## In the web UI

- Claude's calls show in the chat like its other tools (**GitHub · push**,
  **GitHub · create_pr**, …) with their arguments and results.
- Each change on GitHub (a push, a pull request opened or updated) and each
  failed attempt adds a card to the chat, with the pull request's link, or
  the error in red.
- The header lists all the workspace's pull requests, with their base
  branch when there are several (**PR #12 → master ↗ PR #13 → uat ↗**).
- `GET /workspaces/{name}` has `pull_requests`: `[{"number", "url", "head",
  "base", "title", "draft", "by": "agent"|"user"}]`, saved in the database
  with the workspace. `pull_request` is still the one from the working
  branch into the base branch. Chat entries of type `github` carry
  `github`: `{"tool", "text", "error", "branch", "commit", "pull_request"}`.

## Worker API additions

| Method & path | Description |
| --- | --- |
| `POST /tools/calls` | `{"name", "arguments"}` from the MCP server. Records `tool_call` and waits; `200 {"id", "result"}` or `200 {"id", "error"}`; `409` when no task is running; `404` unknown tool; `429` over 100 calls in the task; `504` claimed but unanswered after 20 minutes. |
| `POST /tools/calls/{id}/start` | Mosso claims a waiting call: `200` with `{"id", "name", "arguments"}`, `409` if it is not waiting. |
| `POST /tools/calls/{id}/result` | Mosso answers a claimed call: `{"result": …}` or `{"error": "…"}`; `204`, `409` if not claimed. |
| `GET /status` | Adds `pending_calls` (IDs); `version` is now `"2"`. |

Events: `tool_call` (`call`: `id`, `name`, `arguments`) and `tool_result`
(`call`: `id`, `result` or `error`).

## Tests

- `internal/github` (real Git, fake GitHub over smart HTTP): pushing the
  working branch and a ported `-uat` branch from a local branch based on a
  fetched `uat`, two pull requests into different bases and no duplicate,
  base branches untouched, `uat` kept current before turns and after a
  restore, an extra branch that cannot be sent not blocking syncs, Create PR
  or pushes, refusing other workspaces' branches in fetches, refusing other
  branches, bad sources and force pushes, leaving
  uncommitted work out, refusing credentials in commits, Update PR still
  replacing a rebased branch, pull request details, updates (including
  draft through GraphQL), listing and checks; `OwnBranch`.
- `internal/worker`: the call relay (record, claim once, answer, refuse
  out-of-order answers), withdrawal when unclaimed and when the caller goes
  away, claimed calls not withdrawn, the per-task limit, calls left open
  across a worker restart, a late answer not reopening its task, the MCP
  server (initialize, tools, calls, errors,
  an unreachable worker) and the `--mcp-config` given to Claude Code.
- `internal/server` (fake Vercel with real in-process workers, a scripted
  Claude making real tool calls through them): the master + uat workflow
  end to end, the records, chat entries and saved state, Create PR on the
  agent's pull request afterwards; isolation between two workspaces and
  refusals of other branches, pull requests, forks and repository
  arguments; failures shown and retried, GitHub not connected; a call made
  while Mosso is stopped run once after the restart, with pull requests and
  tracked branches surviving another restart; an old worker replaced when
  idle; a fetch of another workspace's branch refused.
- `node --test internal/server/webtest/*.test.js`: the header's pull
  requests and the chat cards (`github.test.js`).
- To look at it: `MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=github
  go test ./internal/server -run TestServeFakeForBrowser -timeout 1h`.

Not covered by automated tests: a real Claude model choosing and calling the
tools in a real Vercel Sandbox against real GitHub. The author checked the
real worker binary end to end with a stand-in `claude` (worker →
`--mcp-config` → `mosso-worker mcp` → `tool_call` → claim → result), and
that Claude Code 2.1.321 connects to the MCP server (`claude mcp list`:
Connected), but did not run a live turn.

## Remaining limitations

- No approval flow: anything beyond fast-forward pushes to the workspace's
  own branches and pull request creation and edits is unavailable rather
  than approvable.
- A tool call waits for Mosso. With Mosso stopped for more than 5 minutes,
  Claude is told nothing was done; it does not retry later by itself.
- A call whose outcome is unknown (Mosso killed mid-call, without shutting
  down) is left to the agent to check; Mosso does not reconcile it after
  restarting.
- Extra tracked branches are never dropped (at most 20 per workspace).
- Pull requests the agent opened are shown from Mosso's records; their
  state (merged, closed) is read from GitHub only when the agent asks.
- Results are bounded (100 KiB; 100 check runs and statuses; 100 pull
  requests per branch).
