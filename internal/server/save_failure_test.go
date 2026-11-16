package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/github"
)

// failWrites makes the database refuse one kind of write, such as "INSERT
// ON workspaces", the way a full disk would, until the returned function
// is called.
func failWrites(t *testing.T, srv *Server, on string) (allow func()) {
	t.Helper()
	exec := func(q string) {
		t.Helper()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if _, err := srv.store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER test_fail BEFORE ` + on + ` BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	return func() { exec(`DROP TRIGGER test_fail`) }
}

// savedRows counts the rows of a table.
func savedRows(t *testing.T, srv *Server, table string) int {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var n int
	if err := srv.store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCreateFailsWhenTheWorkspaceCannotBeSaved: a workspace that cannot be
// saved is not reported as created, and its sandbox is destroyed rather
// than left running where a restarted Mosso would not know about it.
func TestCreateFailsWhenTheWorkspaceCannotBeSaved(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   string
		// setupNotice makes setup leave a chat entry, so there is one to save.
		setupNotice bool
	}{
		{name: "record", on: "INSERT ON workspaces"},
		{name: "chat", on: "INSERT ON chat_entries", setupNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := newTestServer(t)
			api := &fakeGitHubAPI{}
			withGitHub(t, srv, api)
			if tc.setupNotice {
				srv.gitSync = func(*github.Mirror, context.Context, github.Token) (github.Synced, error) {
					return github.Synced{}, errors.New("github is unreachable")
				}
			}
			fake.stdoutFor = fakeRepoSandbox()
			h := srv.Handler()
			allow := failWrites(t, srv, tc.on)

			rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
			if msg := rec.Body.String(); rec.Code != http.StatusInternalServerError || !strings.Contains(msg, "could not be saved") || !strings.Contains(msg, "injected failure") {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			if len(fake.created) != 1 || len(fake.deleted) != 1 || fake.deleted[0] != fake.created[0] {
				t.Fatalf("created=%v deleted=%v, want the new sandbox destroyed", fake.created, fake.deleted)
			}
			if rec := do(t, h, http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), `"workspaces":[]`) {
				t.Fatalf("an unsaved workspace is listed: %s", rec.Body)
			}
			if rec := do(t, h, http.MethodGet, "/sandboxes", ""); !strings.Contains(rec.Body.String(), `"sandboxes":[]`) {
				t.Fatalf("sandbox still tracked: %s", rec.Body)
			}
			// The record and the chat are saved together or not at all.
			if w, c := savedRows(t, srv, "workspaces"), savedRows(t, srv, "chat_entries"); w != 0 || c != 0 {
				t.Fatalf("saved rows after a failed create: workspaces=%d chat_entries=%d", w, c)
			}

			// Once the database works again, so does creating.
			allow()
			rec = do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create after recovery: status = %d: %s", rec.Code, rec.Body)
			}
			wantChat := 0
			if tc.setupNotice {
				wantChat = 1 // the notice from setup is saved with the record
			}
			if w, c := savedRows(t, srv, "workspaces"), savedRows(t, srv, "chat_entries"); w != 1 || c != wantChat {
				t.Fatalf("saved rows after a create: workspaces=%d chat_entries=%d, want 1 and %d", w, c, wantChat)
			}
		})
	}
}

// TestTerminateFailsWhenItCannotBeSaved: the sandbox is deleted, but the
// request does not report success until the termination is saved, and
// asking again saves it without deleting anything twice.
func TestTerminateFailsWhenItCannotBeSaved(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("hi")
	events.until("result")
	allow := failWrites(t, h.srv, "UPDATE ON workspaces")

	terminate := func() (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/workspaces/"+h.ws.Name, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v.Error
	}
	unsaved := func() bool {
		t.Helper()
		var ws workspace
		_ = json.Unmarshal(do(t, h.srv.Handler(), http.MethodGet, "/workspaces/"+h.ws.Name, "").Body.Bytes(), &ws)
		if ws.Status != "terminated" {
			t.Fatalf("status = %q, want terminated: the sandbox is deleted", ws.Status)
		}
		return ws.Unsaved
	}
	for attempt := 1; attempt <= 2; attempt++ {
		code, msg := terminate()
		if code != http.StatusInternalServerError || !strings.Contains(msg, "were deleted") || !strings.Contains(msg, "could not save") {
			t.Fatalf("terminate attempt %d with a failing database: status = %d, error = %q", attempt, code, msg)
		}
		if len(h.fake.deleted) != 1 {
			t.Fatalf("deleted = %v after attempt %d, want the sandbox deleted once", h.fake.deleted, attempt)
		}
		if !unsaved() {
			t.Fatalf("the workspace is not marked unsaved after attempt %d", attempt)
		}
		if rec := h.record(); rec.SandboxState == sandboxTerminated {
			t.Fatalf("the database says terminated although the write failed: %+v", rec)
		}
	}

	// The database works again: the same request now succeeds.
	allow()
	if code, msg := terminate(); code != http.StatusNoContent {
		t.Fatalf("terminate after recovery: status = %d, error = %q", code, msg)
	}
	if len(h.fake.deleted) != 1 {
		t.Fatalf("deleted = %v, want the sandbox deleted once", h.fake.deleted)
	}
	if unsaved() {
		t.Fatal("the workspace is still marked unsaved")
	}
	if rec := h.record(); rec.SandboxState != sandboxTerminated {
		t.Fatalf("record after recovery = %+v", rec)
	}
	h.stop()
	h.start()
	if st := h.status(); st.Status != "terminated" {
		t.Fatalf("status after a restart = %+v", st)
	}
	// The conversation was saved once, with the termination notice.
	if got := entryTypes(h.history()); got != "user claude:tool_use claude:tool_result result notice" {
		t.Fatalf("history after a restart = %s", got)
	}
}

