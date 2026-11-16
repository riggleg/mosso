# Workspace sections and agent instructions

A **section** is a named group of workspaces with the configuration new
workspaces in it start from, like the repository groups in Conductor's
sidebar. For example:

| Section | Repository | Base | Model | Environment | Agent instructions |
| --- | --- | --- | --- | --- | --- |
| Mosso | `riggleg/mosso` | `main` | Opus | Go via mise | Run `go vet` and `go test ./...` before finishing. |
| Perx API | the Perx API repository | `master` | Sonnet | Ruby image | Follow the conventions, run the relevant tests, prepare the work for pull requests into `master` and `uat`. |
| Perx API Layer 6 | the same repository | `layer6` | Fable | another setup script | Only touch the Layer 6 module. |

Several sections may use the same repository. Sections are kept in Mosso's
SQLite database next to the workspaces (see [history.md](history.md)).

## Use it

- **Create a section**: **+ New section** in the sidebar. Only the name is
  required; the repository, base branch, model, sandbox image, setup script
  and agent instructions are optional. A new section's image and setup
  script are prefilled with the defaults from Settings.
- **Edit, rename, duplicate or delete**: hover a section's header in the
  sidebar and click **⋯**. *Duplicate* copies the configuration to
  "<name> copy" and opens it, to rename and adjust (for example Perx API →
  Perx API Layer 6). *Delete* moves the section's workspaces, including
  its history, to *Unsectioned*; nothing else about them changes.
- **Default section**: tick *Preselect this section for new workspaces* in
  the section dialog. **+ New** then opens the New workspace dialog with it
  selected; the sidebar marks it *default*.
- **Collapse and expand** a section by clicking its header. Each browser
  remembers which sections are collapsed (in `localStorage`); a collapsed
  section opens to show the workspace you select.
- **Create a workspace in a section**: hover the section's header and click
  **+**, or pick the section in the New workspace dialog. The dialog fills in
  the section's repository, base branch, model, image, setup script and
  agent instructions, and says under the picker what the section gives and
  which fields you changed for this workspace. Choose *No section* (or the
  **+** on *Unsectioned*) for a workspace without one, configured as before
  from the defaults in Settings. The first message works as before.
- **Move a workspace**: choose another section (or *Unsectioned*) in the
  menu in the workspace's header. It works for running, history and archived
  workspaces, and without Vercel credentials. Only where the workspace is
  listed changes: its saved configuration, sandbox, instructions and
  conversation stay as they are.

The sidebar lists every section with its running workspaces underneath,
then *Unsectioned* (workspaces in no section, including every workspace
from before sections existed), then **History** and **Archived** as before.
History and archived workspaces keep their section, which the sidebar shows
next to each one, and which is still theirs if they are unarchived. Without
any sections, the sidebar looks as it did before.

## How the configuration is chosen

A workspace's configuration is fixed when it is created. Field by field:

| Field | From the request | Else, with a section | Else, without one |
| --- | --- | --- | --- |
| `repo` | if not empty | the section's (required if the request has none) | required |
| `base` | if present; `""` = the repository's default branch | the section's; empty = default branch | default branch |
| `model` | if present; `""` = the default model | the section's; empty = the default model | the default model |
| `image` | if present; `""` = built-in runtime | the section's; empty = built-in runtime | the default image |
| `setup_script` | if present; `""` = none | the section's; empty = none | the default setup script |
| `instructions` | if present; `""` = none | the section's | none |

The web UI always sends what the dialog shows, so what you see is what the
workspace gets. A section's empty image or setup script means none, not the
defaults from Settings; a section's empty model means the default model at
the time the workspace is created.

The workspace saves the result (`model`, `image`, `setup_script` and
`instructions` columns, plus `section_id`). Editing or deleting the section
later never changes an existing workspace, and moving the workspace never
gives it the new section's configuration.

## Agent instructions

A workspace's instructions are appended to Claude Code's system prompt
(`--append-system-prompt`) on **every turn**: the first message, follow-ups,
turns that resume the Claude Code session, and turns after Mosso restarts,
because they are read from the workspace's saved configuration each time.
They come after Mosso's own workspace instructions (no GitHub credentials,
`origin/*` is fetched for you, Mosso pushes on publish), inside a
`<workspace-instructions>` block, and are framed so that:

- they describe *how* to work; the user's messages are the task and come
  first;
- they do **not** override Mosso's rules: if they ask for something the
  sandbox cannot do (push, open pull requests, use GitHub credentials),
  Claude does the parts it can and says in its reply what is left for you.

The instructions are XML-escaped inside the block (`&`, `<`, `>`, quotes,
tabs and carriage returns become entity references; newlines stay as they
are), so text such as `</workspace-instructions>` cannot appear to close or
reopen it. The delimiters only organize the prompt; they are not a security
boundary: Mosso's restrictions are enforced in code, not by the prompt.

The header's **Instructions** badge shows a workspace's instructions on
hover. They are limited to 16 KiB (they travel as one command-line
argument), trimmed, and stored as given: like the rest of the prompt they
are text you write, not secrets; do not put tokens in them.

## GitHub capabilities: what the agent can do

