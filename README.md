# Mosso

Mosso is an experimental, self-hosted coding-agent orchestration project.

The initial Minimum Usable Product (MUP) will run Pi in a Vercel Sandbox, offer an interactive chat through a local Go server, and allow an agent to create GitHub pull requests.

> Early development. The first end-to-end workflow (repository → Claude Code → pull request) works in a local web UI and through the local API. Workspaces and conversations are kept in memory only.

**Quick start:** configure Vercel, Claude Code and GitHub (see below), run `make run`, and open <http://127.0.0.1:8080>. Create a workspace from a GitHub repository, chat with Claude Code while its tool activity streams in, and publish the result as a pull request. See [docs/web-ui.md](docs/web-ui.md), including its limitations: workspace state and conversations are lost when Mosso stops or a sandbox is terminated or expires.

Mosso can already provision a Vercel Sandbox, run a shell command in it, and destroy it; see [docs/vercel-sandbox.md](docs/vercel-sandbox.md) for setup.

It can also run Claude Code headlessly in a sandbox with your Claude Pro/Max subscription: authenticate once with `mosso auth claude` (stored in the macOS Keychain), send a prompt, and continue the conversation by session ID. See [docs/claude-code.md](docs/claude-code.md) for setup, the API, and subscription and security limitations.

It can work on a GitHub repository. **Connect GitHub once** from the web UI: Mosso opens GitHub's token form with the right permissions preselected (Contents and Pull requests: Read and write, 90-day expiration), you pick the repositories, and paste the fine-grained token back into Mosso. It is stored in the macOS Keychain (in memory on Linux, or set `MOSSO_GITHUB_TOKEN`) and used by every workspace; you can replace it at any time without a restart. A workspace clones the repository onto a new `mosso/*` branch in a sandbox. Claude Code works there, and Mosso keeps `origin/*` fresh before every turn so Claude can rebase. Mosso then pushes the branch and opens or updates the pull request, using `--force-with-lease` for rebased branches. Fetches and pushes run on the Mosso server, so the sandbox never holds GitHub credentials and nobody configures GitHub inside a sandbox. The server needs `git` installed. See [docs/github.md](docs/github.md) for setup, permissions, the security model and troubleshooting.

See [docs/mup.md](docs/mup.md) for the first milestone and [SECURITY.md](SECURITY.md) for credential handling rules. Pull requests are reviewed automatically by Claude Code; see [docs/claude-review.md](docs/claude-review.md) for setup.
