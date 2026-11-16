# Workspace history and the database

Mosso saves every workspace and its conversation in a **SQLite database**.
The history outlives Mosso restarts and the workspace's sandbox: after you
terminate a workspace, its conversation stays readable in the web UI, and
you can archive it to move it out of the way.

The code is `internal/server/store.go` (the only code that talks SQL),
`internal/server/migrations.go` (the schema) and
`internal/server/legacy.go` (the one-time import from the files earlier
versions wrote). The rest of the server works with workspace records and
chat entries and does not know how they are stored.

## What you see

The sidebar lists workspaces in three groups:

- **Workspaces with a sandbox** (running or stopped), as before.
- **History**: terminated workspaces, newest first. Opening one replays its
  conversation; it is read-only (no composer, no publishing).
- **Archived**: collapsed by default. An archived workspace is still
  readable, and **Unarchive** moves it back into the history.

**Terminate** deletes the sandbox and its snapshots, as it always did, and
ends a running turn. What changed is that the workspace record and the
conversation are kept: the workspace moves to the History instead of
disappearing. The branch and pull request stay on GitHub, as before.
Attachment descriptions are kept; the files themselves lived in the sandbox
and are gone with it.

**Archive** is only offered for terminated workspaces. A workspace that
still has a sandbox must be terminated first, so that nothing that costs
money can be hidden in the archive.

A workspace whose setup fails (or that is destroyed while it is being set
up) never worked, and leaves no history.

### Without Vercel credentials

The history only needs the database. If Mosso starts without
`VERCEL_TOKEN`, `VERCEL_TEAM_ID` and `VERCEL_PROJECT_ID`, it still loads
every saved workspace, and you can list them, read their conversations and
archive or unarchive terminated ones: `GET /workspaces`,
`GET /workspaces/{name}`, `GET /workspaces/{name}/events` and
`POST /workspaces/{name}/archive|unarchive` work as usual.

Workspaces that still have a sandbox are shown as Mosso last saved them
and are read-only: they are not reconnected, so their status is not
checked against Vercel, and creating, messaging, resuming, publishing and
terminating answer `503`. The web UI says so (the Vercel check in the
sidebar reads "not configured · read-only", from `GET /vercel/auth`) and
disables those controls. Nothing is changed in the database, so the next
start with credentials reconnects them as usual.

### If the database cannot be written

Creating, terminating and archiving do not report success unless the
result is saved:

- **Create.** The workspace record and the chat from setup are saved in
  one transaction before the workspace is reported as created. If that
  fails, the request answers `500`, nothing is saved, and the new sandbox
  is destroyed: otherwise it would keep running where a restarted Mosso
  would not know about it.
- **Terminate.** The sandbox is deleted first, so nothing is left running
  whatever happens next. If the termination then cannot be saved, the
  request answers `500` and says that the sandbox is deleted but the result
  is not saved. The workspace shows as terminated and `unsaved`, with a
  **Save again** button; `DELETE /workspaces/{name}` again saves it without
  touching Vercel. Mosso also tries once more when it shuts down. If none
  of that succeeds, the next start finds no sandbox and shows the workspace
  as gone, with its conversation.
- **Archive, unarchive.** If the change cannot be saved, the request
  answers `500` and the workspace stays as it was.

Other saves happen in the background, with nobody waiting on them (chat
entries as Claude works, a session ending). A failure there is logged and
the workspace is marked `unsaved` (the web UI shows a warning) until a
later save succeeds. Nothing is dropped meanwhile: every save writes all
the chat entries that are not in the database yet, in order.

There is no button to delete a workspace's history. To remove one, stop
Mosso and delete its rows (the name is in the URL after `#`):

```bash
sqlite3 "$HOME/Library/Application Support/mosso/mosso.db" \
  "DELETE FROM chat_entries WHERE workspace_id = 'mosso-…'; DELETE FROM workspaces WHERE id = 'mosso-…';"
```

## Where the database is

| Setting | Meaning |
| --- | --- |
| `-db <path>` or `MOSSO_DB_PATH` | The database file. |
| `MOSSO_DATA_DIR` | Mosso's data directory; without `MOSSO_DB_PATH` the database is `mosso.db` in it. Defaults to `~/Library/Application Support/mosso` on macOS, `$XDG_CONFIG_HOME/mosso` or `~/.config/mosso` elsewhere. |

