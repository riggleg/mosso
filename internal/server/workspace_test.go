package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/riggleg/mosso/internal/github"
)

const (
	testGitHubToken = "github_pat_11SERVERTEST_secretvalue"
	testBaseSHA     = "1111111111111111111111111111111111111111"
	testNewBaseSHA  = "2222222222222222222222222222222222222222"
	testCommitSHA   = "4444444444444444444444444444444444444444"
)

// fakeGitHubAPI is an in-memory GitHub for octo/widgets: the REST
// endpoints Mosso calls directly, plus fakes of the server-side Git
// operations (sync and publish), which internal/github tests with real Git.
type fakeGitHubAPI struct {
	mu       sync.Mutex
	private  bool
	token    string // accepted token; testGitHubToken if empty
	requests []string
	pulls    []map[string]any

	base      string // base branch head on "GitHub"; the clone's if empty
	seen      string // base head the last sync reported
	syncErr   error
	syncs     int
	noChanges bool
	publishes []github.PublishOptions
	pubTokens []github.Token
}

func (f *fakeGitHubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	token := f.token
	if token == "" {
		token = testGitHubToken
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		reply(401, map[string]string{"message": "Bad credentials"})
		return
	}
	w.Header().Set("GitHub-Authentication-Token-Expiration", "2026-12-01 00:00:00 UTC")
	const repo = "/repos/octo/widgets"
	switch p := r.URL.Path; {
	case p == "/user":
		reply(200, map[string]any{"login": "octo", "id": 42, "name": "Octo Cat"})
	case p == "/user/repos":
		reply(200, []map[string]any{{"full_name": "octo/widgets", "default_branch": "main", "private": f.private}})
	case p == repo+"/branches":
		reply(200, []map[string]string{{"name": "main"}, {"name": "dev"}})
	case p == repo:
		reply(200, map[string]any{"full_name": "octo/widgets", "default_branch": "main", "private": f.private})
	default:
		reply(404, map[string]string{"message": "Not Found"})
	}
}

// sync fakes Mirror.Sync: origin/main follows f.base.
func (f *fakeGitHubAPI) sync(m *github.Mirror, _ context.Context, tok github.Token) (github.Synced, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	if f.syncErr != nil {
		return github.Synced{}, f.syncErr
	}
	if tok.Reveal() == "" {
		return github.Synced{}, errors.New("sync without a token")
	}
	if f.seen == "" {
		f.seen = m.Options().BaseSHA
	}
	if f.base == "" {
		f.base = f.seen
	}
	out := github.Synced{Before: github.Heads{Base: f.seen}, After: github.Heads{Base: f.base}}
	f.seen = f.base
	return out, nil
}

// publish fakes Mirror.Publish: it opens pull request #7 once.
func (f *fakeGitHubAPI) publish(m *github.Mirror, _ context.Context, gh *github.Client, opts github.PublishOptions) (*github.Published, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, opts)
	f.pubTokens = append(f.pubTokens, gh.Token())
	if f.noChanges {
		return nil, github.ErrNoChanges
	}
	branch := m.Options().Branch
	out := &github.Published{Branch: branch, Commit: testCommitSHA,
		PullRequest: github.PullRequest{Number: 7, URL: "https://github.com/octo/widgets/pull/7"}}
	if len(f.pulls) == 0 {
		f.pulls = append(f.pulls, map[string]any{"head": branch, "base": "main", "title": opts.Title, "draft": opts.Draft})
		out.Created = true
	}
	return out, nil
}

// fakeRepoSandbox scripts the sandbox side of a workspace: the clone
// prints the base commit; anything else is a Claude Code run.
func fakeRepoSandbox() func(cmd map[string]any) string {
	return func(cmd map[string]any) string {
		args, _ := json.Marshal(cmd["args"])
		if strings.Contains(string(args), "clone --quiet") {
			return testBaseSHA + "\n"
		}
		return fakeClaude(cmd)
	}
}

func withGitHub(t *testing.T, srv *Server, api *fakeGitHubAPI) {
	t.Helper()
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	srv.newGitHubClient = func(tok github.Token) *github.Client {
		c := github.NewClient(tok)
		c.BaseURL = ts.URL
		return c
	}
	srv.githubClient = func(context.Context) (*github.Client, string, error) {
		return srv.newGitHubClient(testGitHubToken), github.SourceKeychain, nil
	}
	srv.gitSync = api.sync
	srv.gitPublish = api.publish
}

