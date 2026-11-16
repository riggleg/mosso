package server

import (
	"database/sql"
	"fmt"
)

// migrations are the database's schema changes, in order. The database
// records how many it has applied (PRAGMA user_version); opening it applies
// the rest. Only ever append to this list: a released migration has run on
// real databases and must not change.
var migrations = []string{
	// 1: workspaces and their conversations.
	`CREATE TABLE workspaces (
		id          TEXT PRIMARY KEY, -- "mosso-<12 hex>"
		repo        TEXT NOT NULL,    -- "owner/repo"
		base        TEXT NOT NULL,    -- the branch the pull request targets
		branch      TEXT NOT NULL,    -- the working branch
		sandbox_id  TEXT NOT NULL,    -- the Vercel sandbox's name
		status      TEXT NOT NULL,    -- the sandbox as last known: running, stopped, gone or terminated
		state       TEXT NOT NULL,    -- JSON: sandbox session, Claude session, pull request, attachments, ...
		created_at  TEXT NOT NULL,    -- UTC, like 2006-01-02T15:04:05.000000Z
		updated_at  TEXT NOT NULL,
		archived_at TEXT              -- set while the workspace is archived
	);
	CREATE TABLE chat_entries (
		id           INTEGER PRIMARY KEY, -- grows with each entry: a conversation's order
		workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
		entry        TEXT NOT NULL        -- JSON, as the web UI shows it
	);
	CREATE INDEX chat_entries_workspace ON chat_entries (workspace_id, id);`,
	// 2: each workspace's Claude model, and settings such as the default
	// model. Workspaces from before have no model: Claude Code's default.
	`ALTER TABLE workspaces ADD COLUMN model TEXT NOT NULL DEFAULT ''; -- passed to claude --model, fixed for the workspace's life
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY, -- for example "default_model"
		value TEXT NOT NULL
	);`,
	// 3: each workspace's sandbox image and setup script, for reference.
	// Workspaces from before ran the built-in runtime with no setup script.
	`ALTER TABLE workspaces ADD COLUMN image TEXT NOT NULL DEFAULT '';        -- Vercel Container Registry image; '' for the built-in runtime
	ALTER TABLE workspaces ADD COLUMN setup_script TEXT NOT NULL DEFAULT ''; -- the Bash script run once after the clone; '' for none`,
	// 4: sections, named groups of workspaces with the configuration new
	// workspaces in them start from, and each workspace's section and
	// agent instructions. Workspaces from before are in no section
	// ("Unsectioned") and have no instructions.
	`CREATE TABLE sections (
		id           INTEGER PRIMARY KEY,
		name         TEXT NOT NULL COLLATE NOCASE UNIQUE,
		repo         TEXT NOT NULL DEFAULT '', -- "owner/repo"; '' to choose per workspace
		base         TEXT NOT NULL DEFAULT '', -- '' for the repository's default branch
		model        TEXT NOT NULL DEFAULT '', -- '' for the default model
		image        TEXT NOT NULL DEFAULT '', -- '' for the built-in runtime
		setup_script TEXT NOT NULL DEFAULT '', -- '' for none
		instructions TEXT NOT NULL DEFAULT '', -- added to the agent's system prompt; '' for none
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	);
	ALTER TABLE workspaces ADD COLUMN section_id INTEGER REFERENCES sections (id) ON DELETE SET NULL; -- NULL: unsectioned; changes when the workspace is moved
	ALTER TABLE workspaces ADD COLUMN instructions TEXT NOT NULL DEFAULT ''; -- the agent instructions, fixed when the workspace is created`,
}

// migrate applies the migrations the database has not seen yet, each in
// its own transaction.
func (st *store) migrate() error {
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("the database has schema version %d, but this Mosso only knows up to %d: it was written by a newer Mosso", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		err := st.tx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return err
			}
			// PRAGMA takes no parameters; the value is an int of ours.
			_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	return nil
}
