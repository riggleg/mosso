# Remote worker, persistent workspaces and reconnection

Claude Code runs independently inside each workspace's Vercel Sandbox, under
a small Mosso **worker** process. The local Mosso server submits messages to
the worker and reads back what Claude did, by sequence number. So you can
start a task, close the laptop or stop Mosso, let Claude finish in the
sandbox, start Mosso later and read everything that happened. Workspaces are
saved on your machine and their sandboxes are **persistent**: when a sandbox
session ends (Vercel stops it after at most 45 minutes), its filesystem is
snapshotted and resumed on your next message.

The code lives in `internal/worker` (shared types, the event log, the
worker's HTTP API and its Claude Code runner), `cmd/mosso-worker` (the
binary that runs in the sandbox), `internal/server/remote.go` (how the server
talks to the worker), `internal/server/restore.go` (saving, restoring,
resuming, following) and `internal/server/store.go` (the local data
directory).

## Who does what

| Local Mosso server (your machine) | Remote worker (in the sandbox) |
| --- | --- |
| Creates and terminates workspaces; saves them in its data directory | Runs Claude Code as its child process and owns its lifetime |
| GitHub: token, fetch, push, pull requests (unchanged) | Records every event to an append-only, fsync'd log on the sandbox's filesystem |
| Claude: token (Keychain), sent with each message | Holds the token in memory for one task only |
| Submits messages, follows events, stops tasks | Keeps running when the server disconnects or stops |
| Serves the web UI and replays the chat | Answers status, event and stop requests on loopback |
| Restores workspaces after a restart and reconnects | Marks a task interrupted if it finds one still open when it starts |

## Lifecycle

**Create.** `POST /workspaces` provisions a *persistent* sandbox
(`persistent: true`, one kept snapshot), clones the repository, installs
Claude Code, uploads the worker binary to
`/vercel/sandbox/.mosso/bin/mosso-worker-<build>` with Vercel's write-files
API and starts it detached (`setsid nohup … &`). The worker listens on
`127.0.0.1:7777` inside the sandbox and keeps its state in
`/vercel/sandbox/.mosso/`: `events.jsonl` (the event log), `worker.log`,
and `attachments/`. The server saves the workspace record and starts
following the worker's events.

**Message.** `POST /workspaces/{name}/chat` records your prompt, then in the
background: resumes the sandbox if its session ended, makes sure the worker
runs, fetches `origin/*` from GitHub (as before), and sends the message to
the worker with the Claude token. The worker records a `user` event, answers
`202` at once and starts `claude -p … --output-format stream-json` as a
child. Each line of Claude's output becomes a `claude` event (text, tool
call, tool result), written to the log and fsync'd before the next one, so
nothing already recorded is lost if the session ends abruptly. The server's
follower receives each event within about a second and appends it to the
chat, which is also saved locally. The turn ends with a `result`, `error`,
`stopped` or `interrupted` event.

**Stop.** `POST /workspaces/{name}/chat/stop` asks the worker to send
SIGTERM to Claude Code's process group; the worker records `stopped`.

**Mosso stops.** `Ctrl-C` (or a crash) does not touch workspace sandboxes:
the worker and Claude keep going, and the sandbox runs until Vercel's session
timeout. Only plain sandboxes made with `POST /sandboxes` are destroyed on
shutdown, as before. Server-side Git mirrors (temporary directories) are
removed and recreated on demand.

**Mosso starts.** It loads every saved workspace from the data directory
(the chat shows immediately, from the local copy) and checks each sandbox
with the Vercel API:

- *running*: the worker is still there; Mosso follows its events from the
  last sequence number it had seen, so whatever happened meanwhile is added
  to the chat, once.
- *stopped*, and Claude was working when Mosso last saw it: Mosso resumes
  the sandbox to fetch what Claude saved before the session ended, and the
  worker's log marks the task interrupted.
- *stopped*, idle: left stopped (a stopped sandbox costs nothing). Your next
  message, **Resume sandbox** in the header, or `POST …/connect` resumes it.
