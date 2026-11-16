package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

func testRecord(name string) workspaceRecord {
	return workspaceRecord{
		Name: name, Repo: "octo/widgets", Base: "main", Branch: "mosso/x", SandboxState: sandboxStopped,
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 123456000, time.UTC),
		workspaceState: workspaceState{
			BaseSHA: testCommitSHA, Dir: "/vercel/sandbox/widgets",
			Sandbox: vercel.Sandbox{Name: name, SessionID: "sbx_1", Status: "stopped", Persistent: true},
			LastSeq: 7, SessionID: testSessionID, Pushed: testCommitSHA,
			PullRequest: &github.PullRequest{Number: 3, URL: "https://github.com/octo/widgets/pull/3"},
			Attachments: []attachment{{ID: "a1", Name: "x.png", Size: 3, Type: "image/png", Image: true}},
		},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	// The default data directory on macOS has a space in it.
	path := filepath.Join(t.TempDir(), "Application Support", "mosso", "mosso.db")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord("mosso-0123456789ab")
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	older := testRecord("mosso-ffffffffffff")
	older.CreatedAt = rec.CreatedAt.Add(-time.Hour)
	if err := st.save(older); err != nil {
		t.Fatal(err)
	}
	// Saving again updates the row.
	rec.SandboxState, rec.LastSeq, rec.ArchivedAt = sandboxTerminated, 9, time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	entries := []chatEntry{
		{ID: 0, Type: "user", Text: "hi", Seq: 1, Time: rec.CreatedAt},
		{ID: 1, Type: "notice", Text: "n", Time: rec.CreatedAt},
	}
	if err := st.appendEntries(rec.Name, entries[:1]); err != nil {
		t.Fatal(err)
	}
	if err := st.appendEntries(rec.Name, entries[1:]); err != nil {
		t.Fatal(err)
	}
	// Chats of other workspaces are kept apart, whatever the order of writes.
	if err := st.appendEntries(older.Name, []chatEntry{{Type: "notice", Text: "other"}}); err != nil {
		t.Fatal(err)
	}
	// An entry that cannot be read is skipped, not fatal: the workspace
	// must still be restored and its sandbox reconnected.
	if _, err := st.db.Exec(`INSERT INTO chat_entries (workspace_id, entry) VALUES (?, ?)`, rec.Name, `{"type":"us`); err != nil {
		t.Fatal(err)
	}
	if err := st.appendEntries(rec.Name, []chatEntry{{ID: 2, Type: "result", Result: &turnResult{Text: "done"}, Time: rec.CreatedAt}}); err != nil {
		t.Fatal(err)
	}
	// Only a saved workspace has a chat.
	if err := st.appendEntries("mosso-000000000000", entries[:1]); err == nil {
		t.Error("saved a chat entry for a workspace that is not saved")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %v, %v; want 0600", info.Mode(), err)
	}

	// Everything is there after closing and opening the database again.
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	records, err := st.load()
	if err != nil || len(records) != 2 || records[0].Name != older.Name {
		t.Fatalf("load = %+v, %v; want two records, oldest first", records, err)
	}
	got := records[1]
	if got.Name != rec.Name || got.Repo != rec.Repo || got.Base != rec.Base || got.Branch != rec.Branch || got.SandboxState != sandboxTerminated ||
		!got.CreatedAt.Equal(rec.CreatedAt) || !got.ArchivedAt.Equal(rec.ArchivedAt) || got.UpdatedAt.IsZero() {
		t.Fatalf("record = %+v", got)
	}
	if got.BaseSHA != rec.BaseSHA || got.Dir != rec.Dir || got.Sandbox.Name != rec.Name || got.Sandbox.SessionID != "sbx_1" || got.LastSeq != 9 ||
		got.SessionID != testSessionID || got.Pushed != testCommitSHA || got.PullRequest.Number != 3 || len(got.Attachments) != 1 {
		t.Fatalf("record state = %+v", got.workspaceState)
	}
	if !records[0].ArchivedAt.IsZero() {
		t.Fatalf("unarchived record has archived_at = %v", records[0].ArchivedAt)
	}
	loaded, err := st.loadEntries(rec.Name)
	if err != nil || len(loaded) != 3 || loaded[0].Text != "hi" || loaded[0].Seq != 1 || loaded[1].ID != 1 || loaded[1].Type != "notice" ||
		loaded[2].ID != 2 || loaded[2].Result.Text != "done" {
		t.Fatalf("entries = %+v, %v", loaded, err)
	}
	if other, err := st.loadEntries(older.Name); err != nil || len(other) != 1 || other[0].Text != "other" || other[0].ID != 0 {
		t.Fatalf("entries of another workspace = %+v, %v", other, err)
	}
	// The metadata is in columns of its own, for anyone reading the database.
	var repo, branch, sandboxID, status string
	if err := st.db.QueryRow(`SELECT repo, branch, sandbox_id, status FROM workspaces WHERE id = ?`, rec.Name).Scan(&repo, &branch, &sandboxID, &status); err != nil ||
		repo != "octo/widgets" || branch != "mosso/x" || sandboxID != rec.Name || status != sandboxTerminated {
		t.Fatalf("columns = %q %q %q %q, %v", repo, branch, sandboxID, status, err)
	}
}