The directory is created if needed (mode 0700) and the database is created
readable by its owner only (0600): it holds your prompts and Claude's
output. It holds **no credentials**.

Next to `mosso.db` SQLite keeps `mosso.db-wal` and `mosso.db-shm` while
Mosso runs (write-ahead logging); back up or move the three together, or
stop Mosso first, after which the `.db` file alone is complete.

**In a container** (for example Kubernetes), mount a volume and point
either setting at it: `MOSSO_DATA_DIR=/data`, or
`MOSSO_DB_PATH=/data/mosso.db`. Two constraints:

- One Mosso process per database. It is a single-writer store; run one
  replica.
- The volume must be a local or block-backed filesystem (most
  `ReadWriteOnce` volumes are). SQLite's write-ahead log does not work on
  network filesystems such as NFS.

If the database cannot be opened (unwritable path, or a schema written by a
newer Mosso), Mosso exits with the reason instead of running without
history.

## Schema

```sql
CREATE TABLE workspaces (
    id          TEXT PRIMARY KEY, -- "mosso-<12 hex>"
    repo        TEXT NOT NULL,    -- "owner/repo"
    base        TEXT NOT NULL,    -- the branch the pull request targets
    branch      TEXT NOT NULL,    -- the working branch
    model       TEXT NOT NULL DEFAULT '', -- the Claude model (migration 2); '' before model selection
    image       TEXT NOT NULL DEFAULT '', -- the sandbox's Vercel Container Registry image (migration 3); '' for the built-in runtime
    setup_script TEXT NOT NULL DEFAULT '', -- the setup script run once after the clone (migration 3); '' for none
    instructions TEXT NOT NULL DEFAULT '', -- agent instructions for every turn (migration 4); '' for none
    section_id  INTEGER REFERENCES sections (id) ON DELETE SET NULL, -- the section it is listed under (migration 4); NULL for none
    sandbox_id  TEXT NOT NULL,    -- the Vercel sandbox's name
    status      TEXT NOT NULL,    -- running, stopped, gone or terminated
    state       TEXT NOT NULL,    -- JSON, see below
    created_at  TEXT NOT NULL,    -- UTC, 2006-01-02T15:04:05.000000Z
    updated_at  TEXT NOT NULL,
    archived_at TEXT              -- set while the workspace is archived
);
CREATE TABLE chat_entries (
    id           INTEGER PRIMARY KEY, -- grows with each entry: a conversation's order
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    entry        TEXT NOT NULL        -- JSON, as the web UI shows it
);
CREATE INDEX chat_entries_workspace ON chat_entries (workspace_id, id);
CREATE TABLE settings (           -- migration 2
    key   TEXT PRIMARY KEY,       -- "default_model", "default_image", "default_setup_script", "default_section"
    value TEXT NOT NULL
);
CREATE TABLE sections (           -- migration 4
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL COLLATE NOCASE UNIQUE,
    repo         TEXT NOT NULL DEFAULT '', -- '' to choose per workspace
    base         TEXT NOT NULL DEFAULT '', -- '' for the default branch
    model        TEXT NOT NULL DEFAULT '', -- '' for the default model
    image        TEXT NOT NULL DEFAULT '', -- '' for the built-in runtime
    setup_script TEXT NOT NULL DEFAULT '', -- '' for none
    instructions TEXT NOT NULL DEFAULT '', -- '' for none
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
```

`model`, `image`, `setup_script` and `instructions` are set when the
workspace is created and never updated. `section_id` changes when the
workspace is moved to another section; deleting a section sets it to NULL
(unsectioned), and workspaces from before migration 4 have none.
`settings` holds what is configured in the web UI: the default model,
sandbox image and setup script for new workspaces, and the default
section. `sections` holds the sections new workspaces start from. See
[models.md](models.md), [sandbox-environment.md](sandbox-environment.md)
and [sections.md](sections.md).

A chat entry that cannot be read back is skipped with a log line; it never
stops a workspace from being restored.

