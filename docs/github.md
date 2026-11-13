# GitHub workspaces and pull requests

A **workspace** is a GitHub repository checked out on a new working branch in
a Vercel Sandbox. You send Claude Code coding tasks in the checkout, then ask
Mosso to open a pull request with the result, all through the local API.

The code lives in `internal/github` (GitHub REST client, clone and publish)
and `internal/server/workspace.go` (HTTP endpoints). It builds on the sandbox
lifecycle in [vercel-sandbox.md](vercel-sandbox.md) and the Claude Code
integration in [claude-code.md](claude-code.md).

## How it works

1. **Create** (`POST /workspaces`): Mosso checks the token and the repository
   with the GitHub API *before* creating a sandbox. It then provisions a sandbox,
   clones the base branch into `/vercel/sandbox/<repo>`, creates a working branch
   `mosso/<random>`, and sets the Git identity to your GitHub account (your name
   and `<id>+<login>@users.noreply.github.com`). If any step fails, the
   sandbox is destroyed.
2. **Work** (`POST /workspaces/{name}/claude/messages`): the same Claude Code
   turn as the sandbox endpoint, but run in the checkout. Claude may edit and
   even commit; nothing leaves the sandbox yet.
3. **Pull request** (`POST /workspaces/{name}/pull-request`):
   1. In the sandbox, Mosso commits any uncommitted work with Git hooks and
      signing disabled, and lists what changed between the base commit and
      `HEAD`.
   2. It reads the changed files out of the sandbox and refuses to continue if
      any contains the GitHub or Claude token.
   3. On the Mosso server, it recreates the changes on the working branch
      through the GitHub API (blobs, tree, commit, branch). Git trees are
      content-addressed, so Mosso verifies that GitHub's tree hash equals the
      sandbox's `HEAD` tree hash: the pushed files are exactly the sandbox's.
   4. It opens a pull request from the working branch into the base branch,
      or returns the one already open.

   Calling it again after more work adds one commit to the same branch and
   returns the same pull request. If nothing changed since the last push, no
   commit is created.

Mosso only ever writes the `mosso/*` working branch. It never pushes to,
force-pushes, or merges into the base branch.

## Setup

1. Create a **fine-grained personal access token** at
   <https://github.com/settings/personal-access-tokens/new>:
   - **Repository access**: only the repositories Mosso should work on.
   - **Repository permissions**: *Contents: Read and write* and
     *Pull requests: Read and write*. *Metadata: Read* is added
     automatically.
   - A short expiration.
   - Do **not** grant *Workflows*. Without it, GitHub rejects changes to
     `.github/workflows`, so Claude cannot add or edit CI workflows that would
     run with your repository's secrets.

   A classic token with the `repo` scope also works, but it can access every
   repository you can. Avoid it.
2. Store it once in the macOS Keychain:

   ```bash
   go run ./cmd/mosso auth github
   ```

   Paste the token at both hidden prompts. It is saved as service `mosso`,
   account `github-token`. Mosso then checks it with GitHub and prints your login.
   By hand: `security add-generic-password -U -s mosso -a github-token -w`.

   On Linux, export `MOSSO_GITHUB_TOKEN` in the shell that starts Mosso.
   Mosso deliberately ignores `GITHUB_TOKEN`, which is often a broader token
   exported for other tools.
3. Configure Vercel and Claude Code as in their docs, then `make run`. The log
   prints `GitHub token found (keychain)`.

Mosso reads the token for every request, so to rotate it, run
`mosso auth github` again. No restart is needed. To remove it:
`security delete-generic-password -s mosso -a github-token`, and revoke it on
GitHub.

## API