func TestStoreMigrations(t *testing.T) {
	released := migrations[:len(migrations):len(migrations)]
	defer func() { migrations = released }()
	path := filepath.Join(t.TempDir(), "mosso.db")
	version := func(st *store) int {
		t.Helper()
		var v int
		if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	open := func() *store {
		t.Helper()
		st, err := openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	if v := version(st); v != len(released) {
		t.Fatalf("schema version = %d, want %d", v, len(released))
	}
	if err := st.save(testRecord("mosso-0123456789ab")); err != nil {
		t.Fatal(err)
	}
	st.close()

	// A later migration is applied to an existing database, on top of the
	// data it has.
	next := append(released, `ALTER TABLE workspaces ADD COLUMN test_note TEXT NOT NULL DEFAULT 'migrated'`)
	migrations = next
	st = open()
	var note string
	if err := st.db.QueryRow(`SELECT test_note FROM workspaces`).Scan(&note); err != nil || note != "migrated" || version(st) != len(next) {
		t.Fatalf("after the new migration: note = %q, version = %d, %v", note, version(st), err)
	}
	st.close()

	// A migration that fails is rolled back and leaves the version alone.
	migrations = append(next[:len(next):len(next)], `CREATE TABLE half_done (x TEXT); ALTER TABLE no_such_table ADD COLUMN x TEXT`)
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "schema migration") {
		t.Fatalf("open with a failing migration: %v", err)
	}
	migrations = next
	st = open()
	var tables int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'`).Scan(&tables); err != nil || tables != 0 || version(st) != len(next) {
		t.Fatalf("after a failed migration: half_done tables = %d, version = %d, %v", tables, version(st), err)
	}
	st.close()

	// A database from a newer Mosso is refused rather than misread.
	migrations = released
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "newer Mosso") {
		t.Fatalf("open a newer database: %v", err)
	}
}