// TestUnsavedTerminationShowsAsGoneAfterARestart: if the termination never
// gets saved, the next start finds no sandbox and keeps the conversation.
func TestUnsavedTerminationShowsAsGoneAfterARestart(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("hi")
	events.until("result")
	failWrites(t, h.srv, "UPDATE ON workspaces")
	req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/workspaces/"+h.ws.Name, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("terminate: %v %v", resp, err)
	}
	h.stop() // tries to save once more, and fails again
	h.start()
	h.waitStatus("error")
	if st := h.status(); !strings.Contains(st.Error, "no longer exists") {
		t.Fatalf("status = %+v", st)
	}
	if got := entryTypes(h.history()); !strings.HasPrefix(got, "user claude:tool_use claude:tool_result result") {
		t.Fatalf("history = %s", got)
	}
	if len(h.fake.deleted) != 1 {
		t.Fatalf("deleted = %v", h.fake.deleted)
	}
}

// TestArchiveFailsWhenItCannotBeSaved: an archive that is not saved is not
// reported, or shown, as done.
func TestArchiveFailsWhenItCannotBeSaved(t *testing.T) {
	h := newPersistentHarness(t)
	h.terminate()
	allow := failWrites(t, h.srv, "UPDATE ON workspaces")
	archive := func() (int, workspace) {
		t.Helper()
		resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/archive", `{}`)
		var ws workspace
		_ = json.Unmarshal(do(t, h.srv.Handler(), http.MethodGet, "/workspaces/"+h.ws.Name, "").Body.Bytes(), &ws)
		return resp.StatusCode, ws
	}
	if code, ws := archive(); code != http.StatusInternalServerError || !ws.ArchivedAt.IsZero() || ws.Unsaved {
		t.Fatalf("archive with a failing database: status = %d, workspace = %+v", code, ws)
	}
	allow()
	if code, ws := archive(); code != http.StatusOK || ws.ArchivedAt.IsZero() {
		t.Fatalf("archive after recovery: status = %d, workspace = %+v", code, ws)
	}
	if rec := h.record(); rec.ArchivedAt.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
}

// TestChatEntriesAreSavedAfterAFailedWrite: an entry whose save failed is
// written with the next one, in order, instead of being lost.
func TestChatEntriesAreSavedAfterAFailedWrite(t *testing.T) {
	h := newPersistentHarness(t)
	allow := failWrites(t, h.srv, "INSERT ON chat_entries")
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("first")
	events.until("result")
	if n := savedRows(t, h.srv, "chat_entries"); n != 0 {
		t.Fatalf("chat entries saved while the database refused them: %d", n)
	}
	allow()
	h.chat("second")
	events.until("result")
	h.waitStatus("ready")
	h.stop()
	h.start()
	hist := h.history()
	if got := entryTypes(hist); got != "user claude:tool_use claude:tool_result result user claude:tool_use claude:tool_result result" ||
		hist[0].Text != "first" || hist[4].Text != "second" {
		t.Fatalf("history after a restart = %s", got)
	}
}

