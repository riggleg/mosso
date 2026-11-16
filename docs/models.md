# Claude models

Each workspace runs Claude Code with one Claude model, chosen when the
workspace is created and kept for its whole life. A default model, set in
the web UI and saved in Mosso's database, is preselected for new workspaces
(a [section](sections.md) can name its own).

## Use it

- **Default model.** The sidebar footer shows *Model* with the current
  default. Click **Change**, pick a model in *Settings for new workspaces*
  and save (the same dialog has the sandbox image and setup script; see
  [sandbox-environment.md](sandbox-environment.md)). The choice is saved in
  the SQLite database (see [history.md](history.md)), so it survives
  restarts; no environment variable is involved. Until you choose one, the
  default is Claude Opus 5.5. Changing it does not affect existing
  workspaces.
- **New workspace.** The **+ New** dialog has a *Model* menu with the
  default preselected; pick another for this workspace if you like.
- **In the workspace.** The header shows the workspace's model next to its
  branch. Workspaces created before models could be chosen show *Claude Code
  default*: Mosso passes no model for them, as before.

## Models

| Model | ID passed to `claude --model` |
| --- | --- |
| Claude Fable 5.2 | `claude-fable-5-2` |
| Claude Opus 5.5 (built-in default) | `claude-opus-5-5` |
| Claude Sonnet 5.5 | `claude-sonnet-5-5` |
| Claude Haiku 5.5 | `claude-haiku-5-5` |

The list is `Models` in `internal/claude/models.go`; edit it when Claude
Code gains a model. Which models your Claude subscription can use depends
on its plan: if your plan does not include the chosen model, Claude Code
reports an error in the chat on the first message.

If a saved default is no longer in the list (after an upgrade that removed
it), Mosso logs it and falls back to the built-in default. A workspace
keeps running the model it was created with even if that model is no
longer offered for new workspaces.

## How it works

- The workspace's model is saved in the `model` column of the `workspaces`
  table; the default model in the `settings` table under `default_model`.
  Both arrived with schema migration 2.
- Every turn passes the model to Claude Code: the worker runs
  `claude -p … --model <id>`, on the first message and on every follow-up
  that resumes the session (`--resume`), since Claude Code does not keep the
  model in the session.
- The server accepts only models in the list; the worker additionally checks
  that the ID is well formed before it reaches the command line.

## API

| Method & path | Description |
| --- | --- |
| `GET /settings` | `{"default_model": "claude-opus-5-5", "models": [{"id": "…", "name": "…"}], …}`, plus the sandbox environment defaults (see [sandbox-environment.md](sandbox-environment.md#api)). Works without Vercel credentials. |
| `POST /settings` | Body: `{"default_model": "<id>"}`, optionally with the sandbox environment defaults; settings left out are unchanged. Saves them and returns the settings. `400` if the model is not in the list; `500` if they could not be saved (nothing is then changed). |
| `POST /workspaces` | Accepts `"model": "<id>"`; omitted, the workspace gets the current default. `400` if the model is not in the list. |
| `GET /workspaces`, `GET /workspaces/{name}` | Each workspace has `model` (absent for workspaces from before model selection). |
