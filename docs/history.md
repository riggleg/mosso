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
    sandbox_id  TEXT NOT NULL,    -- the Vercel sandbox's name
    status      TEXT NOT NULL,    -- running, stopped, gone or terminated
    state       TEXT NOT NULL,    -- JSON, see below
    created_at  TEXT NOT NULL,    -- UTC, 2006-01-02T15:04:05.000000Z
    updated_at  TEXT NOT NULL,
    archived_at TEXT              -- set while the workspace is archived
);
CREATE TABLE chat_entries (
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    idx          INTEGER NOT NULL, -- position in the conversation, from 0
    entry        TEXT NOT NULL,    -- JSON, as the web UI shows it
    PRIMARY KEY (workspace_id, idx)
) WITHOUT ROWID;
```

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

On start Mosso opens the database, applies migrations and loads every
workspace:

- **Terminated** workspaces are listed at once. They have no sandbox, so
  Vercel is not asked about them, and their conversation is only read from
  the database when you open one.
- **The others** are reconciled with Vercel in the background, as described
  in [worker.md](worker.md#lifecycle): running sandboxes are followed again,
  stopped ones stay stopped (or are resumed to catch up on an interrupted
  task), and ones that no longer exist become `gone`.

### Workspaces saved by earlier versions

Before the database, Mosso saved workspaces as files in
`workspaces/<name>/` in the data directory. On start, Mosso imports any it
finds **next to the database file** (each workspace and its chat in one
transaction, skipping ones already imported), so their sandboxes are not
forgotten and left to bill. It then renames the directory to
`workspaces.imported`, which it never reads again; delete it once you are
happy. If you put the database somewhere other than the data directory,
move the `workspaces` directory next to it for the first start.

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

To look at the History and Archived sections in a browser without any
accounts:

```bash
MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=history \
  go test ./internal/server -run TestServeFakeForBrowser -timeout 1h
```