- *gone* (deleted, or its snapshot expired): the chat stays readable; new
  messages are refused; **Terminate** removes the saved state.

**Resume.** `GET /v2/sandboxes/{name}?resume=true` boots a new session from
the last snapshot: the checkout, Claude Code's transcripts (so follow-ups
still resume the same Claude session), attachments and the worker's event
log are all there; processes are not, so Mosso starts the worker again. The
worker reads its log, continues the sequence numbers, and, if the last task
had no terminal event, records `interrupted` for it. The server's Git mirror
is pointed at the new session.

**Terminate.** `DELETE /workspaces/{name}` stops the session, deletes the
sandbox and the snapshots no other sandbox uses, and removes the workspace
from the data directory. The branch and pull request stay on GitHub. This is
the only way a workspace is deleted.

## The 45-minute session

Vercel stops a session at its timeout (45 minutes is the Hobby maximum and
Mosso's default for workspaces). Persistence makes this an interruption
rather than a loss:

- **Claude finishes before the timeout.** The worker records the result in
  the sandbox; Mosso shows it as soon as it is connected (right away, or on
  the next start). The session then idles until the timeout and stops; the
  snapshot keeps everything.
- **The session ends while Claude is working.** Claude Code is killed with
  the VM. Everything recorded before that (its text, tool calls and results
  so far) is in the snapshot; the result is not. Mosso's follower notices
  the session end and marks the turn *Interrupted*; after a resume the
  worker confirms it from its log. Nothing is rerun automatically: send a
  follow-up (for example "continue where you left off") and Claude resumes
  its own session with its transcript intact.
- **Mosso is stopped while Claude is working.** The worker keeps going until
  Claude finishes or the session ends. Mosso catches up when it starts.

Mosso does not extend sessions or restart work by itself.

## What survives what

| | Local Mosso restart | Sandbox session stop (timeout) | Sandbox gone |
| --- | --- | --- | --- |
| Chat history Mosso had already seen | yes (data directory) | yes | yes |
| Events while Mosso was away | yes, fetched from the worker's log | yes, after resume | no |
| Claude Code session (follow-ups resume it) | yes | yes, after resume | no |
| Checkout, uncommitted changes | yes | yes, after resume | no |
| Attachments | yes | yes, after resume (not readable while stopped) | no |
| Published branch and pull request | yes | yes | yes |

**Durability caveats (verify before relying on them).** Vercel documents that
a persistent sandbox's filesystem is snapshotted automatically when a session
stops, including at the timeout, and that the next session resumes from it.
It does not publish a formal durability guarantee for writes made just before
an abrupt stop, and a snapshot can end up in a `failed` state. Mosso fsyncs
every event, so the event log is only exposed to the snapshot mechanism
itself. The automatic snapshot is **not verified by Mosso's automated
tests**: the acceptance test below stops a session with the API and resumes
it, which exercises the same mechanism, but a real timeout mid-write was not
observed by the author of this change. If a snapshot is lost, the sandbox
cannot resume (`snapshot_not_found`): the workspace shows as gone, and the
history Mosso had already mirrored locally is all that remains. Vercel Drives
(network-attached storage with its own lifecycle) would be the next step if
snapshots prove unreliable; they were not needed for this milestone and are
not used.

Snapshots are also subject to retention: Vercel keeps them 30 days after
their last use by default, deletes sandboxes that cannot resume after 14 days
of inactivity, and on the Hobby plan counts snapshot storage against a 15 GB
lifetime allotment (Mosso keeps one snapshot per workspace). Terminate
workspaces you are done with.

## Data directory

Mosso saves workspaces under `MOSSO_DATA_DIR`, or `~/Library/Application
Support/mosso` on macOS (`$XDG_CONFIG_HOME/mosso` or `~/.config/mosso`
elsewhere):

```
workspaces/<name>/workspace.json   repository, branch, sandbox, Claude session ID,
                                   last worker event applied, pull request, attachments
workspaces/<name>/chat.jsonl       one chat entry per line, as the web UI shows them
```

No credentials are stored there. The files are plain JSON; delete a
workspace's directory to forget it without touching Vercel (Terminate does
both).

