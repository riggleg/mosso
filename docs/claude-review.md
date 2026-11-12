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

The review runs on a Claude Pro/Max subscription, so it uses subscription
usage limits instead of pay-as-you-go API billing. Only one repository secret is
required:

| Secret                    | Purpose                                                  |
| ------------------------- | -------------------------------------------------------- |
| `CLAUDE_CODE_OAUTH_TOKEN` | Authenticates the action to Claude with the subscription. |

1. On your own machine, signed in to Claude Code with the subscription account,
   run `claude setup-token`. It opens a browser login and prints a long-lived
   token starting with `sk-ant-oat01-`.
2. In GitHub, open **Settings → Secrets and variables → Actions → New repository
   secret**.
3. Name it `CLAUDE_CODE_OAUTH_TOKEN`, paste the token and save.

Or with the GitHub CLI (prompts for the value, so it stays out of shell history):

```bash
gh secret set CLAUDE_CODE_OAUTH_TOKEN --repo riggleg/mosso
```

Do not put the subscription token in `ANTHROPIC_API_KEY`: that input expects a
Console API key (`sk-ant-api03-…`), which is billed per token, and the
subscription token is rejected there. If an `ANTHROPIC_API_KEY` secret exists
only for this workflow, delete it so nothing falls back to API billing.

GitHub access uses the workflow's built-in `GITHUB_TOKEN`, scoped to
`contents: read` and `pull-requests: write` for the review job. No personal
access token or extra GitHub App is needed. Comments appear from
`github-actions[bot]`.

Until `CLAUDE_CODE_OAUTH_TOKEN` is set, the job runs a single notice step and
succeeds without reviewing.

## Security notes

- Never paste the token into the workflow, a commit, an issue or a PR comment. If
  it is exposed, revoke it immediately (log out other sessions / revoke it from
  your Claude account settings) and run `claude setup-token` again; deleting it
  from a later commit is not enough.
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
