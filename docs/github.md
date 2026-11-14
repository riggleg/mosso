# GitHub workspaces and pull requests

A **workspace** is a GitHub repository checked out on a new working branch in
a Vercel Sandbox. You send Claude Code coding tasks in the checkout, then ask
Mosso to push the branch and open a pull request, from the web UI or the
local API. You connect GitHub once; every workspace uses that connection, and
nobody ever enters GitHub credentials inside a sandbox.

The code lives in `internal/github` (clone, the server-side Git relay in
`mirror.go`, REST client) and `internal/server/workspace.go` (HTTP endpoints).
It builds on the sandbox lifecycle in [vercel-sandbox.md](vercel-sandbox.md)
and the Claude Code integration in [claude-code.md](claude-code.md).

## Connect GitHub

Mosso uses a **fine-grained personal access token** that you create once and
paste into Mosso.

1. Start Mosso (`make run`) and open <http://127.0.0.1:8080>. In the sidebar,
   click **Connect** next to *GitHub* (or **Connect GitHub** on the welcome
   screen).
2. Click **Open GitHub to create a token**. GitHub opens the token form with
   everything preselected:

   <https://github.com/settings/personal-access-tokens/new?name=Mosso&description=GitHub+access+for+Mosso+coding+workspaces&expires_in=90&contents=write&pull_requests=write>

   | Setting           | Value |
   | ----------------- | ----- |
   | Token name        | Mosso |
   | Description       | GitHub access for Mosso coding workspaces |
   | Expiration        | 90 days |
   | Contents          | Read and write (clone, fetch, push the `mosso/*` branch) |
   | Pull requests     | Read and write (open and find the pull request) |
   | Metadata          | Read (always included) |

3. Under **Repository access**, choose **Only select repositories** and pick
   the repositories Mosso may work on. (You can also choose *All
   repositories*, but a smaller set limits what a leaked token can reach.)
   If the repositories belong to an organization, pick it as the resource
   owner; the organization may need to approve the token.
4. Click **Generate token**, copy it, paste it into Mosso's password field
   and click **Connect**. Mosso checks it with GitHub and shows your login.

The token is now used for every workspace, including ones that already exist.

Where the token is stored:

- **macOS**: in your login Keychain (service `mosso`, account
  `github-token`). It survives restarts.
- **Linux and other systems without a Keychain**: in the Mosso process's
  memory until it stops. For a token that survives restarts, export
  `MOSSO_GITHUB_TOKEN` in the environment that starts Mosso. A token pasted
  into the UI takes precedence over the variable until Mosso stops. Mosso
  deliberately ignores `GITHUB_TOKEN`, which is often a broader token
  exported for other tools.

The token never reaches the browser's storage: it is a password field, sent
once over loopback in a `POST /github/auth` request body, and cleared from
the page right away. Mosso never logs it or returns it. If your browser
offers to save it as a password, decline.

From a terminal instead: `go run ./cmd/mosso auth github` prints the same
link and stores the token in the Keychain with hidden prompts. By hand:
`security add-generic-password -U -s mosso -a github-token -w`.

### Optional permissions

Mosso works with the permissions above. Do **not** grant more unless you
need it:

- **Workflows** (write): only needed for Claude to change
  `.github/workflows`. Without it, GitHub rejects such pushes, which keeps
  Claude from adding or editing CI workflows that would run with your
  repository's secrets. Mosso reports this as a clear error; push workflow
  changes yourself.
- Actions, Administration, Secrets and the rest are not used by any Mosso
  feature.

A classic token with the `repo` scope also works, but it can access every
repository you can. Avoid it.

### Rotation and expiration

- To replace the token (rotation, expiry, or to add repositories), click
  **Replace** next to *GitHub* and paste a new one, or run
  `mosso auth github` again. Mosso reads the token on every request, so no
  restart is needed and existing workspaces use the new token at once.
