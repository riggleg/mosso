package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/claude"
)

func getSettings(t *testing.T, h http.Handler) settings {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d: %s", rec.Code, rec.Body)
	}
	var s settings
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// createWorkspaceModel creates a workspace from body and returns it.
func createWorkspaceModel(t *testing.T, h http.Handler, body string) testWorkspace {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/workspaces", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status = %d: %s", body, rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	return ws
}

// TestWorkspaceModelSelection: a workspace runs the model it was created
// with, or the default model, on its first and every later turn.
func TestWorkspaceModelSelection(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()

	s := getSettings(t, h)
	if s.DefaultModel != claude.DefaultModel || len(s.Models) != len(claude.Models) || !claude.KnownModel(claude.DefaultModel) {
		t.Fatalf("settings = %+v", s)
	}

	// Without a model, the default.
	ws := createWorkspaceModel(t, h, `{"repo":"octo/widgets"}`)
	if ws.Model != claude.DefaultModel {
		t.Fatalf("model = %q, want the default %s", ws.Model, claude.DefaultModel)
	}
	// The first turn and a follow-up that resumes its session.
	session := ""
	for _, prompt := range []string{"first", "second"} {
		body := `{"prompt":"` + prompt + `","session_id":"` + session + `"}`
		rec := do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("message status = %d: %s", rec.Code, rec.Body)
		}
		if task := fake.claude.last(); task.Model != claude.DefaultModel || task.SessionID != session {
			t.Fatalf("turn %s: model = %q, session = %q", prompt, task.Model, task.SessionID)
		}
		var turn claude.Turn
		_ = json.Unmarshal(rec.Body.Bytes(), &turn)
		session = turn.SessionID
	}
	if session == "" {
		t.Fatal("no session to resume")
	}

	// A new default applies to new workspaces only.
	if rec := do(t, h, http.MethodPost, "/settings", `{"default_model":"claude-sonnet-5-5"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"default_model":"claude-sonnet-5-5"`) {
		t.Fatalf("set default: status = %d: %s", rec.Code, rec.Body)
	}
	if ws2 := createWorkspaceModel(t, h, `{"repo":"octo/widgets"}`); ws2.Model != "claude-sonnet-5-5" {
		t.Fatalf("model after changing the default = %q", ws2.Model)
	}
	do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"third"}`)
	if task := fake.claude.last(); task.Model != claude.DefaultModel {
		t.Fatalf("an existing workspace changed model with the default: %q", task.Model)
	}

	// An explicit model wins over the default.
	ws3 := createWorkspaceModel(t, h, `{"repo":"octo/widgets","model":"claude-haiku-5-5"}`)
	if ws3.Model != "claude-haiku-5-5" {
		t.Fatalf("explicit model = %q", ws3.Model)
	}
	do(t, h, http.MethodPost, "/workspaces/"+ws3.Name+"/claude/messages", `{"prompt":"hi"}`)
	if task := fake.claude.last(); task.Model != "claude-haiku-5-5" {
		t.Fatalf("turn model = %q", task.Model)
	}
	if got := getWorkspace(t, h, ws3.Name).Model; got != "claude-haiku-5-5" {
		t.Fatalf("GET workspace model = %q", got)
	}
}

func getWorkspace(t *testing.T, h http.Handler, name string) testWorkspace {
	t.Helper()
	var ws testWorkspace
	_ = json.Unmarshal(do(t, h, http.MethodGet, "/workspaces/"+name, "").Body.Bytes(), &ws)
	return ws
}

func TestModelValidation(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()
	for name, tc := range map[string]struct{ path, body string }{
		"unknown model":     {"/workspaces", `{"repo":"octo/widgets","model":"gpt-5"}`},
		"flag as model":     {"/workspaces", `{"repo":"octo/widgets","model":"--dangerously"}`},
		"unknown default":   {"/settings", `{"default_model":"claude-opus-1"}`},
		"empty default":     {"/settings", `{}`},
		"bad settings JSON": {"/settings", `{`},
	} {
		if rec := do(t, h, http.MethodPost, tc.path, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d: %s", name, rec.Code, rec.Body)
		}
	}
	fake.mu.Lock()
	ran := len(fake.cmds)
	fake.mu.Unlock()
	if ran != 0 {
		t.Fatalf("a rejected request ran %d sandbox commands", ran)
	}
	if s := getSettings(t, h); s.DefaultModel != claude.DefaultModel {
		t.Fatalf("a rejected update changed the default to %q", s.DefaultModel)
	}
}

// TestModelsSurviveRestart: the default model and each workspace's model
// are saved in the database and used after a restart.
func TestModelsSurviveRestart(t *testing.T) {
	h := newPersistentHarness(t) // created with the built-in default
	if rec := do(t, h.srv.Handler(), http.MethodPost, "/settings", `{"default_model":"claude-fable-5-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("set default: status = %d: %s", rec.Code, rec.Body)
	}
	if rec := h.record(); rec.Model != claude.DefaultModel {
		t.Fatalf("saved model = %q", rec.Model)
	}
	h.stop()
	h.start()
	if s := getSettings(t, h.srv.Handler()); s.DefaultModel != "claude-fable-5-2" {
		t.Fatalf("default model after a restart = %q", s.DefaultModel)
	}
	if got := getWorkspace(t, h.srv.Handler(), h.ws.Name).Model; got != claude.DefaultModel {
		t.Fatalf("workspace model after a restart = %q", got)
	}
	h.waitStatus("ready")
	if resp := h.chat("after the restart"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return h.fake.claude.count() > 0 })
	if task := h.fake.claude.last(); task.Model != claude.DefaultModel {
		t.Fatalf("turn after a restart ran %q", task.Model)
	}
}