## Worker API

The worker listens on loopback only. Mosso reaches it by running `curl`
through the Vercel Sandbox command API, so nothing is exposed on the
internet and no port is published. Request bodies (which carry the Claude
token) travel in the command's environment, never in its arguments.

| Method & path | Description |
| --- | --- |
| `GET /status` | `{"state":"idle"\|"running","seq":N,"session_id":"…","task_id":"…","started_at":"…","version":"1","pid":…}` |
| `GET /events?after=N[&follow=1]` | The events with a sequence number greater than N, one JSON object per line. With `follow`, the stream stays open, sends new events as they are recorded and a `{"type":"ping"}` line every 15 s. |
| `POST /messages` | `{"text","attachments","prompt","session_id","dir","system_prompt","token"}`. Records the `user` event and starts Claude Code. `202 {"task_id","seq"}`; `409` if a task is running. |
| `POST /stop` | Interrupts the running task (`202`); `409` if idle. |

Events: `{"seq","time","type","task_id",…}` with `type` one of `user`
(`text`, `attachments`), `session` (`session_id`), `claude` (`event`: the
same shape as in [claude-code.md](claude-code.md)), `result` (`result`,
`session_id`), `error`, `stopped`, `interrupted`, `notice`.

The event log is bounded: tool inputs over 64 KiB are replaced with a
placeholder, tool results are truncated to 16 KiB (as before), and past 64
MiB of log the tool inputs and outputs are no longer recorded (a notice says
so). Claude's text and progress are always recorded.

Mosso follows a worker with one long-running `curl -N` command per
workspace, restarted every 10 minutes and whenever it ends, always from the
last sequence number it applied, so a reconnect never duplicates an entry.

## API changes on the Mosso server

- `GET /workspaces` and `GET /workspaces/{name}` gain `sandbox_state`
  (`running`, `stopped`, `gone`) and `created_at`; `expires_at` is omitted
  while the sandbox is stopped; `status` gains `connecting` (checking the
  sandbox after a restart) and `stopped`.
- `POST /workspaces/{name}/connect` resumes a stopped sandbox, restarts the
  worker and catches up; returns the workspace. `410` if the sandbox is gone.
- `POST …/chat`, `POST …/claude/messages`, `POST …/attachments` and
  `POST …/pull-request` resume a stopped sandbox themselves. They answer
  `410` for a gone sandbox.
- `GET …/attachments/{id}` answers `410` while the sandbox is stopped (it
  does not boot a sandbox to serve a thumbnail).
- Chat entries carry `seq`, the worker event they came from.
- The default workspace session length is now 45 minutes.

## Security

- **The worker API is loopback only** and is reached through Vercel's
  authenticated command API. It is not published on a port.