- The sidebar shows when the token expires within two weeks (GitHub reports
  the expiration on each API response; `GET /github/auth` returns it as
  `expires_at`). When it has expired, GitHub errors say so and offer
  **Replace the GitHub token**.
- To disconnect: `security delete-generic-password -s mosso -a github-token`
  on macOS (or restart Mosso without `MOSSO_GITHUB_TOKEN`), then delete the
  token on GitHub under *Settings → Developer settings → Personal access
  tokens*.

## How it works

1. **Create** (`POST /workspaces`): Mosso checks the token and the
   repository with the GitHub API *before* creating a sandbox. It provisions a
   sandbox, clones the base branch into `/vercel/sandbox/<repo>`, creates a
   working branch `mosso/<random>`, and sets the Git identity to your GitHub
   account (your name and `<id>+<login>@users.noreply.github.com`). Meanwhile
   it fetches the base branch into a temporary bare repository on the Mosso
   server (the workspace's *mirror*). If any step fails, the sandbox is
   destroyed.
2. **Work** (`POST /workspaces/{name}/chat` or `.../claude/messages`): before
   every Claude turn, Mosso fetches the base branch and the working branch
   from GitHub into the mirror and updates the sandbox's
   `refs/remotes/origin/<base>` and `origin/<branch>` to match, sending only
   the new commits. When the base moved, the chat says so ("Fetched from
   GitHub: origin/main moved …"). Claude is told (in its system prompt) that
   it has no GitHub credentials, that `origin/*` is kept current for it, and
   that Mosso pushes on publish. Claude can edit, commit, create and switch
   branches, and rebase onto or merge `origin/<base>`, all locally.
3. **Publish** (`POST /workspaces/{name}/pull-request`):
   1. Mosso fetches from GitHub again and updates the sandbox's `origin/*`.
   2. In the sandbox, it refuses to continue while a rebase, merge,
      cherry-pick or revert is in progress or conflicts are unresolved. It
      then commits any uncommitted work (hooks and signing disabled) with the
      pull request title as the message.
   3. It brings the sandbox's new commits to the server as a Git bundle and
      checks every new object (`fetch.fsckObjects`).
   4. It refuses to push if any new file, in **any** new commit, or any
      commit message contains the GitHub or Claude token.
   5. It pushes the sandbox's `HEAD` to the working branch with
      `git push --force-with-lease=refs/heads/<branch>:<expected>`, from the
      server. Your commits are pushed as they are, history included.
   6. It opens a pull request from the working branch into the base branch,
      or returns the one already open, and the UI shows its link.

   Publishing again after more work pushes the new commits to the same pull
   request. If nothing changed since the last push, nothing is pushed.

### Rebasing an open pull request

The workflow that used to fail (the base branch advanced and the pull request
has conflicts):

1. Ask Claude, for example: *"main has moved on; rebase onto origin/main and
   resolve the conflicts."* Before the turn, Mosso has already fetched the new
   `origin/main` into the sandbox.
2. Claude runs `git rebase origin/main`, resolves conflicts and finishes the
   rebase.
3. Click **Update PR**. The branch's history was rewritten, so Mosso pushes
   with `--force-with-lease` and the chat says the pull request was updated
   with a rebased branch. The existing pull request shows the new commits.

Safety rules for pushes:

- Mosso only ever pushes `HEAD` to the workspace's own `mosso/*` branch. It
  never pushes to, force-pushes, or merges into the base branch or any other
  branch.
- A fast-forward is a normal push.
- A rewritten branch (rebase, amend, reset) is pushed only if the branch on
  GitHub is still exactly the commit Mosso last pushed from this workspace.
  If someone else pushed to it meanwhile, Mosso refuses and keeps their
  commits; it has already fetched them as `origin/<branch>` in the sandbox,
  so ask Claude to rebase onto or merge `origin/<branch>`, then publish again.
- The lease makes GitHub reject the push if the branch changed between
  Mosso's fetch and its push.
- A failed publish changes nothing on GitHub, and the sandbox keeps all its
  work (including the commit Mosso made of uncommitted changes), so you can
  fix the cause and publish again.

## How credentials reach Git operations

| Operation | Where it runs | Credential |
| --------- | ------------- | ---------- |
| Check token and repository, list repositories and branches, open pull requests | Mosso server, GitHub REST API | `Authorization` header to `api.github.com` |
| Clone a **public** repository | Sandbox | None: anonymous |
| Clone a **private** repository | Sandbox, once, before Claude or any repository code has run there | Environment of that one `git clone`, read by a command-line credential helper |
| Fetch the base and working branches | Mosso server (mirror), then a bundle into the sandbox | Environment of the server's `git` process |
| Push the working branch | Mosso server (mirror), after a bundle out of the sandbox | Environment of the server's `git` process |
| `git fetch`, `git pull`, `git push`, `gh` inside the sandbox | Sandbox | None. They fail for private repositories (fetch works anonymously for public ones). Claude is told to use `origin/*` instead. |

On the server, Git runs with `GIT_CONFIG_NOSYSTEM=1` and
`GIT_CONFIG_GLOBAL=/dev/null`, so your own Git configuration (credential
helpers such as `osxkeychain`, `url.*.insteadOf`, hooks) is not used and the
token is not stored anywhere. The credential helper is passed with
`-c credential.helper=` (which clears all others) and reads the token from
the environment: it is never in a remote URL, a command line, a config file
or a log. Bundles move through the Vercel Sandbox command API: into the
sandbox as base64 command arguments, out of it as command output.

The mirror is a temporary directory (`$TMPDIR/mosso-git-*`) per workspace,
removed when the workspace is terminated or Mosso shuts down, and recreated
on demand after a restart (the first fetch downloads the base branch's
history again). Mosso saves the commit it last pushed with the workspace,
so a rebased branch can still be published with `--force-with-lease` after a
restart. It needs `git` on the machine running Mosso (on macOS:
`xcode-select --install`).

## API

| Method & path                                | Description |
| -------------------------------------------- | ----------- |
| `GET /github/auth`                           | `{"configured": true, "source": "keychain"\|"env"\|"memory", "login": "...", "expires_at": "...", "create_url": "https://github.com/settings/personal-access-tokens/new?...", "storage": "keychain"\|"memory"}`, plus `error` if GitHub rejects the token. Never returns the token. |
| `POST /github/auth`                          | Body: `{"token": "github_pat_..."}`. Checks the token with GitHub, then stores it (Keychain, or memory without one) for all workspaces. `200` with the status above; `400` malformed; `422` GitHub rejected it; `500` the Keychain refused. |
| `POST /workspaces`                           | Body: `{"repo": "owner/repo", "base": "main", "timeout_seconds": 1800}`. `base` defaults to the repository's default branch; `timeout_seconds` defaults to 1800 (max 2700). Clones, installs Claude Code and returns `201` with the workspace. Setup continues if the client disconnects. |
| `GET /workspaces`, `GET /workspaces/{name}`  | Workspaces of this Mosso process with their `status`, including ones still being created and `pull_request` once one exists. |
| `POST /workspaces/{name}/claude/messages`    | Same body and response as `POST /sandboxes/{name}/claude/messages`, run in the checkout after syncing `origin/*`. The turn is also recorded in the workspace's chat. |
| `POST /workspaces/{name}/chat`, `GET /workspaces/{name}/events` | Asynchronous chat with streamed progress, used by the web UI; see [web-ui.md](web-ui.md#api-used-by-the-ui). |
| `POST /workspaces/{name}/pull-request`       | Body (all optional): `{"title": "...", "body": "...", "draft": false}`. `201` when it opened the pull request, `200` when it already existed. |
| `DELETE /workspaces/{name}`                  | Terminate: delete the sandbox with its snapshots, the server-side mirror and the saved history. The branch and pull request stay on GitHub. |

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

Pull request (`forced` is set when a rebased branch was pushed with
`--force-with-lease`):

```json
{"number": 12, "url": "https://github.com/you/project/pull/12", "branch": "mosso/0a1b2c3d4e5f", "commit": "…", "created": false, "forced": true}
```

Errors (`{"error": "..."}`):

- `400` invalid input; `404` unknown workspace, or the repository does not
  exist or the token cannot see it (no sandbox is created).
- `409` Claude or another publish is still running in the workspace (a
  publish finishes even if the client disconnects).
- `422` no changes to publish: the branch has no commits beyond the base and
  was never pushed.
- `429` sandbox limit reached; `503` Vercel or the GitHub token is not
  configured (or the Claude token, for messages).
- `502` the sandbox, Git or GitHub failed, with an explanation (see
  Troubleshooting) followed by Git's or GitHub's own message. Failed creates
  destroy their sandbox. A failed publish leaves the workspace usable.

Limits: 16 MiB of new commits (as a compressed bundle) per publish, 512 MiB
of uncompressed new content checked for credentials, and 64 MiB per sync.

## Troubleshooting

| Message | Cause and fix |
| ------- | ------------- |
| *GitHub is not connected* | No token yet. Connect GitHub in the UI (or `mosso auth github`, or `MOSSO_GITHUB_TOKEN`). |
| *the GitHub token is invalid, expired or revoked* | Create a new token with the link and click **Replace**. No restart needed. |
| *the GitHub token lacks access: give it this repository…* | The repository is not under the token's *Repository access*, or *Contents* / *Pull requests* is not *Read and write*. Edit the token on GitHub (*Settings → Developer settings → Fine-grained tokens*) or create a new one. Organizations may need to approve the token. |
| *repository … was not found, or the GitHub token cannot access it* | Wrong `owner/repo`, or the token does not include the repository (GitHub answers 404 for private repositories a token cannot see). |
| *GitHub refused the push because it changes files in .github/workflows* | Expected without the Workflows permission. Ask Claude to drop those changes, or push them yourself. |
| *the working branch on GitHub has commits that are not in the workspace* | Someone pushed to the `mosso/*` branch. Mosso fetched it as `origin/<branch>`; ask Claude to rebase onto or merge it, then publish again. |
| *the branch changed on GitHub while Mosso was pushing* | Another push won the race; publish again. |
| *a rebase, merge, cherry-pick or revert is in progress* / *unresolved merge conflicts* | Ask Claude to finish (or abort) it, then publish. |
| *a branch protection rule or ruleset rejected the push* | A ruleset covers `mosso/*` branches; adjust it or exempt them. |
| *refusing to push …: it contains one of Mosso's credentials* | A token is in a file or commit message, possibly in an earlier commit. Ask Claude to remove it from history (for example by rebasing), and revoke and rotate the credential if it may have left the sandbox. |
| *git is not installed on the Mosso server* | Install Git on the machine running Mosso (`xcode-select --install` on macOS). |
| *Could not fetch the latest changes from GitHub* (chat notice) | The pre-turn sync failed (often one of the token errors above). Claude still runs with what the sandbox has; publishing retries the fetch. |
| Claude says `git push` or `git fetch` failed | Expected: the sandbox has no credentials. Click **Create PR** / **Update PR** instead; `origin/*` is refreshed before every turn. |

## Try it

```bash
curl -s localhost:8080/github/auth | jq

WS=$(curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces \
  -d '{"repo":"you/project"}' | tee /dev/stderr | jq -r .name)

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/claude/messages \
  -d '{"prompt":"Fix the typo in README.md and run the tests."}' | jq .result

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/pull-request \
  -d '{"title":"Fix README typo","draft":true}' | jq .url

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/claude/messages \
  -d '{"prompt":"Rebase onto origin/main and resolve any conflicts."}' | jq .result

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/workspaces/$WS/pull-request \
  -d '{"title":"Rebase onto main"}' | jq '{url, forced}'

curl -s -X DELETE localhost:8080/workspaces/$WS
```

## Tests

`go test ./...` covers this with fakes only: no real network or accounts.

- `internal/github` runs the real Git scripts and the real server-side Git
  on your machine, against a local bare repository served over
  authenticated smart HTTP (`git http-backend`, pushes always need the
  token, like GitHub) and backed by a fake GitHub API. It covers private and
  public clones, syncing `origin/*` into the sandbox without the token,
  pushing new and updated branches, a rebase with conflicts pushed with
  `--force-with-lease`, refusing to overwrite someone else's commits, the
  lease rejecting a branch that moved, refusing credentials anywhere in the
  new commits, permission, expired-token and workflow errors, and cleanup.
  These tests skip if `git` or `git-http-backend` is missing.
- `internal/server` covers the HTTP endpoints with the Git operations faked:
  connecting, replacing and reporting the token (with and without a
  Keychain, never echoing or logging it), syncing before each turn, publish
  results and failures, cleanup on failure and termination, and that the
  token enters the sandbox only for a private clone.

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
- The test adds `mosso-e2e/<id>.md` on a `mosso/<id>` branch, publishes,
  then has Claude amend its commit and publishes again, which exercises the
  `--force-with-lease` path. It never merges or writes to the base branch.
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

- **The token stays on the machine running Mosso** (Keychain, memory, or the
  server's environment). Mosso reads it per request and sends it only to
  GitHub: in the `Authorization` header to `api.github.com`, and to the
  server's own `git` processes through their environment. It never appears in
  logs, API responses, Git remote URLs, command arguments, `.git/config` or
  the browser's storage, and Mosso redacts it from GitHub and Git error
  messages.
- **Fetches and pushes run on the Mosso server**, so Claude, its child
  processes, hooks and repository code in the sandbox never see the GitHub
  token, and cannot push, open pull requests or reach other repositories by
  themselves. Only Mosso pushes, when you publish, and only to the
  workspace's `mosso/*` branch.
- **Public repositories are cloned anonymously**, so the token never enters
  their sandbox.
- **Private repositories need the token for the initial clone.** It is in the
  environment of that one `git clone` command only, delivered through the
  Vercel API request body, read by a command-line credential helper (after
  an empty `credential.helper` clears any other), and stored nowhere. The
  clone runs in a fresh sandbox before Claude or any repository code has
  run there. Environment variables do not protect secrets from code in the
  sandbox, but nothing untrusted is running then. Vercel, which operates the
  sandbox, can see it like any value sent to the sandbox.
- **Commits come from the sandbox.** Mosso pushes the commits Claude made, as
  they are, after checking their objects with `fsck` and refusing any that
  contain the GitHub or Claude token. That check catches only the exact
  values, not an encoded copy, and cannot stop network exfiltration.
- **The Claude token is still in the sandbox** while Claude runs (see
  [claude-code.md](claude-code.md#security-model-and-limitations)). A prompt
  injection from repository content can read it and send it out. Only point
  Claude at repositories you trust.
- **The token's scope is the real boundary.** Anyone who obtains it (for
  example from the Mac) can do what it allows: read and write the contents
  and pull requests of the selected repositories. Select as few
  repositories as practical and keep the expiration short.
- **Pull requests come from branches in the same repository**, so your
  existing CI runs Claude's code with the secrets those workflows can use (as
  for any collaborator's branch, unlike pull requests from forks). Keep
  sensitive secrets out of workflows that run on `pull_request`, or require
  approval for them. Without the Workflows permission, Claude cannot change
  the workflows themselves.
- **Everything Claude commits is published**, except files ignored by
  `.gitignore`. Review the pull request before merging, as for any
  contributor.
- Commits are authored with your GitHub account's name and noreply address.
- Workspaces are saved in Mosso's data directory and their sandboxes
  persist across restarts (see [worker.md](worker.md)); mirrors are
  temporary directories recreated as needed. If Mosso is killed without
  shutting down, `$TMPDIR/mosso-git-*` directories may be left behind; they
  are safe to delete. Publishing resumes a stopped sandbox first.
