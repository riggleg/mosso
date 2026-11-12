# Mosso

Mosso is an experimental, self-hosted coding-agent orchestration project.

The initial Minimum Usable Product (MUP) will run Pi in a Vercel Sandbox, offer an interactive chat through a local Go server, and allow an agent to create GitHub pull requests.

> Early development. No usable agent workflow has been implemented yet.

Mosso can already provision a Vercel Sandbox, run a shell command in it, and destroy it; see [docs/vercel-sandbox.md](docs/vercel-sandbox.md) for setup.

See [docs/mup.md](docs/mup.md) for the first milestone and [SECURITY.md](SECURITY.md) for credential handling rules. Pull requests are reviewed automatically by Claude Code; see [docs/claude-review.md](docs/claude-review.md) for setup.
