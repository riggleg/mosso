# Session length, idle shutdown, Stop and Terminate

A workspace's sandbox is a persistent Vercel Sandbox. It runs in
**sessions**: a session is one running VM, billed for its memory for as long
as it runs, idle or not. When a session stops, Vercel snapshots the
sandbox's filesystem; the next session boots from that snapshot. Mosso
decides how long sessions may last, stops them when nothing is happening,
and resumes them when you need the workspace again.

| | What it is | Set where | Default |
| --- | --- | --- | --- |
| **Session length** | The most one session may run before **Vercel** stops it, whatever happens. A backstop that Vercel enforces even if Mosso is not running. | New workspace dialog (`timeout_seconds`); fixed for the workspace's life | 45 minutes, on every plan |
| **Idle timeout** | How long a session may sit **idle** before **Mosso** stops it. | New workspace dialog (`idle_timeout_seconds`); can be changed in the workspace header (`PATCH /workspaces/{name}`) | 15 minutes; "Never" turns it off |

The two are independent. For example, with a 24-hour session length and a
15-minute idle timeout, Claude can work on one long task for hours, and the
sandbox stops 15 minutes after it finishes. With idle shutdown off, the
session runs until its session length.

And four actions:

| | Compute | Files and snapshot | Conversation and Claude session | Listed under |
| --- | --- | --- | --- | --- |
| **Stop** (idle shutdown, or **Stop sandbox**) | Stops | Kept (snapshotted) | Kept | Active workspaces, as *Stopped* |
| **Resume** (your next message, or **Resume sandbox**) | Starts a new session | Restored from the snapshot | Continues | Active workspaces |
| **Terminate** | Stops | **Deleted**, with the snapshots | Kept as read-only history | History |
| **Permanently delete** | — | — | Deleted from Mosso's database | Nowhere (see [history.md](history.md); there is no button) |

A stopped sandbox costs only snapshot storage. Resuming takes a few
seconds; Mosso restarts its worker, which picks up its event log, so the
chat continues with no gaps and no duplicates, and follow-ups resume the
same Claude Code session.

## The Vercel plan

Mosso reads your plan with the Vercel credentials it already has
(`GET /v2/teams/{teamId}`, field `billing.plan`). Vercel's API does not
report Sandbox limits, so Mosso uses the documented ones
([Sandbox pricing and quotas](https://vercel.com/docs/sandbox/pricing#runtime-limits),
checked November 2026):

| Plan | Longest session |
| --- | --- |
| Hobby | 45 minutes |
| Pro | 24 hours |
| Enterprise | 24 hours |

- The plan is cached for an hour. When a workspace asks for a longer session
  than the cached plan allows, Mosso checks again first (at most once a
  minute), so an account upgraded on vercel.com can use its new limit, with
  no restart or configuration. `GET /vercel/auth?refresh=1` checks again
  too.
- If the check fails, Mosso keeps the plan from the last successful check,
  or uses the Hobby limits if there was none. A plan Mosso does not know
  also gets the Hobby limits. The sidebar's Vercel line shows the plan and
  its limit, and says when the Hobby limits are assumed.
- `GET /vercel/auth` returns `plan`, `plan_known`, `max_session_seconds`,
  `default_session_seconds`, `default_idle_timeout_seconds`,
  `plan_checked_at` and `plan_error`. It never returns credentials.

The New workspace dialog offers 15, 30 and 45 minutes, then 1, 2, 4, 8, 12
and 24 hours, up to the plan's maximum. The API accepts any value from 5
minutes to that maximum.

**Session length when resuming.** Mosso creates the sandbox with the
chosen session length. If Vercel starts a resumed session with less, Mosso
extends it (`POST /v2/sandboxes/sessions/{id}/extend-timeout`). If Vercel
refuses (for example after a downgrade to Hobby), the chat says so and the
session keeps what Vercel gave it.

## When a workspace is idle

A workspace is idle when nothing that Mosso manages runs in it:

- no Claude turn is running. The worker in the sandbox runs Claude Code and
  reports `running` from the moment it accepts a message until it has
  recorded the turn's end. That covers Claude's tool calls and its
  subagents, which run inside the turn.
- no call of Mosso's GitHub tools is waiting for, or being run by, the
  server;
- no workspace setup, publish (Create PR), resume or stop is in progress.

The idle time counts from the later of:

- when the worker became idle. The worker reports `idle_since` in
  `GET /status`: the end of its last task, or, after it restarts, the time
  of the last event in its log. A worker restart does not reset it.
- the last thing the server did in the workspace: a resume, a publish,
  changing the idle timeout, or the end of a turn as recorded from the
  worker's events (by the worker's clock).

