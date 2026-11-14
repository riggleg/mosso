# Web interface

Mosso has a browser UI for the whole workflow: create a workspace from a
GitHub repository, chat with Claude Code running in its Vercel Sandbox, watch
Claude's tool activity as it happens, publish the result as a pull request,
keep working and update that pull request, and terminate the workspace.

The Go server serves the UI itself. It is plain HTML, CSS and JavaScript in
`internal/server/web`, embedded into the binary with `go:embed`: there is no
frontend build step, Node.js dependency or separate frontend server.

## Use it

1. Set up Vercel, Claude Code and GitHub as described in
   [vercel-sandbox.md](vercel-sandbox.md), [claude-code.md](claude-code.md)
   and [github.md](github.md).
2. Run `make run` and open <http://127.0.0.1:8080>. The sidebar footer shows
   whether Vercel, Claude and GitHub are configured. Click **Connect** next to
   *GitHub* (or **Connect GitHub** on the welcome screen), open the
   preconfigured GitHub token link, choose the repositories, generate the
   token and paste it into the password field. **Replace** swaps in a new
   token later. See [github.md](github.md#connect-github).
3. Click **+ New** and pick or type a repository (`owner/repo`) and, optionally,
   a base branch. The suggestions list the repositories and branches your
   GitHub token can access. Mosso provisions a sandbox, clones the base branch
   onto a new `mosso/*` branch and installs Claude Code, which takes about a
   minute. The workspace shows as *Setting up* in the sidebar until it is ready.
4. Describe a task and press Enter (Shift+Enter for a new line). Claude's
   messages, tool calls and tool results appear as Claude produces them. Click
   a tool to see its input (commands, file edits as diffs) and its result.
   While Claude works, the composer is disabled; **Stop** interrupts the turn.
5. Send follow-ups. They resume the same Claude Code session, so Claude
   remembers the conversation.
6. Click **Create PR**, check the title, and publish. The chat shows the pull
   request link and whether it was opened or updated, and the header links
   to it. If nothing changed, the chat says so and nothing is pushed.
7. Keep working and click **Update PR** to push the new work to the same pull
   request. Before each message Mosso fetches the base branch, so you can ask
   Claude to rebase onto `origin/<base>` when the pull request has conflicts;
   the rebased branch is pushed with `--force-with-lease`. See
   [github.md](github.md#rebasing-an-open-pull-request).
8. Click **Terminate** and confirm to destroy the sandbox. The branch and pull
   request stay on GitHub.

You can switch between workspaces, reload the page or open a second tab at
any time. Claude keeps working on the server, and the conversation is
replayed from the server when you come back.

## Limitations

- **Everything is in memory.** Workspaces and conversations exist only in the
  running Mosso process and the sandbox. They are lost when Mosso stops
  (Mosso destroys every sandbox on shutdown), when you terminate the
  workspace, or when the sandbox expires. Only what you published (the
  branch and pull request) survives. There is no database yet.
- **Sandboxes expire.** A workspace sandbox lives 30 minutes (Vercel stops
  it then). The header counts down. After expiry the workspace shows an error:
  publish before then, and terminate and recreate the workspace afterwards.
- Each chat log is capped at about 32 MiB in memory. Past that, new tool
  inputs and outputs are replaced with a placeholder (the chat says so);
  Claude's messages and progress are still shown.
- Claude's progress streams one message at a time (each text block, tool call
  and tool result), not token by token.
- Single user, local only: the server has no authentication and only listens
  on loopback. See [SECURITY.md](../SECURITY.md).
- One thing runs at a time per workspace: a Claude turn, a publish, or setup.
  Other workspaces are not affected.

## How it works

```
browser ──POST /workspaces/{name}/chat──────────▶ Mosso ── starts a background turn
        ◀─GET /workspaces/{name}/events (SSE)──── Mosso ◀─ streamed stream-json lines ── Vercel command logs ◀── claude -p
```

- **Streaming.** Claude Code runs as `claude -p --output-format stream-json`.
  Mosso starts it as a Vercel Sandbox command and reads the command's log
  stream while it runs (`GET …/cmd/{id}/logs` is an NDJSON stream that stays
  open until the command exits, as in the official SDK). Each complete
  stream-json line is redacted, parsed and turned into chat events
  immediately. If the log connection drops, Mosso reconnects and skips the
  output it has already seen.
- **Turns belong to the server, not to a browser.** `POST …/chat` starts the
  turn and returns `202` at once. The turn runs in a goroutine until Claude
  finishes, you stop it, the workspace is destroyed, or Mosso shuts down.
  Closing or reloading the page does not interrupt it. Workspace setup also
  continues if the browser goes away while it is creating.
- **Chat log.** Every workspace keeps a log of entries: your prompts,
  Claude's events, turn results, errors, notices and pull requests. The
  server also keeps the Claude Code session ID, so follow-ups resume the
  conversation even after a turn was stopped. The UI never handles session IDs.
- **Server-sent events.** `GET …/events` replays the log and then streams new
  entries. Each entry's event ID is its index, so a reconnecting `EventSource`
  sends `Last-Event-ID` and receives only what it missed. The stream also
  sends the workspace (status, error, pull request) whenever it changes, and
  a keep-alive comment every 15 seconds.
- **Busy workspaces.** Claude turns, publishes and setup share the existing
  per-sandbox lock, so a second message or a publish while Claude works is
  rejected with `409`. The UI also disables the composer and buttons.

## API used by the UI

These come in addition to the endpoints in [github.md](github.md) and
[claude-code.md](claude-code.md), which the UI also uses (`POST /workspaces`,
`POST …/pull-request`, `DELETE /workspaces/{name}`, `GET /claude/auth`,
`GET /github/auth`, `POST /github/auth`).

| Method & path                              | Description |
| ------------------------------------------ | ----------- |
| `POST /workspaces/{name}/chat`             | Body: `{"prompt": "...", "attachments": ["<id>"]}` (`attachments` optional; `prompt` may then be empty). Starts a Claude Code turn that continues the workspace's conversation and returns `202 {"status":"working"}`. `409` if the workspace is busy, `503` if there is no Claude token. |
| `POST /workspaces/{name}/attachments`      | Multipart upload into the sandbox; see [attachments.md](attachments.md). `GET /workspaces/{name}/attachments/{id}` reads one back. |
| `POST /workspaces/{name}/chat/stop`        | Stops the running turn (`202`); `409` if Claude is not running. |
| `GET /workspaces/{name}/events`            | Server-sent events, see below. |
| `GET /github/repos`                        | `{"repos": [{"full_name", "default_branch", "private"}]}`: up to 100 repositories the GitHub token can access, most recently pushed first. |
| `GET /github/repos/{owner}/{repo}/branches`| `{"branches": ["main", …]}`: up to 100 branches. |

`GET /workspaces` and `GET /workspaces/{name}` now include:

```json
{
  "status": "ready",
  "error": "",
  "expires_at": "2026-11-13T15:04:05Z"
}
```

`status` is `creating`, `ready`, `working` (Claude or a publish is running)
or `error` (the last turn or publish failed, or the sandbox expired; `error`
says why). A workspace that is still being created is listed with status
`creating`. Workspace creation also installs Claude Code, so the first
message starts straight away.

The synchronous `POST /workspaces/{name}/claude/messages` still works as
before (it waits and returns the turn, and stops the turn if the client
disconnects). Its turns are also recorded in the chat log, so they show up
in the UI.

### Events

```
event: workspace
data: {"name":"mosso-…","status":"working",…}

id: 3
event: entry
data: {"id":3,"time":"…","type":"claude","event":{"type":"tool_use","tool_use_id":"toolu_…","name":"Bash","input":{"command":"go test ./..."}}}

event: closed
data: {}
```

Entry `type`s:

| type           | fields | meaning |
| -------------- | ------ | ------- |
| `user`         | `text` | a prompt |
| `claude`       | `event` | a Claude Code event: `text`, `tool_use` or `tool_result`, as in the `events` of a turn (see [claude-code.md](claude-code.md#api)) |
| `result`       | `result`: `text`, `is_error`, `subtype`, `num_turns`, `duration_ms`, `total_cost_usd` | the turn finished; `is_error` is true for failures Claude Code reports, such as an expired token or a usage limit |
| `error`        | `text` | the turn or publish failed in Mosso, the sandbox or GitHub (for example, an expired sandbox) |
| `notice`       | `text` | progress and status: installing Claude Code, publishing, stopped, nothing to publish |
| `pull_request` | `pull_request`: `number`, `url`, `branch`, `commit`, `created` | a publish succeeded |

`closed` means the workspace was destroyed and the stream ends. An unknown
workspace returns `404`.

## Security

- The server is unchanged in its exposure: loopback only, with the existing
  `Host`, `Origin` and JSON content-type checks. They also apply to the UI and
  the event stream, so other websites cannot read or drive it.
- No credential reaches the browser. The auth endpoints report only whether a
  token is configured and its source or GitHub login. Claude's output is
  redacted for the Claude token before it is parsed, logged or streamed, and
  prompts are not logged.
- Claude's output is untrusted: a repository can prompt-inject it. The UI
  inserts all of it as text (`textContent`). The Markdown renderer builds DOM
  nodes, never HTML strings, and only links `http(s)` URLs. A strict Content
  Security Policy (`script-src 'self'`, no inline scripts, no third-party
  origins, `frame-ancestors 'none'`) is a second line of defence.
