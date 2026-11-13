# Mosso

Mosso is an experimental, self-hosted coding-agent orchestration project.

The initial Minimum Usable Product (MUP) will run Pi in a Vercel Sandbox, offer an interactive chat through a local Go server, and allow an agent to create GitHub pull requests.

> Early development. The first end-to-end workflow (repository → Claude Code → pull request) works in a local web UI and through the local API. Workspaces and conversations are kept in memory only.

**Quick start:** configure Vercel, Claude Code and GitHub (see below), run `make run`, and open <http://127.0.0.1:8080>. Create a workspace from a GitHub repository, chat with Claude Code while its tool activity streams in, and publish the result as a pull request. See [docs/web-ui.md](docs/web-ui.md), including its limitations: workspace state and conversations are lost when Mosso stops or a sandbox is terminated or expires.

Mosso can already provision a Vercel Sandbox, run a shell command in it, and destroy it; see [docs/vercel-sandbox.md](docs/vercel-sandbox.md) for setup.

It can also run Claude Code headlessly in a sandbox with your Claude Pro/Max subscription: authenticate once with `mosso auth claude` (stored in the macOS Keychain), send a prompt, and continue the conversation by session ID. See [docs/claude-code.md](docs/claude-code.md) for setup, the API, and subscription and security limitations.

It can work on a GitHub repository: authenticate once with `mosso auth github` (a fine-grained personal access token, stored in the macOS Keychain), create a workspace that clones the repository onto a new branch in a sandbox, send Claude coding tasks there, and open a pull request with the result. See [docs/github.md](docs/github.md).

See [docs/mup.md](docs/mup.md) for the first milestone and [SECURITY.md](SECURITY.md) for credential handling rules. Pull requests are reviewed automatically by Claude Code; see [docs/claude-review.md](docs/claude-review.md) for setup.
