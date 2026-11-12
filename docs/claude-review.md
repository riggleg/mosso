# Automated Claude Code review

`.github/workflows/claude-review.yml` runs the official
[Claude Code GitHub Action](https://github.com/anthropics/claude-code-action)
(`anthropics/claude-code-action@v1`) on every pull request when it is opened,
updated, reopened or marked ready for review. Claude reviews the diff against
[AGENTS.md](../AGENTS.md) and [SECURITY.md](../SECURITY.md) for credential
leakage, security vulnerabilities, bugs and code quality, then posts inline
comments and a short summary comment.

AI review complements the `CI` workflow (gofmt, `go vet`, `go test`, Gitleaks);
it does not replace secret scanning.

## Configure secrets

Only one repository secret is required:

| Secret              | Purpose                              |
| ------------------- | ------------------------------------ |
| `ANTHROPIC_API_KEY` | Authenticates the action to Claude.  |

1. Create an API key in the [Anthropic Console](https://console.anthropic.com/).
   Use a dedicated key for this repository and set a spend limit on its
   workspace.
2. In GitHub, open **Settings → Secrets and variables → Actions → New repository
   secret**.
3. Name it `ANTHROPIC_API_KEY`, paste the key and save.

Or with the GitHub CLI (prompts for the value, so it stays out of shell history):

```bash
gh secret set ANTHROPIC_API_KEY --repo riggleg/mosso
```

GitHub access uses the workflow's built-in `GITHUB_TOKEN`, scoped to
`contents: read` and `pull-requests: write` for the review job. No personal
access token or extra GitHub App is needed. Comments appear from
`github-actions[bot]`.

Until `ANTHROPIC_API_KEY` is set, the job runs a single notice step and
succeeds without reviewing.

## Security notes

- Never paste the key into the workflow, a commit, an issue or a PR comment. If
  it is exposed, revoke it in the Anthropic Console immediately and create a new
  one; deleting it from a later commit is not enough.
- Only pull requests opened by `riggleg`, on events triggered by `riggleg`
  (for example their own pushes), are reviewed, to limit cost and exposure.
  A commit pushed by someone else is reviewed on `riggleg`'s next push. This
  limits cost; it is not a security boundary against users with write access,
  who could edit the workflow itself. Edit the job's `if:` condition to change
  this.
- Pull requests from forks are skipped: GitHub does not pass secrets to them, and
  the workflow intentionally does not use `pull_request_target`, which would run
  with secrets on untrusted code.
- Claude is limited to reading the checkout, reading the PR with `gh pr view` /
  `gh pr diff`, and posting comments. It cannot push or approve.
- The prompt tells Claude to treat PR content as untrusted and never to quote a
  suspected secret in a comment.

## Adjusting the review

Edit the `prompt` in the workflow to change review focus. To stop automatic
reviews temporarily, disable the **Claude Code Review** workflow from the
repository's **Actions** tab.