type testWorkspace struct {
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	Base        string `json:"base"`
	BaseSHA     string `json:"base_sha"`
	Branch      string `json:"branch"`
	Dir         string `json:"dir"`
	PullRequest *struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	} `json:"pull_request"`
}

func TestWorkspaceToPullRequest(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	api := &fakeGitHubAPI{}
	withGitHub(t, srv, api)
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()

	rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	if ws.Repo != "octo/widgets" || ws.Base != "main" || ws.BaseSHA != testBaseSHA ||
		!strings.HasPrefix(ws.Branch, "mosso/") || ws.Dir != "/vercel/sandbox/widgets" || !strings.HasPrefix(ws.Name, "mosso-") {
		t.Fatalf("unexpected workspace: %s", rec.Body)
	}
	clone, _ := json.Marshal(fake.cmds[0])
	if !strings.Contains(string(clone), "https://github.com/octo/widgets.git") ||
		!strings.Contains(string(clone), "42+octo@users.noreply.github.com") {
		t.Fatalf("unexpected clone command: %s", clone)
	}

	rec = do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"improve the readme"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "you said improve the readme") {
		t.Fatalf("message status = %d: %s", rec.Code, rec.Body)
	}
	run := fake.cmds[len(fake.cmds)-1]
	if run["cwd"] != ws.Dir {
		t.Fatalf("claude ran in %v, want %s", run["cwd"], ws.Dir)
	}
	// Claude is told how Git works here, and origin/* was refreshed first.
	if args, _ := json.Marshal(run["args"]); !strings.Contains(string(args), "--append-system-prompt") ||
		!strings.Contains(string(args), "origin/main") || !strings.Contains(string(args), ws.Branch) {
		t.Fatalf("claude args lack the workspace prompt: %s", args)
	}
	if api.syncs != 2 { // once while creating, once before the turn
		t.Fatalf("syncs = %d, want 2", api.syncs)
	}

	rec = do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{"title":"Improve README","draft":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("pull request status = %d: %s", rec.Code, rec.Body)
	}
	var pub github.Published
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if pub.URL != "https://github.com/octo/widgets/pull/7" || pub.Number != 7 || !pub.Created ||
		pub.Branch != ws.Branch || pub.Commit != testCommitSHA {
		t.Fatalf("unexpected result: %s", rec.Body)
	}
	if pr := api.pulls[0]; pr["head"] != ws.Branch || pr["base"] != "main" || pr["draft"] != true || pr["title"] != "Improve README" {
		t.Fatalf("unexpected pull request: %v", pr)
	}

	// Publishes refuse to push the Claude token, and use the GitHub token.
	if p := api.publishes[0]; p.Title != "Improve README" || !p.Draft || len(p.Secrets) != 1 || p.Secrets[0] != testClaudeToken {
		t.Fatalf("unexpected publish options: %+v", p)
	}
	if api.pubTokens[0].Reveal() != testGitHubToken {
		t.Fatal("publish did not use the GitHub token")
	}

	// Again: the same pull request is updated.
	rec = do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("second pull request status = %d: %s", rec.Code, rec.Body)
	}
	if p := api.publishes[1]; !strings.HasPrefix(p.Title, "Changes from Mosso workspace ") {
		t.Fatalf("default title = %q", p.Title)
	}

	rec = do(t, h, http.MethodGet, "/workspaces/"+ws.Name, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	if ws.PullRequest == nil || ws.PullRequest.Number != 7 {
		t.Fatalf("workspace does not record the pull request: %s", rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), ws.Name) {
		t.Fatalf("list missing workspace: %s", rec.Body)
	}

	// The GitHub token never reaches the sandbox for a public repository.
	for _, c := range fake.cmds {
		if b, _ := json.Marshal(c); strings.Contains(string(b), testGitHubToken) {
			t.Fatalf("GitHub token sent into the sandbox: %s", b)
		}
	}
	if strings.Contains(rec.Body.String(), testGitHubToken) {
		t.Fatal("response leaks the GitHub token")
	}

	if rec := do(t, h, http.MethodDelete, "/workspaces/"+ws.Name, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != ws.Name {
		t.Fatalf("deleted = %v", fake.deleted)
	}
	if rec := do(t, h, http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), `"workspaces":[]`) {
		t.Fatalf("workspace not forgotten: %s", rec.Body)
	}
}