`status` is the sandbox as Mosso last knew it:

| `status` | Meaning |
| --- | --- |
| `running` | A sandbox session is running. |
| `stopped` | The session ended; the sandbox resumes from its snapshot on the next message. |
| `gone` | The sandbox was deleted outside Mosso, or its snapshot expired. Read-only; Terminate moves it to the history. |
| `terminated` | You terminated the workspace. History. |

`state` holds what Mosso needs to continue a workspace and nobody needs to
query: the checkout directory and base commit, the sandbox's current
session, the Claude Code session ID, the last worker event applied, whether
a task was running, the pull request, the last pushed commit and the
attachment descriptions. Adding a field there needs no migration.

## Migrations

`migrations.go` is an ordered list of SQL scripts. The database records how
many have been applied in SQLite's `user_version`; opening it applies the
rest, each in its own transaction, so a failed migration changes nothing. To
change the schema, **append** a script; never edit one that has shipped. A
database with a higher version than the running Mosso knows is refused.

## Startup

On start Mosso opens the database and applies migrations. Then it does two
separate things (`Restore` in `internal/server/restore.go`):

1. **Load.** Every workspace is loaded from the database as last saved and
   listed. This needs no credentials. Terminated workspaces have no
   sandbox, and their conversation is only read from the database when you
   open one.
2. **Reconnect.** With Vercel configured, the workspaces that have a
   sandbox are reconciled with it in the background, as described in
   [worker.md](worker.md#lifecycle): running sandboxes are followed again,
   stopped ones stay stopped (or are resumed to catch up on an interrupted
   task), and ones that no longer exist become `gone`. Without Vercel this
   step is skipped; see [Without Vercel credentials](#without-vercel-credentials).

### Workspaces saved by earlier versions

Before the database, Mosso saved workspaces as files in
`workspaces/<name>/` in the data directory. On start, Mosso imports any it
finds there, wherever the database itself is (each workspace and its chat
in one transaction, skipping ones already imported), so their sandboxes are
not forgotten and left to bill. It then renames the directory to
`workspaces.imported`, which it never reads again; delete it once you are
happy.

## Dependency

The database driver is [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite),
SQLite translated to Go. It needs no C compiler and no cgo, so `go build`
and cross-compiling work as before and the binary stays self-contained. It
is Mosso's first third-party dependency (with its own indirect ones in
`go.mod`).

## Tests

`go test ./internal/server` covers the store on real database files in a
temporary directory: the round trip across a close and reopen (in a path
with a space, like the macOS default), file permissions, migrations
(applied once, a later one applied on top of existing data, a failing one
rolled back, a newer database refused), and the import of the earlier file
format. The server tests then cover the behaviour over a fake Vercel:
terminating keeps the workspace and its conversation across a restart and
frees its sandbox slot; a terminated workspace refuses messages, connects,
uploads and publishing; terminating mid-task ends the turn; archiving
(refused while a sandbox exists, kept across a restart, undone by
unarchive); and a failed setup saves nothing.

`save_failure_test.go` makes the database refuse writes (with a trigger,
like a full disk) and checks that a create fails, destroys its sandbox and
saves nothing; that a terminate reports the unsaved result, deletes the
sandbox once, and succeeds when retried; that an unsaved termination shows
as gone after a restart; that a failed archive is not shown as done; and
that chat entries whose save failed are written later, in order. It also
runs Mosso without a Vercel client over a database with a terminated
workspace and one that has a sandbox: both are listed and replayed,
archiving works, everything that needs a sandbox answers `503`, and a
later start with Vercel reconnects.

`sections_test.go` covers sections, moving workspaces between them and
migration 4 on a database with existing workspaces; see
[sections.md](sections.md#tests).

To look at the History and Archived sections in a browser without any
accounts:

```bash
MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=history \
  go test ./internal/server -run TestServeFakeForBrowser -timeout 1h
```

`MOSSO_FAKE_SCENARIO=novercel` serves the same workspaces from a Mosso
without Vercel credentials, and `MOSSO_FAKE_SCENARIO=unsaved` makes the
database refuse updates, to see a termination that cannot be saved.