Mosso keeps GitHub credentials on the Mosso server (Keychain, memory or
`MOSSO_GITHUB_TOKEN`); see [github.md](github.md#security-model-and-limitations).
The sandbox, and so Claude, never has a GitHub token. Instead Claude has
Mosso's GitHub tools, which Mosso runs on its server for the workspace's
repository only; see [agent-github.md](agent-github.md).

| Capability | |
| --- | --- |
| Read the repository, edit files, run tests | Yes, in the sandbox checkout. |
| Commit, create and switch local branches, rebase, merge, cherry-pick | Yes, locally. |
| See the latest base branch | Yes: before each turn Mosso fetches the workspace's **base** and **working** branches, and every branch Claude fetched or pushed, into `origin/<branch>`. |
| See other branches (for example `uat` when the base is `master`) | Yes, with `github_fetch`; they are then kept current before each turn. |
| Push | Yes, with `github_push`: committed work, fast-forward only, to the workspace's own branches (`mosso/<id>` and `mosso/<id>-<suffix>`). |
| Open, read and update pull requests | Yes, any number, into any base branch of the repository, from the workspace's own branches (`github_create_pr`, `github_list_prs`, `github_get_pr`, `github_update_pr`). |
| CI results | Yes, with `github_checks`. |
| Force-push, delete branches, merge or close pull requests, push to other branches, other repositories, `gh` | No. |

So an instruction such as *"create pull requests targeting both master and
uat"* is carried out by Claude in its turn: it pushes the working branch and
opens a pull request into `master`, fetches `uat`, ports the change onto a
local branch from `origin/uat` (handling where the branches differ), pushes
that to `mosso/<id>-uat` and opens a pull request into `uat`. Mosso does not
know the instruction asked for two: the instructions say what to do, the
tools are the same for one pull request or five. **Create PR** still
publishes the working branch into the base branch, and is the same pull
request as the one Claude opened from it.

## API

Sections only need the database, so they work without Vercel credentials,
like the history.

| Method & path | Description |
| --- | --- |
| `GET /sections` | `{"sections": [{"id", "name", "repo", "base", "model", "image", "setup_script", "instructions", "created_at", "updated_at"}]}`, by name. |
| `POST /sections` | Body: `{"name": "Perx API", "repo": "owner/repo", "base": "master", "model": "claude-sonnet-5-5", "image": "…", "setup_script": "…", "instructions": "…"}`; only `name` is required. `201` with the section; `400` if a field is invalid; `409` if another section has the name (ignoring case). |
| `PATCH /sections/{id}` | Changes the fields given, for example `{"name": "Perx"}` to rename. `200` with the section; `404` unknown section. Existing workspaces are not affected. |
| `POST /sections/{id}/duplicate` | Body: `{}` or `{"name": "…"}`. Copies the configuration under the name, or "<name> copy" ("<name> copy 2", …). `201` with the copy. |
| `DELETE /sections/{id}` | `204`. Its workspaces become unsectioned; if it was the default section, there is none. |
| `POST /workspaces/{name}/section` | Body: `{"section_id": 3}`, or `0` for none. Moves the workspace; `200` with the workspace. `400` unknown section; `409` while the workspace is being set up; `500` if the change could not be saved (it is then not made). |
| `GET /settings`, `POST /settings` | Also `default_section_id`: the section the New workspace dialog preselects, `0` for none. `400` if the section does not exist. |
| `POST /workspaces` | Also `section_id` and `instructions`; see [How the configuration is chosen](#how-the-configuration-is-chosen). `400` if the section does not exist, has no repository and the request names none, or its model is no longer offered (choose another model). The API never applies the default section by itself: a request without `section_id` creates an unsectioned workspace, as before. |

Workspaces (`GET /workspaces`, `GET /workspaces/{name}`, the event stream)
include `section_id` (omitted for none) and `instructions` (omitted for
none). `PATCH` requests need `Content-Type: application/json`, like `POST`.

## Storage

Migration 4 adds the `sections` table and two columns to `workspaces`
(see [history.md](history.md#schema)):

- `section_id INTEGER REFERENCES sections (id) ON DELETE SET NULL`: NULL
  for unsectioned, which every existing workspace is after the migration.
  It is the only one of these columns a save updates.
- `instructions TEXT NOT NULL DEFAULT ''`: written once, when the workspace
  is created.

The default section is the `default_section` setting. Section names are
unique, ignoring case.

## Tests

`go test ./internal/server -run 'Section|Instructions|MoveWorkspace'`
covers section CRUD and validation (`TestSectionCRUD`, `TestStoreSections`),
the default section (`TestDefaultSection`), inheritance
(`TestWorkspaceInheritsSection`: the sandbox image, setup script, model and
instructions actually used), overrides (`TestWorkspaceOverridesSection`),
persistence across section edits, moves, restarts with a resumed Claude
session, and section deletion (`TestSectionChangesDoNotAffectWorkspaces`),
moving running, history and archived workspaces, with and without Vercel
(`TestMoveWorkspace`, `TestMoveWorkspaceSaveFailure`), the migration of
existing databases (`TestSectionsMigrationKeepsOldWorkspaces`) and the
system prompt (`TestInstructionsPrompt`).

`node --test internal/server/webtest/*.test.js` covers the web UI's grouping and
dialog rules (`sections.test.js`). To look at the UI with sample sections:
`MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=sections go test
./internal/server -run TestServeFakeForBrowser -timeout 1h`.
