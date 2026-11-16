# Mosso

Mosso is an experimental, self-hosted coding-agent orchestration project.

The initial Minimum Usable Product (MUP) will run Pi in a Vercel Sandbox, offer an interactive chat through a local Go server, and allow an agent to create GitHub pull requests.

> Early development. The first end-to-end workflow (repository → Claude Code → pull request) works in a local web UI and through the local API. Workspaces are saved on your machine and in persistent sandboxes, so you can stop Mosso while Claude works and pick the conversation up later.

**Quick start:** configure Vercel, Claude Code and GitHub (see below), run `make run`, and open <http://127.0.0.1:8080>. Create a workspace from a GitHub repository (optionally with Claude's first task, which starts as soon as the workspace is ready), chat with Claude Code while its tool activity streams in, and publish the result as a pull request. See [docs/web-ui.md](docs/web-ui.md).

**Toolchain:** [`.tool-versions`](.tool-versions) pins Go (build and tests) and Node.js (only for the web UI tests; Mosso itself has no Node.js dependency). With [asdf](https://asdf-vm.com), run `asdf plugin add golang && asdf plugin add nodejs && asdf install`; [mise](https://mise.jdx.dev) reads the same file with `mise install`.

Claude Code runs under a small **Mosso worker inside the sandbox**, which records everything to a durable event log there. Stop Mosso or close the laptop while Claude is working: the worker keeps going until Claude finishes or the sandbox's 45-minute session ends. Start Mosso again and it reconnects to the workspace, fetches what it missed and shows the whole conversation. A sandbox whose session ended resumes from its snapshot on your next message. Only **Terminate** deletes a workspace's sandbox. See [docs/worker.md](docs/worker.md) for the lifecycle, what survives what, the durability caveats and the live acceptance test.

Workspaces and their conversations are saved in a local SQLite database. A terminated workspace stays readable under **History** in the sidebar and can be archived. See [docs/history.md](docs/history.md) for the database path (`MOSSO_DB_PATH`), schema and migrations.

Mosso can already provision a Vercel Sandbox, run a shell command in it, and destroy it; see [docs/vercel-sandbox.md](docs/vercel-sandbox.md) for setup.

It can also run Claude Code headlessly in a sandbox with your Claude Pro/Max subscription: authenticate once with `mosso auth claude` (stored in the macOS Keychain), send a prompt, and continue the conversation by session ID. See [docs/claude-code.md](docs/claude-code.md) for setup, the API, and subscription and security limitations. Building the worker for the sandbox needs Go (`make run` and `make build` cross-compile and embed it).

It can work on a GitHub repository. **Connect GitHub once** from the web UI: Mosso opens GitHub's token form with the right permissions preselected (Contents and Pull requests: Read and write, 90-day expiration), you pick the repositories, and paste the fine-grained token back into Mosso. It is stored in the macOS Keychain (in memory on Linux, or set `MOSSO_GITHUB_TOKEN`) and used by every workspace; you can replace it at any time without a restart. A workspace clones the repository onto a new `mosso/*` branch in a sandbox. Claude Code works there, and Mosso keeps `origin/*` fresh before every turn so Claude can rebase. Mosso then pushes the branch and opens or updates the pull request, using `--force-with-lease` for rebased branches. Fetches and pushes run on the Mosso server, so the sandbox never holds GitHub credentials and nobody configures GitHub inside a sandbox. The server needs `git` installed. See [docs/github.md](docs/github.md) for setup, permissions, the security model and troubleshooting.

Each workspace runs Claude Code with the Claude model you pick when creating it (Fable, Opus, Sonnet or Haiku); the default model is set in the web UI and saved in the database. See [docs/models.md](docs/models.md).

New workspaces can get a **custom sandbox environment**: a Bash **setup script** that runs once after the clone (install system packages, mise, languages and tools), and/or a **custom image** from Vercel Container Registry to boot the sandbox from. Both have defaults in the web UI's settings and can be changed per workspace. See [docs/sandbox-environment.md](docs/sandbox-environment.md).

Workspaces are grouped into **sections** in the sidebar, Conductor-style: a section (say *Perx API*) has a repository, base branch, model, sandbox environment and **agent instructions** that new workspaces in it start from, and that you can change per workspace. The instructions are added to Claude's system prompt on every turn. Each workspace keeps the configuration it was created with, and can be moved between sections. See [docs/sections.md](docs/sections.md), which also describes what Claude can and cannot do on GitHub (it cannot push or open pull requests itself).

Chat messages can carry screenshots and files: paste with Cmd+V, drag them in or use the paperclip; Claude sees images visually. See [docs/attachments.md](docs/attachments.md) for how it works and the browser clipboard limitations.

See [docs/mup.md](docs/mup.md) for the first milestone and [SECURITY.md](SECURITY.md) for credential handling rules. Pull requests are reviewed automatically by Claude Code; see [docs/claude-review.md](docs/claude-review.md) for setup.
