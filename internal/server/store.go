package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // the SQLite driver, in pure Go (no cgo)

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

const (
	// EnvDataDir names the directory Mosso keeps its state in; see DataDir.
	EnvDataDir = "MOSSO_DATA_DIR"
	// EnvDBPath names the SQLite database file; see DBPath.
	EnvDBPath = "MOSSO_DB_PATH"
)

// DataDir is where Mosso keeps its state: $MOSSO_DATA_DIR, or "mosso" in
// the user's configuration directory (~/Library/Application Support on
// macOS, $XDG_CONFIG_HOME or ~/.config elsewhere).
func DataDir() (string, error) {
	if v := os.Getenv(EnvDataDir); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the data directory: %w (set %s)", err, EnvDataDir)
	}
	return filepath.Join(base, "mosso"), nil
}

// LegacyDir is where a Mosso from before the database saved workspaces as
// files: "workspaces" in the data directory, wherever the database is now.
// It is "" if there is no data directory to look in.
func LegacyDir() string {
	dir, err := DataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "workspaces")
}

// DBPath is the SQLite database Mosso saves workspaces and chats in:
// $MOSSO_DB_PATH, or mosso.db in the data directory. In a container, point
// either at a mounted volume.
func DBPath() (string, error) {
	if v := os.Getenv(EnvDBPath); v != "" {
		return v, nil
	}
	dir, err := DataDir()
	if err != nil {
		return "", fmt.Errorf("%w (or set %s)", err, EnvDBPath)
	}
	return filepath.Join(dir, "mosso.db"), nil
}