- **The Claude token** is sent with each message, inside the request body,
  through the command environment (as Mosso already did for the `claude`
  command). The worker keeps it in memory for the duration of the task and
  passes it to Claude Code's environment. It is never written to the
  worker's log or disk, and is redacted from Claude's output before it is
  recorded. As before, code running in the sandbox, including a
  prompt-injected Claude, can read it from the worker's or Claude Code's
  process environment while a task runs; the sandbox is the trust boundary.
  See [claude-code.md](claude-code.md#security-model-and-limitations).
- **Anything in the sandbox can call the worker** (`curl 127.0.0.1:7777`),
  for example Claude itself. It could read the event log or queue a message
  (which needs a token it would have to steal first). This is the same
  trust level as the sandbox's own files.
- **GitHub credentials never enter the sandbox** (unchanged); pushes still
  run on your machine.
- **The data directory** holds your prompts and Claude's output, like the
  web UI did in memory. It holds no credentials.

## Tests

`go test ./...` covers this with fakes and real local processes:

- `internal/worker`: the event log (fsync, recovery of a truncated tail,
  sequence numbers, size bounds), the HTTP API, running a fake `claude`
  asynchronously, incremental recording while it runs, stop, 409 on a second
  message, redaction, `--resume`, and a restart marking an open task
  interrupted.
- `internal/server`: a fake Vercel API with persistent sandboxes (stop,
  resume into a new session, delete, loss) hosting a real in-process worker
  per sandbox with a scripted Claude. It covers: Claude continuing after
  Mosso shuts down and the events being recovered once on restart; restart
  after the session stopped (history from disk, resume on the next message,
  worker restarted over its log, attachments and publishing afterwards); the
  connect endpoint; a session ending mid-task (interrupted once, follow-up
  continues); restart after a mid-task session end (automatic resume and
  catch-up); terminate removing sandbox, snapshots and saved state; a gone
  sandbox keeping its history; restored publish state (`--force-with-lease`
  still works); replay without duplicates; the store.

The live acceptance test, opt-in, with your accounts (one persistent
sandbox, destroyed at the end; nothing pushed to GitHub):

```bash
export VERCEL_TOKEN=… VERCEL_TEAM_ID=team_… VERCEL_PROJECT_ID=prj_…
# Claude token: `mosso auth claude` or CLAUDE_CODE_OAUTH_TOKEN
# GitHub token: `mosso auth github` or MOSSO_GITHUB_TOKEN, able to read the repo
MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/some-repo \
  go test ./internal/server -run TestReconnectEndToEnd -v -timeout 40m
```

It starts a 45-second Claude task, shuts the server down mid-task, confirms
through the Vercel API that the worker finishes alone, restarts over the same
data directory and checks the history is complete and not duplicated, sends
a follow-up, then stops the sandbox session with the API, restarts, resumes
and sends another follow-up. It needs Go on the machine (it cross-compiles
the worker) and runs from the repository. **It was not run by the author of
this change**, who had no Vercel or Claude credentials; run it on your Mac.
Because it runs on your machine with your Keychain tokens, it is the test
that stands in for "close the laptop".

To watch the UI over a fake sandbox instead:
`MOSSO_FAKE_ADDR=127.0.0.1:8099 go test ./internal/server -run TestServeFakeForBrowser -timeout 1h`.

## Remaining failure scenarios

- **Snapshot not taken or failed** at an abrupt session end: the sandbox
  cannot resume; the workspace shows as gone. Only what Mosso had already
  mirrored survives. Unverified how often this happens (see above).
- **Mosso stops before the message reaches the worker** (within the first
  second or two after Send, while the sandbox is resuming or `origin/*` is
  fetched): the prompt is in the local chat, but Claude never saw it. On
  restart Mosso sees the worker idle with no new events and marks the turn
  *Interrupted*; send it again.
- **Older snapshot than Mosso's view.** If a sandbox resumes from a snapshot
  that predates events Mosso had already recorded (it should not, since the
  snapshot is taken at stop), Mosso says so in the chat and follows from the
  worker's sequence; some entries may appear twice.
- **Worker binary.** `make run`/`make build` embed a linux/amd64 worker;
  `go run ./cmd/mosso` from the source tree builds it on first use with the
  Go toolchain. A `mosso` binary moved elsewhere without an embedded worker
  cannot create or resume workspaces; the error says to run `make worker`.
- **The Mosso server crashes (SIGKILL) mid-write.** The workspace record is
  written atomically; a half-written last chat line is skipped on load. At
  most the last entry is lost locally, and it is re-fetched from the worker
  if it was a worker event.
- **Vercel API errors while following.** The follower retries with backoff
  (1 s to 30 s). Chat entries resume where they left off.
- **Snapshot storage and retention** limits (Hobby: 15 GB lifetime; 30-day
  snapshot expiry; 14-day removal of unresumable sandboxes) can make a
  dormant workspace gone. Terminate workspaces you no longer need.