// TestHistoryWithoutVercel: with no sandbox provider configured, Mosso
// still loads every saved workspace and serves the endpoints that only
// read (or archive) them; nothing that needs a sandbox works, and nothing
// is lost for the next start with credentials.
func TestHistoryWithoutVercel(t *testing.T) {
	h := newPersistentHarness(t)
	past := h.ws.Name
	events := openEvents(t, h.ts.URL, past, "")
	h.chat("old work")
	events.until("result")
	// A second workspace keeps its sandbox.
	rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var live testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &live)
	events = openEvents(t, h.ts.URL, live.Name, "")
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+live.Name+"/chat", `{"prompt":"current work"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	events.until("result")
	h.terminate()
	h.stop()

	// Mosso without Vercel credentials, over the same database. (It has no
	// Vercel client at all, so it cannot reach a sandbox by accident.)
	srv := New(nil)
	if err := srv.Restore(context.Background(), filepath.Join(h.dataDir, "mosso.db"), ""); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.CloseStreams()
	handler := srv.Handler()
	if rec := do(t, handler, http.MethodGet, "/vercel/auth", ""); !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("/vercel/auth = %s", rec.Body)
	}

	var list struct {
		Workspaces []workspace `json:"workspaces"`
	}
	rec = do(t, handler, http.MethodGet, "/workspaces", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || len(list.Workspaces) != 2 {
		t.Fatalf("GET /workspaces without Vercel: status = %d: %s", rec.Code, rec.Body)
	}
	if ws := list.Workspaces[0]; ws.Name != past || ws.Status != "terminated" {
		t.Fatalf("history workspace = %+v", ws)
	}
	// The one with a sandbox is shown as last saved, not as reconnecting.
	if ws := list.Workspaces[1]; ws.Name != live.Name || (ws.Status != "ready" && ws.Status != "stopped") {
		t.Fatalf("workspace with a sandbox = %+v", ws)
	}
	for _, name := range []string{past, live.Name} {
		if rec := do(t, handler, http.MethodGet, "/workspaces/"+name, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET /workspaces/%s without Vercel: status = %d", name, rec.Code)
		}
	}
	// Conversations are replayed from the database.
	if got := entryTypes(openEvents(t, ts.URL, past, "").until("notice")); got != "user claude:tool_use claude:tool_result result notice" {
		t.Fatalf("replay of the terminated workspace = %s", got)
	}
	replay := openEvents(t, ts.URL, live.Name, "").until("result")
	if got := entryTypes(replay); got != "user claude:tool_use claude:tool_result result" || replay[0].Text != "current work" {
		t.Fatalf("replay of the workspace with a sandbox = %s", got)
	}
	// Archiving only touches the database.
	for _, action := range []string{"archive", "unarchive", "archive"} {
		if resp := postJSON(t, ts.URL+"/workspaces/"+past+"/"+action, `{}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s without Vercel: status = %d", action, resp.StatusCode)
		}
	}
	if resp := postJSON(t, ts.URL+"/workspaces/"+live.Name+"/archive", `{}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("archive of a workspace with a sandbox: status = %d", resp.StatusCode)
	}
	// Everything that needs a sandbox says Vercel is not configured.
	for _, tc := range [][2]string{
		{http.MethodPost, "/workspaces"},
		{http.MethodPost, "/workspaces/" + live.Name + "/chat"},
		{http.MethodPost, "/workspaces/" + live.Name + "/connect"},
		{http.MethodPost, "/workspaces/" + live.Name + "/pull-request"},
		{http.MethodPost, "/workspaces/" + live.Name + "/chat/stop"},
		{http.MethodDelete, "/workspaces/" + live.Name},
		{http.MethodGet, "/sandboxes"},
	} {
		if rec := do(t, handler, tc[0], tc[1], `{"repo":"octo/widgets","prompt":"x"}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not configured") {
			t.Fatalf("%s %s without Vercel: status = %d: %s", tc[0], tc[1], rec.Code, rec.Body)
		}
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Started with credentials again, the sandbox is reconnected to and
	// the archive made meanwhile is still there.
	h.start()
	if st := h.status(); st.Status != "terminated" {
		t.Fatalf("history workspace after the restart = %+v", st)
	}
	if rec := h.record(); rec.ArchivedAt.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
	waitFor(t, func() bool {
		var ws workspace
		_ = json.Unmarshal(do(t, h.srv.Handler(), http.MethodGet, "/workspaces/"+live.Name, "").Body.Bytes(), &ws)
		return ws.Status == "ready"
	})
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+live.Name+"/chat", `{"prompt":"more"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat after the restart: status = %d", resp.StatusCode)
	}
}

// TestWorkspaceListWithoutVercelOrDatabase: the list is served (empty)
// even with nothing configured.
func TestWorkspaceListWithoutVercelOrDatabase(t *testing.T) {
	rec := do(t, NewHandler(), http.MethodGet, "/workspaces", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"workspaces":[]`) {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}