// workspaceRecord is what Mosso remembers about a workspace between runs:
// a row of the workspaces table. The chat is stored next to it, one row
// per entry. No credentials.
type workspaceRecord struct {
	// Name is the workspace's ID; the Vercel sandbox has the same name.
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
	// Model is the Claude model the workspace runs; empty for workspaces
	// from before models could be chosen.
	Model string `json:"model,omitempty"`
	// Image is the image the sandbox was created from; empty for the
	// built-in runtime. SetupScript is the script that ran once after the
	// clone; empty for none. Both are kept for reference and never change.
	Image       string `json:"image,omitempty"`
	SetupScript string `json:"setup_script,omitempty"`
	// Instructions are the agent instructions every turn's system prompt
	// carries; fixed at creation. SectionID is the section the workspace
	// is listed under, 0 for none; moving the workspace changes it.
	Instructions string `json:"instructions,omitempty"`
	SectionID    int64  `json:"section_id,omitempty"`
	// SandboxState is "running", "stopped", "gone" or "terminated", as
	// last known: the workspace's saved status.
	SandboxState string    `json:"sandbox_state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// ArchivedAt is when the workspace was archived, if it is.
	ArchivedAt time.Time `json:"archived_at,omitzero"`
	workspaceState
}

// workspaceState is the rest of a record, kept as JSON in the state column:
// what Mosso needs to find the sandbox again and continue where it left
// off. Adding a field here needs no migration.
type workspaceState struct {
	BaseSHA     string              `json:"base_sha"`
	Dir         string              `json:"dir"`
	Sandbox     vercel.Sandbox      `json:"sandbox"`
	ExpiresAt   time.Time           `json:"expires_at,omitzero"`
	PullRequest *github.PullRequest `json:"pull_request,omitempty"`
	// Pushed is the working branch head Mosso last pushed, so a rebased
	// branch can still be force-pushed after a restart.
	Pushed string `json:"pushed,omitempty"`
	// SessionID is the Claude Code session follow-ups resume.
	SessionID string `json:"claude_session_id,omitempty"`
	// LastSeq is the last worker event applied to the chat; TaskRunning
	// says whether a Claude task was in flight, so a restart knows it
	// may have missed events.
	LastSeq     int64        `json:"last_seq"`
	TaskRunning bool         `json:"task_running"`
	Attachments []attachment `json:"attachments,omitempty"`
	// QueuedPrompt says the first message given when the workspace was
	// created (its first user entry) is not known to have reached the
	// worker, so a restart must send it or match it to the worker's copy.
	QueuedPrompt bool `json:"queued_prompt,omitempty"`
	// PullRequests are all the workspace's pull requests, by the user or
	// the agent; PullRequest is the one Create PR publishes to.
	PullRequests []pullRequestRecord `json:"pull_requests,omitempty"`
	// Branches are the extra branches the workspace's mirror keeps in
	// sync (see workspace.branches).
	Branches []string `json:"branches,omitempty"`
	// SessionSeconds is the most a sandbox session may last; 0 for
	// workspaces from before it could be chosen (45 minutes).
	// IdleTimeoutSeconds is how long the sandbox may idle before Mosso
	// stops it, 0 for never; nil for workspaces from before idle shutdown,
	// which get the default. See idle.go.
	SessionSeconds     int64  `json:"session_seconds,omitempty"`
	IdleTimeoutSeconds *int64 `json:"idle_timeout_seconds,omitempty"`
}

// store keeps workspaces and their chats in a SQLite database. It is the
// only code that talks SQL; its schema is in migrations.go.
type store struct {
	db *sql.DB
}

// openStore opens the database at path, creating it and its directory if
// needed, and brings its schema up to date.
func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory: %w", err)
	}
	// Chats are private: create the file readable by its owner only.
	// SQLite gives its -wal and -shm files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	f.Close()
	// WAL with synchronous=NORMAL makes saving a chat entry cheap (no fsync
	// per entry) and cannot corrupt the database; after a power cut the
	// last entries may be missing, and are fetched from the worker again.
	// WAL needs a local filesystem, not a network one such as NFS.
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	// One connection: Mosso is the database's only user and writes one
	// thing at a time.
	db.SetMaxOpenConns(1)
	st := &store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open the database %s: %w", path, err)
	}
	return st, nil
}

func (st *store) close() error { return st.db.Close() }

// tx runs fn in a transaction.
func (st *store) tx(fn func(*sql.Tx) error) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Timestamps are stored as UTC text of a fixed width, so they sort.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

// save inserts or updates the workspace's row and adds entries to the end
// of its conversation, all or nothing.
func (st *store) save(rec workspaceRecord, entries ...chatEntry) error {
	return st.tx(func(tx *sql.Tx) error {
		if err := saveRecord(tx, rec); err != nil {
			return err
		}
		return insertEntries(tx, rec.Name, entries)
	})
}

func saveRecord(tx *sql.Tx, rec workspaceRecord) error {
	state, err := json.Marshal(rec.workspaceState)
	if err != nil {
		return err
	}
	var archived any
	if !rec.ArchivedAt.IsZero() {
		archived = formatTime(rec.ArchivedAt)
	}
	sandboxID := rec.Sandbox.Name
	if sandboxID == "" {
		sandboxID = rec.Name
	}
	// The model, image, setup script and instructions are written once:
	// they are what the workspace was created with. The section changes
	// when the workspace is moved.
	_, err = tx.Exec(`
		INSERT INTO workspaces (id, repo, base, branch, model, image, setup_script, instructions, section_id, sandbox_id, status, state, created_at, updated_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			repo = excluded.repo, base = excluded.base, branch = excluded.branch, section_id = excluded.section_id,
			sandbox_id = excluded.sandbox_id, status = excluded.status, state = excluded.state,
			updated_at = excluded.updated_at, archived_at = excluded.archived_at`,
		rec.Name, rec.Repo, rec.Base, rec.Branch, rec.Model, rec.Image, rec.SetupScript, rec.Instructions, nullID(rec.SectionID),
		sandboxID, rec.SandboxState, string(state), formatTime(rec.CreatedAt), formatTime(time.Now()), archived)
	return err
}

// nullID stores a section ID of 0 (none) as NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// load returns every saved workspace, oldest first.
func (st *store) load() ([]workspaceRecord, error) {
	rows, err := st.db.Query(`
		SELECT id, repo, base, branch, model, image, setup_script, instructions, section_id, sandbox_id, status, state, created_at, updated_at, archived_at
		FROM workspaces ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workspaceRecord
	for rows.Next() {
		var rec workspaceRecord
		var sandboxID, state, created, updated string
		var archived sql.NullString
		var section sql.NullInt64
		if err := rows.Scan(&rec.Name, &rec.Repo, &rec.Base, &rec.Branch, &rec.Model, &rec.Image, &rec.SetupScript, &rec.Instructions, &section,
			&sandboxID, &rec.SandboxState, &state, &created, &updated, &archived); err != nil {
			return nil, err
		}
		rec.SectionID = section.Int64
		if err := json.Unmarshal([]byte(state), &rec.workspaceState); err != nil {
			return nil, fmt.Errorf("read workspace %s: %w", rec.Name, err)
		}
		rec.Sandbox.Name = sandboxID
		rec.CreatedAt, rec.UpdatedAt = parseTime(created), parseTime(updated)
		if archived.Valid {
			rec.ArchivedAt = parseTime(archived.String)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Setting keys.
const (
	settingDefaultModel       = "default_model"
	settingDefaultImage       = "default_image"
	settingDefaultSetupScript = "default_setup_script"
	// settingDefaultSection is the ID of the section the New workspace
	// dialog preselects, in decimal; unset or "" for none.
	settingDefaultSection = "default_section"
)

// setting returns a saved setting, or "" if it is not set.
func (st *store) setting(key string) (string, error) {
	var v string
	err := st.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// setSetting saves a setting, replacing its previous value.
func (st *store) setSetting(key, value string) error {
	_, err := st.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// setSettings saves several settings, all or nothing.
func (st *store) setSettings(values map[string]string) error {
	return st.tx(func(tx *sql.Tx) error {
		for k, v := range values {
			if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// exists reports whether the workspace is saved.
func (st *store) exists(name string) (bool, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, name).Scan(&n)
	return n > 0, err
}

// appendEntries adds chat entries to the end of a saved workspace's
// conversation, in the order given.
func (st *store) appendEntries(name string, entries []chatEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return st.tx(func(tx *sql.Tx) error { return insertEntries(tx, name, entries) })
}

func insertEntries(tx *sql.Tx, name string, entries []chatEntry) error {
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO chat_entries (workspace_id, entry) VALUES (?, ?)`, name, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// loadEntries returns the workspace's conversation, in order, numbered
// from 0. An entry that cannot be read is skipped: losing one entry must
// not stop a workspace from being restored and its sandbox reconnected.
func (st *store) loadEntries(name string) ([]chatEntry, error) {
	rows, err := st.db.Query(`SELECT entry FROM chat_entries WHERE workspace_id = ? ORDER BY id`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatEntry
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e chatEntry
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			log.Printf("workspace %s: skipping a saved chat entry that cannot be read: %v", name, err)
			continue
		}
		e.ID = len(out)
		out = append(out, e)
	}
	return out, rows.Err()
}

// sectionColumns are the columns scanSection reads, in its order.
const sectionColumns = `id, name, repo, base, model, image, setup_script, instructions, created_at, updated_at`

func scanSection(row interface{ Scan(...any) error }) (section, error) {
	var sec section
	var created, updated string
	err := row.Scan(&sec.ID, &sec.Name, &sec.Repo, &sec.Base, &sec.Model, &sec.Image, &sec.SetupScript, &sec.Instructions, &created, &updated)
	sec.CreatedAt, sec.UpdatedAt = parseTime(created), parseTime(updated)
	return sec, err
}

// sections returns every section, by name.
func (st *store) sections() ([]section, error) {
	rows, err := st.db.Query(`SELECT ` + sectionColumns + ` FROM sections ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []section{}
	for rows.Next() {
		sec, err := scanSection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// section returns the section with the ID, or errNoSection.
func (st *store) section(id int64) (section, error) {
	sec, err := scanSection(st.db.QueryRow(`SELECT `+sectionColumns+` FROM sections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return section{}, errNoSection
	}
	return sec, err
}

// sectionNameTaken reports whether another section than id has the name,
// ignoring case.
func (st *store) sectionNameTaken(name string, id int64) (bool, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM sections WHERE name = ? AND id != ?`, name, id).Scan(&n)
	return n > 0, err
}

// saveSection inserts a section (ID 0) or updates one, and returns it as
// saved, with its ID and timestamps.
func (st *store) saveSection(sec section) (section, error) {
	now := time.Now().UTC().Truncate(time.Microsecond) // as stored
	sec.UpdatedAt = now
	if sec.ID == 0 {
		sec.CreatedAt = now
		res, err := st.db.Exec(`INSERT INTO sections (name, repo, base, model, image, setup_script, instructions, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sec.Name, sec.Repo, sec.Base, sec.Model, sec.Image, sec.SetupScript, sec.Instructions, formatTime(now), formatTime(now))
		if err != nil {
			return section{}, err
		}
		sec.ID, err = res.LastInsertId()
		return sec, err
	}
	res, err := st.db.Exec(`UPDATE sections SET name = ?, repo = ?, base = ?, model = ?, image = ?, setup_script = ?, instructions = ?, updated_at = ?
		WHERE id = ?`,
		sec.Name, sec.Repo, sec.Base, sec.Model, sec.Image, sec.SetupScript, sec.Instructions, formatTime(now), sec.ID)
	if err != nil {
		return section{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return section{}, errNoSection
	}
	return sec, nil
}

// deleteSection deletes a section, all or nothing. Its workspaces become
// unsectioned, keeping everything else, and it stops being the default
// section if it was.
func (st *store) deleteSection(id int64) error {
	return st.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE workspaces SET section_id = NULL WHERE section_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM settings WHERE key = ? AND value = ?`, settingDefaultSection, strconv.FormatInt(id, 10)); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM sections WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNoSection
		}
		return nil
	})
}
