package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/github"
)

// captureLog collects the server's log output for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

type authStatus struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source"`
	Login      string `json:"login"`
	ExpiresAt  string `json:"expires_at"`
	Error      string `json:"error"`
	CreateURL  string `json:"create_url"`
	Storage    string `json:"storage"`
}

func authOf(t *testing.T, body []byte) authStatus {
	t.Helper()
	var st authStatus
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return st
}

// TestGitHubConnectWithoutKeychain is the web UI flow on Linux: the pasted
// token is checked, kept in memory, used for every workspace, and can be
// replaced without a restart.
func TestGitHubConnectWithoutKeychain(t *testing.T) {
	logs := captureLog(t)
	t.Setenv(github.EnvToken, "")
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	api := &fakeGitHubAPI{}
	withGitHub(t, srv, api)
	srv.githubClient = srv.loadGitHubClient // the real lookup, with the fake API
	srv.saveGitHubToken = func(context.Context, github.Token) error { return github.ErrNoKeychain }
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()
	const rotated = "github_pat_11ROTATED_secondvalue0000"

	for body, want := range map[string]int{
		`{`:                              http.StatusBadRequest,
		`{"token":""}`:                   http.StatusBadRequest,
		`{"token":"not a token at all"}`: http.StatusBadRequest,
		`{"token":"` + rotated + `"}`:    http.StatusUnprocessableEntity, // GitHub does not know it yet
	} {
		rec := do(t, h, http.MethodPost, "/github/auth", body)
		if rec.Code != want || strings.Contains(rec.Body.String(), rotated) {
			t.Errorf("POST %s: status = %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
	}

	rec := do(t, h, http.MethodPost, "/github/auth", `{"token":" `+testGitHubToken+` "}`)
	st := authOf(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || !st.Configured || st.Source != github.SourceMemory || st.Login != "octo" ||
		st.ExpiresAt == "" || st.CreateURL != github.NewTokenURL {
		t.Fatalf("connect: status = %d: %s", rec.Code, rec.Body)
	}
	st = authOf(t, do(t, h, http.MethodGet, "/github/auth", "").Body.Bytes())
	if !st.Configured || st.Source != github.SourceMemory || st.Login != "octo" || st.Storage != github.SourceMemory {
		t.Fatalf("status after connect: %+v", st)
	}

	// Every workspace uses the connected token.
	if rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rec.Code, rec.Body)
	}

	// The token is revoked on GitHub: the status says so.
	api.mu.Lock()
	api.token = rotated
	api.mu.Unlock()
	st = authOf(t, do(t, h, http.MethodGet, "/github/auth", "").Body.Bytes())
	if !st.Configured || !strings.Contains(st.Error, "invalid, expired or revoked") {
		t.Fatalf("status with a revoked token: %+v", st)
	}
	// Replacing it takes effect at once, for new and existing workspaces.
	if rec := do(t, h, http.MethodPost, "/github/auth", `{"token":"`+rotated+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if st = authOf(t, do(t, h, http.MethodGet, "/github/auth", "").Body.Bytes()); st.Error != "" || st.Login != "octo" {
		t.Fatalf("status after rotation: %+v", st)
	}
	if rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create workspace after rotation: %d %s", rec.Code, rec.Body)
	}

	for _, tok := range []string{testGitHubToken, rotated} {
		if strings.Contains(logs.String(), tok) {
			t.Fatal("a token was logged")
		}
	}
}

func TestGitHubConnectStoresInKeychain(t *testing.T) {
	srv, _ := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	var saved github.Token
	srv.saveGitHubToken = func(_ context.Context, tok github.Token) error { saved = tok; return nil }
	srv.memoryToken = "github_pat_old_memory_token_000000000"

	rec := do(t, srv.Handler(), http.MethodPost, "/github/auth", `{"token":"`+testGitHubToken+`"}`)
	if st := authOf(t, rec.Body.Bytes()); rec.Code != http.StatusOK || st.Source != github.SourceKeychain || st.Login != "octo" {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if saved.Reveal() != testGitHubToken || srv.memoryToken != "" {
		t.Fatalf("saved %q, memory %q", saved, srv.memoryToken)
	}

	srv.saveGitHubToken = func(context.Context, github.Token) error { return errors.New("user denied access") }
	rec = do(t, srv.Handler(), http.MethodPost, "/github/auth", `{"token":"`+testGitHubToken+`"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "denied") ||
		strings.Contains(rec.Body.String(), testGitHubToken) {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestGitHubConnectRejectsCrossSiteRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/github/auth", strings.NewReader(`token=x`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTurnFetchesFromGitHubFirst(t *testing.T) {
	srv, _, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")

	srv.gitSync = func(*github.Mirror, context.Context, github.Token) (github.Synced, error) {
		return github.Synced{Before: github.Heads{Base: testBaseSHA}, After: github.Heads{Base: testNewBaseSHA}}, nil
	}
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"rebase onto main"}`)
	entries := events.until("result")
	if entries[1].Type != "notice" || entries[1].Text != "Fetched from GitHub: origin/main moved 1111111 → 2222222." || entries[2].Type != "claude" {
		t.Fatalf("entries = %s: %+v", entryTypes(entries), entries[1])
	}

	// A failed fetch is reported, and Claude still runs.
	srv.gitSync = func(*github.Mirror, context.Context, github.Token) (github.Synced, error) {
		return github.Synced{}, errors.New("fetch from GitHub: boom")
	}
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"again"}`)
	entries = events.until("result")
	if entries[1].Type != "notice" || !strings.Contains(entries[1].Text, "Could not fetch the latest changes from GitHub (fetch from GitHub: boom)") ||
		entries[len(entries)-1].Result.IsError {
		t.Fatalf("entries = %s: %+v", entryTypes(entries), entries[1])
	}
}

func TestWorkspacePublishFailureKeepsWorkspaceUsable(t *testing.T) {
	srv, _, ts, ws := newChatServer(t)
	gitPublish := srv.gitPublish
	srv.gitPublish = func(*github.Mirror, context.Context, *github.Client, github.PublishOptions) (*github.Published, error) {
		return nil, github.ErrBranchMoved
	}
	resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/pull-request", `{"title":"t"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.String(); !strings.Contains(got, `"status":"error"`) ||
		!strings.Contains(got, "will not overwrite") {
		t.Fatalf("workspace = %s", got)
	}
	srv.gitPublish = gitPublish
	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/pull-request", `{"title":"t"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry status = %d", resp.StatusCode)
	}
	if got := do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.String(); !strings.Contains(got, `"status":"ready"`) {
		t.Fatalf("workspace = %s", got)
	}
}

func TestWorkspaceDestroyRemovesGitRepository(t *testing.T) {
	srv, _, _, ws := newChatServer(t)
	srv.mu.Lock()
	m := srv.workspaces[ws.Name].git
	srv.mu.Unlock()
	if m == nil {
		t.Fatal("workspace has no Git mirror")
	}
	if rec := do(t, srv.Handler(), http.MethodDelete, "/workspaces/"+ws.Name, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	srv.cleanups.Wait()
	if _, err := m.Sync(context.Background(), testGitHubToken); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("err = %v, want the mirror closed", err)
	}
}