// TestDefaultModelNoLongerOffered: a saved default that a later Mosso no
// longer offers falls back to the built-in default.
func TestDefaultModelNoLongerOffered(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "mosso.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.setSetting(settingDefaultModel, "claude-retired-1"); err != nil {
		t.Fatal(err)
	}
	st.close()
	fake, api := newFakeVercel(t)
	srv := newServerOver(t, fake, api, dir)
	if s := getSettings(t, srv.Handler()); s.DefaultModel != claude.DefaultModel {
		t.Fatalf("default = %q", s.DefaultModel)
	}
}

func TestStoreSettingsAndModel(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "mosso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if v, err := st.setting(settingDefaultModel); err != nil || v != "" {
		t.Fatalf("unset setting = %q, %v", v, err)
	}
	for _, v := range []string{"claude-opus-5-5", "claude-haiku-5-5"} {
		if err := st.setSetting(settingDefaultModel, v); err != nil {
			t.Fatal(err)
		}
		if got, err := st.setting(settingDefaultModel); err != nil || got != v {
			t.Fatalf("setting = %q, %v; want %s", got, err, v)
		}
	}

	// A workspace's model is saved with it and never changes.
	rec := testRecord("mosso-0123456789ab")
	rec.Model = "claude-sonnet-5-5"
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	rec.Model = "claude-haiku-5-5"
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.load()
	if err != nil || len(loaded) != 1 || loaded[0].Model != "claude-sonnet-5-5" {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
}

// TestModelMigrationKeepsOldWorkspaces: workspaces saved before models
// could be chosen load with no model, which leaves Claude Code's default.
func TestModelMigrationKeepsOldWorkspaces(t *testing.T) {
	released := migrations
	defer func() { migrations = released }()
	path := filepath.Join(t.TempDir(), "mosso.db")
	migrations = released[:1]
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO workspaces (id, repo, base, branch, sandbox_id, status, state, created_at, updated_at)
		VALUES ('mosso-0123456789ab', 'octo/widgets', 'main', 'mosso/x', 'mosso-0123456789ab', 'terminated', '{}', '2026-05-01T12:00:00.000000Z', '2026-05-01T12:00:00.000000Z')`); err != nil {
		t.Fatal(err)
	}
	st.close()

	migrations = released
	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	loaded, err := st.load()
	if err != nil || len(loaded) != 1 || loaded[0].Model != "" {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	if v, err := st.setting(settingDefaultModel); err != nil || v != "" {
		t.Fatalf("setting after the migration = %q, %v", v, err)
	}
}