CPU use plays no part. Neither do browser tabs: closing Mosso's page does
not stop a sandbox, and an open page does not keep an idle one running.
Background processes that Claude leaves running after its turn ends (a dev
server, say) do not count as activity, and stopping the session ends them.

The workspace header shows when an idle sandbox will be stopped ("stops
when idle in 12 min"). `GET /workspaces/{name}` has `idle_stop_at` while the
countdown runs, and `session_seconds` and `idle_timeout_seconds` always.

## How a sandbox is stopped

Every 30 seconds Mosso looks for workspaces whose idle time has reached
their idle timeout. For each one:

1. It marks the workspace **Stopping**, which holds it like a running task:
   no message or publish can start. A message sent now waits for the stop
   to finish, then resumes the sandbox and runs. It is never refused or
   lost.
2. It asks the worker for its status. If Claude is running, or a tool call
   is pending, nothing is stopped. The same happens if the worker became
   idle more recently than Mosso knew; the check is repeated later.
3. It stops the session with `POST /v2/sandboxes/sessions/{id}/stop`. That
   ends the VM; Vercel snapshots the persistent sandbox's filesystem. Mosso
   never deletes the sandbox or its snapshots here (only Terminate does).
4. The workspace is marked **Stopped**, saved in the database, and the chat
   says why it stopped. It stays under the active workspaces.

If the stop request fails, the sandbox keeps running and the chat says so
once. Mosso tries again after 1 minute, then 2, 4 and 8, and then every 15
minutes, until it works. A sandbox already stopped by Vercel meanwhile is
simply marked stopped. Stops and resumes of one workspace never overlap: a
resume waits for a stop in progress to end.

**Stop sandbox** in the header (`POST /workspaces/{name}/sandbox/stop`)
does the same at once, without waiting for the idle timeout. It is refused
(`409`) while Claude or a publish is running: stop Claude first. If Vercel
refuses the stop, the request fails (`502`) and the sandbox keeps running;
nothing is retried for you. Clicking Stop while an idle check is deciding
waits for it, then stops the sandbox if the check did not.

## Workspace statuses

`status` in the API, and the label in the web UI:

| `status` | Shown as | Meaning |
| --- | --- | --- |
| `creating` | Setting up | Provisioning, cloning, setup script, installing |
| `connecting` | Connecting | Checking the sandbox after Mosso started |
| `working` | Working | Claude or a publish is running |
| `ready` | Idle | The sandbox runs and nothing runs in it; idle shutdown counts down |
| `stopping` | Stopping | Mosso is stopping the session |
| `stopped` | Stopped | No session; the next message resumes it |
| `resuming` | Resuming | A new session is starting from the snapshot |
| `error` | Error / Gone | The last turn or publish failed, or the sandbox no longer exists |
| `terminated` | Terminated / Archived | History |

`sandbox_state` (`running`, `stopped`, `gone`, `terminated`) is unchanged.

## When Mosso is not running

The worker cannot stop its own sandbox. Stopping a session takes a Vercel
API call with a Vercel access token, and Vercel offers no way for a sandbox
to stop itself without one (as of November 2026: no in-sandbox token or
self-stop endpoint is documented, and Vercel does not document what happens
to the snapshot if the VM powers itself off). Mosso's rule is that the
sandbox, where Claude runs unattended with every permission, never holds
credentials. A team-scoped Vercel token there would let a prompt-injected
agent control every project and deployment of the team. So **idle shutdown
runs on the Mosso server**, and:

- **While Mosso is not running, or your Mac is asleep, idle sandboxes are
  not stopped by Mosso.** They run until their session length, which Vercel
  enforces regardless. The session length is your cost ceiling for those
  times: 45 minutes by default. A 24-hour session that finishes its work
  just after you close the laptop can idle for up to the rest of the 24
  hours. Choose long sessions only for workspaces that need them.
- **Claude's work is never interrupted by idle shutdown**, and Mosso being
  away interrupts nothing either: the worker keeps running Claude until the
  task ends or the session length is reached, as before.
- **When Mosso is back** (it starts, or the Mac wakes), it reconnects and
  reads the worker's `idle_since`. A sandbox that has already been idle for
  its idle timeout, counting from when Claude finished rather than from when
  Mosso came back, is stopped at the next check, within about 30 seconds.

**Possible improvements, not implemented.** Vercel's
`PATCH /v2/sandboxes/{name}` can change a sandbox's `timeout`. If that
applied to the running session, Mosso could shorten the session's deadline
to the idle timeout as soon as a turn ends. Vercel would then stop the
sandbox even if the Mac went to sleep right after. Whether it applies to the
running session is not documented, and it was not tested. Alternatively,
sessions could start with a short timeout that Mosso extends while Claude
works. That would stop idle sandboxes without Mosso, but would interrupt
Claude whenever Mosso is away during a task, which this design rules out.

## Concurrency

Mosso no longer limits how many sandboxes run at once (it used to allow 5).
Vercel enforces each account's own limit: 10 concurrent sandboxes on Hobby,
10,000 on Pro and Enterprise by default, plus a rate of vCPU allocation.
When Vercel refuses to create or resume a sandbox (`429`), Mosso answers
`429` for a create and `502` for a resume, saying that the account's limit
was reached: stop or terminate a workspace you are not using, or wait a
minute. Stopped workspaces do not count against Vercel's limit.

## Database

The session length and idle timeout are saved in each workspace's `state`
JSON (`session_seconds`, `idle_timeout_seconds`), so no migration was
needed. Workspaces from before this change have 45-minute sessions and get
the default 15-minute idle timeout. The rest (stopping, the idle clock,
retry timing) lives in memory: after a restart, Mosso asks the worker again.

## Tests

`go test ./...` covers, over the fake Vercel with real in-process workers:
plan detection (Hobby, Pro, Enterprise, unknown, API failure, caching, an
upgrade noticed without a restart, session limits on create); stopping
after the idle timeout once a task ends; never stopping while Claude runs,
even if the server lost track of the turn (the worker is asked); a pending
tool call; a message arriving during a stop (it waits, then resumes);
Mosso stopped while Claude works and back after Claude finished (it stops
by the worker's idle clock); idle shutdown turned off and on; a failing
stop retried with backoff; stop and resume across a restart; resumed
sessions extended to the workspace's session length; Vercel's capacity
errors; workspaces saved before this change; and Stop sandbox.
`node --test internal/server/webtest/*.test.js` covers the plan-aware
duration choices, idle timeout choices and status labels.

The live test, opt-in, with your accounts (one persistent sandbox,
destroyed at the end):

```bash
MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/some-repo \
  go test ./internal/server -run TestIdleShutdownEndToEnd -v -timeout 30m
```

It reads the plan, runs a 100-second Claude task in a workspace with a
1-minute idle timeout while checking for idle workspaces every 20 seconds,
checks with the Vercel API that nothing was stopped, then that the sandbox
is stopped (not deleted) once the task ended, and that a follow-up resumes
it and remembers the conversation. **It was not run by the author of this
change**, who had no Vercel or Claude credentials.

To look at it in a browser without accounts:

```bash
MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=idle MOSSO_FAKE_PLAN=pro \
  go test ./internal/server -run TestServeFakeForBrowser -timeout 1h
```