func TestWorkspacePrivateRepoSendsTokenOnlyToClone(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{private: true})
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()

	rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets","base":"main"}`)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), testGitHubToken) {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"hi"}`)
	do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{"title":"t"}`)

	for i, c := range fake.cmds {
		env, _ := c["env"].(map[string]any)
		args, _ := json.Marshal(c["args"])
		if strings.Contains(string(args), testGitHubToken) {
			t.Fatalf("command %d has the token in its arguments", i)
		}
		hasToken := false
		for _, v := range env {
			hasToken = hasToken || v == testGitHubToken
		}
		if isClone := strings.Contains(string(args), "clone --quiet"); hasToken != isClone {
			t.Fatalf("command %d (clone=%t) has token=%t; only the clone may", i, isClone, hasToken)
		}
	}
}

func TestWorkspacePullRequestWithoutChanges(t *testing.T) {
	srv, fake := newTestServer(t)
	api := &fakeGitHubAPI{}
	withGitHub(t, srv, api)
	api.noChanges = true
	fake.stdoutFor = fakeRepoSandbox()
	h := srv.Handler()
	var ws testWorkspace
	_ = json.Unmarshal(do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`).Body.Bytes(), &ws)

	rec := do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{"title":"Nothing"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no changes") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(api.pulls) != 0 {
		t.Fatal("nothing should be opened")
	}
}

func TestWorkspaceCloneFailureDestroysSandbox(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = fakeRepoSandbox()
	fake.exitFor = func(map[string]any) int { return 128 }
	h := srv.Handler()

	rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "exit code 128") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(fake.created) != 1 || len(fake.deleted) != 1 || fake.deleted[0] != fake.created[0] {
		t.Fatalf("created=%v deleted=%v, want the sandbox destroyed", fake.created, fake.deleted)
	}
	if rec := do(t, h, http.MethodGet, "/sandboxes", ""); !strings.Contains(rec.Body.String(), `"sandboxes":[]`) {
		t.Fatalf("sandbox still tracked: %s", rec.Body)
	}
}

func TestWorkspaceValidation(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	h := srv.Handler()
	for name, tc := range map[string]struct {
		method, path, body string
		want               int
	}{
		"bad json":        {"POST", "/workspaces", `{`, http.StatusBadRequest},
		"no repo":         {"POST", "/workspaces", `{}`, http.StatusBadRequest},
		"bad repo":        {"POST", "/workspaces", `{"repo":"https://github.com/octo/widgets"}`, http.StatusBadRequest},
		"bad base":        {"POST", "/workspaces", `{"repo":"octo/widgets","base":"--upload-pack=x"}`, http.StatusBadRequest},
		"bad timeout":     {"POST", "/workspaces", `{"repo":"octo/widgets","timeout_seconds":99999}`, http.StatusBadRequest},
		"unknown repo":    {"POST", "/workspaces", `{"repo":"octo/missing"}`, http.StatusNotFound},
		"unknown message": {"POST", "/workspaces/nope/claude/messages", `{"prompt":"hi"}`, http.StatusNotFound},
		"unknown pr":      {"POST", "/workspaces/nope/pull-request", `{}`, http.StatusNotFound},
		"unknown get":     {"GET", "/workspaces/nope", ``, http.StatusNotFound},
		"unknown sandbox": {"DELETE", "/workspaces/nope", ``, http.StatusNotFound},
	} {
		if rec := do(t, h, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rec.Code, tc.want, rec.Body)
		}
	}
	if len(fake.created) != 0 {
		t.Fatalf("invalid requests created %d sandboxes", len(fake.created))
	}
}

func TestWorkspaceWithoutGitHubToken(t *testing.T) {
	srv, fake := newTestServer(t)
	srv.githubClient = func(context.Context) (*github.Client, string, error) { return nil, "", github.ErrNoToken }
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "mosso auth github") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(fake.created) != 0 {
		t.Fatal("no sandbox should be created without a GitHub token")
	}
}

func TestGitHubAuthStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	rec := do(t, srv.Handler(), http.MethodGet, "/github/auth", "")
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || strings.Contains(string(body), testGitHubToken) ||
		!strings.Contains(string(body), `"configured":true`) || !strings.Contains(string(body), `"login":"octo"`) {
		t.Fatalf("status = %d: %s", rec.Code, body)
	}

	srv.githubClient = func(context.Context) (*github.Client, string, error) { return nil, "", github.ErrNoToken }
	rec = do(t, srv.Handler(), http.MethodGet, "/github/auth", "")
	if !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("unexpected body: %s", rec.Body)
	}
}