| Method & path                                | Description |
| -------------------------------------------- | ----------- |
| `GET /github/auth`                           | `{"configured": true, "source": "keychain"\|"env", "login": "..."}`. Never returns the token. |
| `POST /workspaces`                           | Body: `{"repo": "owner/repo", "base": "main", "timeout_seconds": 1800}`. `base` defaults to the repository's default branch; `timeout_seconds` defaults to 1800 (max 2700). Clones, installs Claude Code and returns `201` with the workspace. Setup continues if the client disconnects. |
| `GET /workspaces`, `GET /workspaces/{name}`  | Workspaces of this Mosso process with their `status`, including ones still being created and `pull_request` once one exists. |
| `POST /workspaces/{name}/claude/messages`    | Same body and response as `POST /sandboxes/{name}/claude/messages`, run in the checkout. The turn is also recorded in the workspace's chat. |
| `POST /workspaces/{name}/chat`, `GET /workspaces/{name}/events` | Asynchronous chat with streamed progress, used by the web UI; see [web-ui.md](web-ui.md#api-used-by-the-ui). |
| `POST /workspaces/{name}/pull-request`       | Body (all optional): `{"title": "...", "body": "...", "draft": false}`. `201` when it opened the pull request, `200` when it already existed. |
| `DELETE /workspaces/{name}`                  | Destroy the sandbox (same as `DELETE /sandboxes/{name}`). The branch and pull request stay on GitHub. |

Workspace:

```json
{
  "name": "mosso-1a2b3c4d5e6f",
  "repo": "you/project",
  "base": "main",
  "base_sha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
  "branch": "mosso/0a1b2c3d4e5f",
  "dir": "/vercel/sandbox/project",
  "sandbox": {"name": "mosso-1a2b3c4d5e6f", "session_id": "sbx_…", "status": "running", "created_at": "…"},
  "expires_at": "…",
  "pull_request": {"number": 12, "url": "https://github.com/you/project/pull/12"},
  "status": "ready"
}
```

Pull request:

```json
{"number": 12, "url": "https://github.com/you/project/pull/12", "branch": "mosso/0a1b2c3d4e5f", "commit": "…", "created": true}
```

Errors (`{"error": "..."}`):

- `400` invalid input; `404` unknown workspace, or the repository does not
  exist or the token cannot see it (no sandbox is created).
- `409` Claude or another publish is still running in the workspace (a publish finishes even if the client disconnects).
- `422` no changes to publish: the working tree matches the base commit and
  nothing was published before. If an earlier publish exists and the work was
  reverted, the branch is updated back to the base tree instead.
- `429` sandbox limit reached; `503` Vercel or the GitHub token is not
  configured (or the Claude token, for messages).
- `502` the sandbox, clone, Git or GitHub API failed. The message includes
  Git's or GitHub's error, for example a missing base branch or a token
  without *Contents: write*. Failed creates destroy their sandbox. A failed
  publish leaves the workspace usable, so you can retry.

Limits: at most 2000 changed files and 16 MiB of changed content per publish.
Submodule changes are rejected. The token is only available in the sandbox
during the clone, so `git fetch`/`git pull` inside a private repository's
sandbox fail later.

## Try it

```bash
curl -s localhost:8080/github/auth | jq

WS=$(curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces \
  -d '{"repo":"you/project"}' | tee /dev/stderr | jq -r .name)

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/claude/messages \
  -d '{"prompt":"Fix the typo in README.md and run the tests."}' | jq .result

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/pull-request \
  -d '{"title":"Fix README typo","draft":true}' | jq .url

curl -s -X DELETE localhost:8080/workspaces/$WS
```

## Tests

`go test ./...` covers this with fakes only: no real network or accounts.
`internal/github` runs the real Git scripts on your machine against a local
bare repository. The repository is served over authenticated smart HTTP
(`git http-backend`) and backed by a fake GitHub API, so the credential
helper, commit and tree replication are exercised for real. These tests skip
if `git` or `git-http-backend` is missing. `internal/server` covers the HTTP
endpoints, cleanup on failure, and that the token is sent into the sandbox
only for a private clone.

### End-to-end test (real accounts, opt-in)

`TestGitHubEndToEnd` creates a real workspace, has Claude add one file,
and opens a real **draft** pull request. It never runs unless you ask:

```bash
export VERCEL_TOKEN=… VERCEL_TEAM_ID=team_… VERCEL_PROJECT_ID=prj_…   # see vercel-sandbox.md
# Claude token: `mosso auth claude` or CLAUDE_CODE_OAUTH_TOKEN
# GitHub token: `mosso auth github` or MOSSO_GITHUB_TOKEN, with write access to the repo below
MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/mosso-playground \
  go test ./internal/server -run TestGitHubEndToEnd -v -timeout 30m
```

- Use a **disposable repository**, such as an empty repository with a README,
  that the token can write to. `MOSSO_E2E_GITHUB_BASE` picks the base branch
  (default: the repository's default branch).
- The test adds `mosso-e2e/<id>.md` on a `mosso/<id>` branch. It never merges
  or writes to the base branch.
- **Cleanup**: when the test ends, pass or fail, it closes the pull request,
  deletes the `mosso/<id>` branch and destroys the sandbox. Set
  `MOSSO_E2E_KEEP_PR=1` to keep the pull request and branch for inspection.
  Then close the pull request and delete the branch on GitHub yourself, or run
  `gh pr close <number> --delete-branch -R you/mosso-playground`.
- If the test is killed (for example with Ctrl-C twice or a timeout), check for
  leftovers: `mosso-*` sandboxes in the Vercel dashboard (they also stop at
  their 20-minute timeout), and `mosso/*` branches and open pull requests in
  the repository.
- It uses a little of your Claude subscription and Vercel usage.

## Security model and limitations

- **The token stays on your Mac** (Keychain or the server's environment).
  Mosso reads it per request and sends it only to `api.github.com` in the
  `Authorization` header. It never appears in logs, API responses, Git remote
  URLs, command arguments or `.git/config`, and Mosso redacts it from GitHub
  error messages.
- **Public repositories are cloned anonymously**, so the token never enters
  the sandbox.
- **Private repositories need the token for the clone.** It is in the
  environment of that one `git clone` command only, delivered through the
  Vercel API request body. A command-line credential helper reads it, after an
  empty `credential.helper` clears any other helper. The clone runs before
  Claude or any repository code runs in the sandbox, and nothing stores the
  token on disk. Environment variables do not protect secrets from code in
  the sandbox. This design is safer because nothing untrusted runs while the
  token is there. Vercel, which operates the sandbox, can see it like
  any value sent to the sandbox.
- **Pushing and opening pull requests happen on the Mosso server** through the
  GitHub API, so later Claude turns, hooks and repository code cannot read
  the GitHub token. Claude cannot push or open pull requests by itself;
  only Mosso does, when you call the endpoint.
- **The Claude token is still in the sandbox** while Claude runs (see
  [claude-code.md](claude-code.md#security-model-and-limitations)). A prompt
  injection from repository content can read it and send it out over the
  network. Mosso refuses to publish files that contain the Claude or GitHub
  token, so a leaked token cannot end up in a pull request. That check
  catches only the exact value, not an encoded copy, and it cannot stop
  network exfiltration. Only point Claude at repositories you trust.
- **Pull requests come from branches in the same repository**, so your
  existing CI runs Claude's code with the secrets those workflows can use (as
  for any collaborator's branch, unlike pull requests from forks). Keep
  sensitive secrets out of workflows that run on `pull_request`, or require
  approval for them.
- **Everything Claude changes is published**, except files ignored by
  `.gitignore`. Review the pull request before merging, as for any
  contributor.
- Commits on GitHub are authored by your token's account.
- Workspaces are in memory only; restarting Mosso destroys their sandboxes,
  but branches and pull requests remain on GitHub.
