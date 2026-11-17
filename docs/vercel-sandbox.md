# Vercel Sandbox provisioning

Mosso can create a [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox), run
shell commands in it, and destroy it through its local HTTP API. The Go client in
`internal/vercel` calls the same REST endpoints as the official
`@vercel/sandbox` SDK; there is no Node.js dependency.

## Setup

1. Create (or pick) a Vercel project to own the sandboxes. Note its **project
   ID** (`prj_…`, Project → Settings → General) and your **team ID** (`team_…`,
   Team → Settings → General).
2. Create an access token at <https://vercel.com/account/tokens> scoped to that
   team. Prefer a short expiry.
3. Export the credentials in the shell that runs Mosso. Do not put them in a
   committed file; `.env` is git-ignored if you keep them in one locally.

   ```bash
   export VERCEL_TOKEN=...        # never commit or paste into issues/logs
   export VERCEL_TEAM_ID=team_...
   export VERCEL_PROJECT_ID=prj_...
   ```

4. Start Mosso:

   ```bash
   make run
   ```

   The log prints `Vercel Sandbox credentials loaded`. If any variable is
   missing, Mosso still starts but the `/sandboxes` endpoints, and
   everything else that needs a sandbox, return `503`; saved workspaces and
   their conversations can still be read
   (see [history.md](history.md#without-vercel-credentials)).
   Credential values are never logged or returned by the API.
   `GET /vercel/auth` also reports the detected plan and the longest session
   it allows.

## Try it

```bash
# 1. Provision a sandbox (optional body: {"timeout_seconds": 300})
curl -s -X POST -H 'Content-Type: application/json' localhost:8080/sandboxes
# {"name":"mosso-1a2b3c4d5e6f","session_id":"sbx_...","status":"pending","created_at":"..."}

# 2. Run a command
curl -s -X POST -H 'Content-Type: application/json' localhost:8080/sandboxes/mosso-1a2b3c4d5e6f/exec -d '{"command":"echo hello"}'
# {"command_id":"cmd_...","exit_code":0,"stdout":"hello\n","stderr":""}

# 3. Destroy it
curl -s -X DELETE localhost:8080/sandboxes/mosso-1a2b3c4d5e6f   # 204 No Content
```

## API

| Method & path                   | Description                                                                 |
| ------------------------------- | --------------------------------------------------------------------------- |
| `POST /sandboxes`               | Create a non-persistent `node22` sandbox. Body (optional): `timeout_seconds` (default 600, max 2700 on Hobby and 86400 on Pro and Enterprise; see below). |
| `GET /sandboxes`                | List sandboxes created by this Mosso process that have not been destroyed. |
| `POST /sandboxes/{name}/exec`   | Run `{"command": "..."}` with `bash -c`; waits and returns `exit_code`, `stdout`, `stderr` (each capped at 1 MiB). |
| `DELETE /sandboxes/{name}`      | Stop the running session and delete the sandbox.                            |

To run Claude Code in a sandbox, see [claude-code.md](claude-code.md). To
work on a GitHub repository and open a pull request, see [github.md](github.md).
Workspaces use the same sandboxes, limit and cleanup. A workspace's sandbox
can boot from a custom Vercel Container Registry image instead of `node22`
and run a setup script; see [sandbox-environment.md](sandbox-environment.md).

`POST` requests must send `Content-Type: application/json`. Mosso also rejects
requests whose `Host` or `Origin` is not loopback, so a web page you visit cannot
drive the API (CSRF / DNS rebinding), and it refuses to start on a non-loopback
`-listen`/`MOSSO_LISTEN` address because the API has no authentication.

A non-zero exit code is a successful `200` response; `502` means the Vercel API
call itself failed. Command text is not logged, but stdout/stderr are returned
to the caller verbatim, so avoid echoing secrets.

## Avoiding abandoned sandboxes

Sandboxes are billed while running, so Mosso layers several safeguards:

- **Vercel-side timeout**: every sandbox is created with a timeout (10 minutes by
  default for plain sandboxes, the workspace's session length, 45 minutes by
  default, for workspaces) after which Vercel stops it, even if Mosso crashes.
  Mosso detects the Vercel plan (`GET /v2/teams/{teamId}`) and allows up to
  its documented maximum: 45 minutes on Hobby, 24 hours on Pro and
  Enterprise, and 45 minutes when the plan cannot be read. See
  [idle-shutdown.md](idle-shutdown.md#the-vercel-plan).
- **Idle shutdown**: while Mosso runs, it stops a workspace's sandbox once
  nothing has run in it for the workspace's idle timeout (15 minutes by
  default), keeping its snapshot. See [idle-shutdown.md](idle-shutdown.md).
- **Concurrency**: Vercel enforces each account's limit on concurrent
  sandboxes (10 on Hobby, far more on Pro and Enterprise); Mosso has no
  limit of its own. When Vercel refuses a sandbox, creating answers `429`
  and says so. Stopped workspaces do not count.
- **Non-persistent** plain sandboxes: `POST /sandboxes` creates them with
  `persistent: false`, so no snapshots are kept after they stop. Workspaces
  (`POST /workspaces`) are **persistent** on purpose: their filesystem is
  snapshotted when a session stops and resumed later; see
  [worker.md](worker.md). One snapshot per workspace is kept.
- **Create failures**: Mosso picks the sandbox name before calling Vercel. If the
  create request fails or returns an unusable response, Mosso deletes that name.
- **Shutdown**: on `Ctrl-C`/`SIGTERM`, Mosso destroys every plain sandbox it
  created and has not yet destroyed. A failed destroy is kept in the list so
  you can retry `DELETE`, and is reported in the shutdown log. Workspace
  sandboxes are **not** destroyed on shutdown: Claude may still be working
  in them, and they stop by themselves at the session timeout. They are
  saved in Mosso's database and reconnected to on the next start;
  only `DELETE /workspaces/{name}` (Terminate) deletes them, together with
  their snapshots (the workspace's conversation is kept as history).

Plain sandboxes are tracked in memory only. If the process is killed with
`SIGKILL`, check the Vercel dashboard (or wait for the timeout) for any
`mosso-*` sandboxes that are not workspaces.
