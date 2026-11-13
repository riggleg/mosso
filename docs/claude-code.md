# Claude Code in a Vercel Sandbox

Mosso can run the official [Claude Code](https://code.claude.com) CLI headlessly
in a Vercel Sandbox, using your own Claude subscription (Pro/Max). You
authenticate once on your Mac. Every sandbox then reuses that login, and a
conversation can continue across messages by its session ID.

The code lives in `internal/claude`; the HTTP endpoints are in
`internal/server/claude.go`. To run Claude on a GitHub repository and open a
pull request, use a workspace; see [github.md](github.md).

## How it works

1. **Authentication** uses Claude Code's supported headless mechanism:
   `claude setup-token` runs the normal browser sign-in and prints a one-year
   OAuth token. Claude Code reads it from `CLAUDE_CODE_OAUTH_TOKEN`
   ([docs](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token)).
   Mosso does not read Claude Code's own credential store or extract tokens in
   any other way.
2. **Storage**: Mosso keeps the token in your macOS login Keychain (service
   `mosso`, account `claude-code-oauth-token`). It reads the token through
   `/usr/bin/security` for each message, so it is never written to disk by
   Mosso, never appears on a command line, and a rotated token takes effect
   without a restart. If there is no Keychain item (or you are not on macOS),
   Mosso falls back to `CLAUDE_CODE_OAUTH_TOKEN` in its own environment.
3. **Install**: on the first message to a sandbox, Mosso installs Claude Code
   for the sandbox user with the official native installer
   (`curl -fsSL https://claude.ai/install.sh | bash -s stable`) into
   `~/.local/bin`, unmodified. This adds some time to the first message and
   does not use the token.
4. **Run**: each message is one non-interactive run in `/vercel/sandbox` (or
   the repository checkout, for a workspace):

   ```
   claude -p --output-format stream-json --verbose --permission-mode bypassPermissions [--resume <session_id>]
   ```

   The prompt is fed on stdin. The token and prompt are passed as environment
   variables of that single command through the Vercel API request body, so they never appear
   in command arguments, URLs or logs. Auto-updates are disabled for the run.
5. **Output**: Mosso parses the stream-json events into Claude's text, tool
   calls and tool results, plus the final result and session ID. Any
   occurrence of the token in the output is replaced with `[REDACTED]` before
   it is parsed or returned.
6. **Follow-ups** pass the previous `session_id`; Mosso runs
   `claude -p --resume <session_id>`. Claude Code keeps the transcript on the
   sandbox's disk, so **a conversation only lives as long as its sandbox**.

## Setup

1. Install Claude Code on your Mac and make sure you are on a Pro or Max plan.
2. Authenticate once:

   ```bash
   go run ./cmd/mosso auth claude
   ```

   This runs `claude setup-token` (browser sign-in), then asks `security` to
   store the token. Paste the token at both hidden prompts. Clear the
   terminal scrollback afterwards (Cmd-K), because `setup-token` printed the
   token there.

   To do the same by hand:

   ```bash
   claude setup-token
   security add-generic-password -U -s mosso -a claude-code-oauth-token -w   # prompts; paste the token
   ```

   On Linux, export `CLAUDE_CODE_OAUTH_TOKEN` in the shell that starts Mosso.
   Do not put it in a committed file.
3. Configure Vercel as in [vercel-sandbox.md](vercel-sandbox.md), then
   `make run`. The log prints `Claude Code token found (keychain)`.

To rotate the token, run `mosso auth claude` again; it replaces the Keychain
item. To remove it: `security delete-generic-password -s mosso -a claude-code-oauth-token`.

## API

| Method & path                              | Description |
| ------------------------------------------ | ----------- |
| `GET /claude/auth`                         | `{"configured": true, "source": "keychain"\|"env"}` or `{"configured": false, "error": "..."}`. Never returns the token. |
| `POST /sandboxes/{name}/claude/messages`   | Body: `{"prompt": "...", "session_id": "..."}` (`session_id` optional; omit it to start a conversation). Installs Claude Code if needed, runs one turn and waits for it to finish. |

Response from `POST …/claude/messages`:

```json
{
  "session_id": "0f8fad5b-d9cb-469f-a165-70867728950e",
  "model": "claude-…",
  "result": "mosso-42",
  "is_error": false,
  "subtype": "success",
  "num_turns": 2,
  "duration_ms": 5120,
  "total_cost_usd": 0.01,
  "events": [
    {"type": "tool_use", "tool_use_id": "toolu_…", "name": "Bash", "input": {"command": "echo mosso-$((6*7))"}},
    {"type": "tool_result", "tool_use_id": "toolu_…", "content": "mosso-42"},
    {"type": "text", "text": "mosso-42"}
  ]
}
```

- `is_error: true` with `200` means Claude Code ran but the turn failed, for
  example an invalid or expired token or a usage limit; `result` has the message.
- `total_cost_usd` is Claude Code's own client-side estimate. Subscription use
  is not billed per token.
- Tool results are truncated to 16 KiB each. Events from subagents carry
  `parent_tool_use_id`.
- `400` invalid input, `404` unknown sandbox, `409` another message is still
  running in this sandbox (one at a time), `502` the sandbox or Vercel API
  failed, `503` Vercel or the Claude token is not configured.
- The request blocks until Claude finishes. If the client disconnects, Mosso
  asks Vercel to stop the run. The sandbox timeout (default 10 minutes, max
  45) bounds every run, so create the sandbox with a larger `timeout_seconds`
  for longer tasks.

## Try it

```bash
SB=$(curl -s -X POST -H 'Content-Type: application/json' localhost:8080/sandboxes -d '{"timeout_seconds":900}' | jq -r .name)

R=$(curl -s -X POST -H 'Content-Type: application/json' localhost:8080/sandboxes/$SB/claude/messages \
  -d '{"prompt":"Remember the codeword PINEAPPLE and run `uname -a` with Bash."}')
echo "$R" | jq '{result, session_id, events: [.events[] | {type, name}]}'

curl -s -X POST -H 'Content-Type: application/json' localhost:8080/sandboxes/$SB/claude/messages \
  -d "{\"prompt\":\"What was the codeword?\",\"session_id\":\"$(echo "$R" | jq -r .session_id)\"}" | jq .result

curl -s -X DELETE localhost:8080/sandboxes/$SB
```

### Acceptance test

The same flow is automated against your real accounts. It creates and destroys
one sandbox and uses a little of your subscription:

```bash
MOSSO_E2E=1 go test ./internal/server -run TestClaudeEndToEnd -v -timeout 20m
```

## Security model and limitations

**The sandbox can use your subscription.** Claude Code needs the token in
its process environment. Claude Code removes `CLAUDE_CODE_OAUTH_TOKEN` from
the environment of the Bash commands it runs, but those commands run as the same
user and can still read it from `/proc/<pid>/environ`. A prompt injection (for
example, from a repository you ask Claude to work on) could therefore read the
token and send it out over the network. Mosso redacts the exact token from
API responses, but it cannot stop exfiltration from inside the sandbox.
Treat the sandbox as holding your Claude login while it runs:

- Only point Claude at code and content you trust.
- If you suspect exposure, revoke the token and generate a new one.
  Anthropic does not document a CLI revoke command for `setup-token` tokens.
  Look for it in your claude.ai account settings and confirm the old token
  stops working, or contact Anthropic support.
- The token is subscription-wide. It cannot be scoped to one sandbox,
  repository or time window shorter than its one-year lifetime.

**Why permissions are bypassed.** Nobody can approve tool calls in a headless
run, so Mosso uses `--permission-mode bypassPermissions` and relies on the
Vercel Sandbox (a microVM) as the isolation boundary. Claude Code's stronger
options do not work there today: `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`
requires Claude Code's bubblewrap sandbox, which cannot start in Vercel
Sandbox. Sandbox commands run with ambient capabilities that bubblewrap
rejects, and even with those dropped it cannot mount `/proc` because the image
masks `/proc/kcore` and `/proc/keys`. That mode also creates empty placeholder files
(`package.json`, `.env`, …) in the working directory. Revisit this if Vercel
or Claude Code change.

**Subscription limits and terms** (a summary, not legal advice; see
Anthropic's [legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)
page):

- `setup-token` requires a Pro, Max, Team or Enterprise plan. The token only
  makes model requests; claude.ai connectors and Remote Control are
  unavailable with it.
- Every sandbox draws on **your** plan's usage limits, shared with your
  other Claude and Claude Code use. Advertised limits assume ordinary
  individual use, and parallel sandboxes consume them faster. When a limit is hit, the
  turn returns `is_error: true`.
- Subscription OAuth is meant for your own use of Claude Code. Mosso is a
  single-user, local tool: you sign in through Anthropic's own flow and run
  the unmodified CLI. Do not share a Mosso instance or the token with
  other people, and do not use it to serve other users. A multi-user version
  would need each user's own credentials (or Claude Console API keys).
- `--bare` mode does not read `CLAUDE_CODE_OAUTH_TOKEN`, so Mosso does not use
  it. An `ANTHROPIC_API_KEY` in the sandbox would take precedence over the
  token and switch to API billing; Mosso never sets one.
- Claude Code installs from `claude.ai` on every new sandbox. A failed
  download returns `502`; retry, or check the sandbox's network access.
- The transcript is lost when the sandbox stops. Restarting a conversation in
  a new sandbox is out of scope for this milestone.