// TestImportFiles: workspaces saved as files by a Mosso from before the
// database are imported once, with their chats.
func TestImportFiles(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "workspaces")
	name := "mosso-0123456789ab"
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(legacy, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(name+"/workspace.json", `{"name":"`+name+`","repo":"octo/widgets","base":"main","base_sha":"`+testCommitSHA+`","branch":"mosso/x","dir":"/vercel/sandbox/widgets",
		"sandbox":{"name":"`+name+`","session_id":"sbx_9","status":"stopped","created_at":"2026-05-01T12:00:00Z","persistent":true},
		"sandbox_state":"stopped","pull_request":{"number":3,"url":"https://github.com/octo/widgets/pull/3"},"claude_session_id":"`+testSessionID+`",
		"last_seq":4,"task_running":false,"created_at":"2026-05-01T12:00:00Z","updated_at":"2026-05-01T13:00:00Z"}`)
	// A truncated last line is skipped.
	write(name+"/chat.jsonl", `{"id":0,"time":"2026-05-01T12:00:01Z","type":"user","text":"hi","seq":1}`+"\n"+
		`{"id":1,"time":"2026-05-01T12:00:02Z","type":"notice","text":"n"}`+"\n"+`{"type":"us`)
	write("junk/workspace.json", `{}`)
	write("mosso-aaaaaaaaaaaa/chat.jsonl", "") // no record: ignored

	st, err := openStore(filepath.Join(dir, "mosso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if n, err := st.importFiles(legacy); err != nil || n != 1 {
		t.Fatalf("importFiles = %d, %v", n, err)
	}
	records, err := st.load()
	if err != nil || len(records) != 1 {
		t.Fatalf("load = %+v, %v", records, err)
	}
	got := records[0]
	if got.Name != name || got.Repo != "octo/widgets" || got.SandboxState != sandboxStopped || got.Sandbox.SessionID != "sbx_9" || got.LastSeq != 4 ||
		got.SessionID != testSessionID || got.PullRequest.Number != 3 || !got.CreatedAt.Equal(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("imported record = %+v", got)
	}
	entries, err := st.loadEntries(name)
	if err != nil || len(entries) != 2 || entries[0].Text != "hi" || entries[0].Seq != 1 || entries[1].Type != "notice" {
		t.Fatalf("imported entries = %+v, %v", entries, err)
	}
	// The files are set aside, so the next start has nothing to import.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the imported directory is still in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy+".imported", name, "chat.jsonl")); err != nil {
		t.Fatalf("the imported files were not kept: %v", err)
	}
	if n, err := st.importFiles(legacy); err != nil || n != 0 {
		t.Fatalf("second importFiles = %d, %v", n, err)
	}
}

// TestRestoreImportsFiles: the import runs on startup and the imported
// workspace is restored like any other.
func TestRestoreImportsFiles(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("hi")
	events.until("result")
	rec := h.record()
	chat := h.history()
	h.stop()

	// The same workspace as an earlier Mosso would have saved it, and no
	// database yet.
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspaces", rec.Name)
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var files [2]bytes.Buffer
	if err := json.NewEncoder(&files[0]).Encode(rec); err != nil {
		t.Fatal(err)
	}
	for _, e := range chat {
		if err := json.NewEncoder(&files[1]).Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"workspace.json", "chat.jsonl"} {
		if err := os.WriteFile(filepath.Join(wsDir, name), files[i].Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.dataDir = dir
	h.start()
	h.waitStatus("ready")
	if got := entryTypes(h.history()); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("history after import = %s", got)
	}
	if got := h.record(); got.Sandbox.SessionID != rec.Sandbox.SessionID || got.SessionID != testSessionID {
		t.Fatalf("record after import = %+v", got)
	}
}

func TestDataDirAndDBPath(t *testing.T) {
	t.Setenv(EnvDBPath, "")
	t.Setenv(EnvDataDir, "/tmp/mosso-test-data")
	if d, err := DataDir(); err != nil || d != "/tmp/mosso-test-data" {
		t.Fatalf("DataDir = %q, %v", d, err)
	}
	// The files of an earlier Mosso are in the data directory, wherever
	// the database is.
	t.Setenv(EnvDBPath, "/data/history.db")
	if d := LegacyDir(); d != "/tmp/mosso-test-data/workspaces" {
		t.Fatalf("LegacyDir = %q", d)
	}
	t.Setenv(EnvDBPath, "")
	if p, err := DBPath(); err != nil || p != "/tmp/mosso-test-data/mosso.db" {
		t.Fatalf("DBPath = %q, %v", p, err)
	}
	t.Setenv(EnvDBPath, "/data/history.db")
	if p, err := DBPath(); err != nil || p != "/data/history.db" {
		t.Fatalf("DBPath = %q, %v", p, err)
	}
	t.Setenv(EnvDBPath, "")
	t.Setenv(EnvDataDir, "")
	d, err := DataDir()
	if err != nil || !strings.HasSuffix(d, "mosso") {
		t.Fatalf("DataDir = %q, %v", d, err)
	}
	if p, err := DBPath(); err != nil || p != filepath.Join(d, "mosso.db") {
		t.Fatalf("DBPath = %q, %v", p, err)
	}
}
