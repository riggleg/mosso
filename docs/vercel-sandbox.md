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
   missing, Mosso still starts but the `/sandboxes` endpoints return `503`.
   Credential values are never logged or returned by the API.

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
| `POST /sandboxes`               | Create a non-persistent `node22` sandbox. Body (optional): `timeout_seconds` (default 600, max 18000). |
| `GET /sandboxes`                | List sandboxes created by this Mosso process that have not been destroyed. |
| `POST /sandboxes/{name}/exec`   | Run `{"command": "..."}` with `bash -c`; waits and returns `exit_code`, `stdout`, `stderr` (each capped at 1 MiB). |
| `DELETE /sandboxes/{name}`      | Stop the running session and delete the sandbox.                            |

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
  default) after which Vercel stops it, even if Mosso crashes.
- **Non-persistent**: sandboxes are created with `persistent: false`, so no
  snapshots are kept after they stop.
- **Create failures**: Mosso picks the sandbox name before calling Vercel. If the
  create request fails or returns an unusable response, Mosso deletes that name.
- **Shutdown**: on `Ctrl-C`/`SIGTERM`, Mosso destroys every sandbox it created
  and has not yet destroyed. A failed destroy is kept in the list so you can
  retry `DELETE`, and is reported in the shutdown log.

Mosso keeps its sandbox list in memory only. If the process is killed with
`SIGKILL`, check the Vercel dashboard (or wait for the timeout) for any
`mosso-*` sandboxes.
