# Security

Mosso runs coding agents capable of executing commands and reading repositories.
**Treat each agent sandbox as untrusted compute.** The initial MUP is experimental
and is not suitable for internet-facing deployment.

## Reporting vulnerabilities

Do not post credentials, exploit payloads containing live secrets, or sensitive
data in public issues. Contact the repository owner privately before disclosure.

## Credential rules

- Never commit actual credentials, `.env` files, private keys, or session recordings
  that might contain tokens.
- Inject credentials only at runtime, preferably with short-lived, scoped tokens.
- Do not place tokens in prompts, command arguments, git remotes, or URLs.
- Keep GitHub access scoped to the target repository and the permissions needed
  to create branches and pull requests.
- Avoid logging environment variables, request bodies containing secrets, agent
  tool input/output containing secrets, or process command lines containing secrets.
- Remove credentials and destroy the sandbox when the session ends.
- The Claude Code subscription token is stored in the macOS Keychain and passed
  to a sandbox only in the environment of the Claude Code process. Code running
  in that sandbox can still read it, so only run Claude on trusted content; see
  [docs/claude-code.md](docs/claude-code.md#security-model-and-limitations).
- The GitHub token (a fine-grained personal access token limited to the target
  repositories, with Contents and Pull requests write access) is stored in the
  macOS Keychain, or in memory or `MOSSO_GITHUB_TOKEN` without one. It can be
  pasted into the local web UI (a password field, sent once over loopback and
  never stored by the browser). The Mosso server uses it for the GitHub API
  and for its own `git` fetches and pushes, which exchange commits with the
  sandbox as bundles. It enters a sandbox only in the environment of the
  initial `git clone` of a private repository, before any agent or repository
  code runs there; see [docs/github.md](docs/github.md#security-model-and-limitations).
- If a credential is exposed, **revoke or rotate it immediately**. Removing it
  from a subsequent commit does not make the exposure safe.

## CI secret scanning

Gitleaks runs on pull requests and pushes to `main` to catch common secret
patterns. It can miss secrets, and the GitHub history may already contain the
value by the time CI executes. Local pre-commit scanning and careful credential
handling are still necessary. AI code review is not a substitute for scanning.

## Initial local server

The MUP server binds to `127.0.0.1` by default and has no authentication.
Mosso refuses to start on a non-loopback address, and rejects requests with a
non-loopback `Host` or `Origin` header or a non-JSON `POST` body, to block
browser-based CSRF and DNS rebinding. Do not work around these checks until
access control is implemented.
