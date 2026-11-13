package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	testBaseTree    = "2222222222222222222222222222222222222222"
	testHeadTree    = "3333333333333333333333333333333333333333"
	testCommitSHA   = "4444444444444444444444444444444444444444"
	testBlobSHA     = "5555555555555555555555555555555555555555"
)

// fakeGitHubAPI is an in-memory GitHub REST API for octo/widgets.
type fakeGitHubAPI struct {
	mu       sync.Mutex
	private  bool
	requests []string
	head     string // working branch head; "" until created
	pulls    []map[string]any
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
	if r.Header.Get("Authorization") != "Bearer "+testGitHubToken {
		reply(401, map[string]string{"message": "Bad credentials"})
		return
	}
	const repo = "/repos/octo/widgets"
	p := r.URL.Path
	switch {
	case p == "/user":
		reply(200, map[string]any{"login": "octo", "id": 42, "name": "Octo Cat"})
	case p == repo:
		reply(200, map[string]any{"full_name": "octo/widgets", "default_branch": "main", "private": f.private})
	case strings.HasPrefix(p, repo+"/git/ref/heads/"):
		if f.head == "" {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		reply(200, map[string]any{"object": map[string]string{"sha": f.head}})
	case strings.HasPrefix(p, repo+"/git/commits/"):
		reply(200, map[string]any{"tree": map[string]string{"sha": testHeadTree}})
	case p == repo+"/git/blobs":
		reply(201, map[string]string{"sha": testBlobSHA})
	case p == repo+"/git/trees":
		reply(201, map[string]string{"sha": testHeadTree})
	case p == repo+"/git/commits":
		reply(201, map[string]string{"sha": testCommitSHA})
	case p == repo+"/git/refs" || strings.HasPrefix(p, repo+"/git/refs/heads/"):
		f.head = testCommitSHA
		reply(201, map[string]string{})
	case p == repo+"/pulls" && r.Method == http.MethodGet:
		reply(200, f.pulls)
	case p == repo+"/pulls" && r.Method == http.MethodPost:
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		in["number"], in["html_url"] = 7, "https://github.com/octo/widgets/pull/7"
		f.pulls = append(f.pulls, in)
		reply(201, in)
	default:
		reply(404, map[string]string{"message": "Not Found"})
	}
}

// fakeRepoSandbox scripts the sandbox side of a workspace: clone prints the
// base commit, commit reports one modified file (unless noChanges), and the
// read script returns its content. Anything else is a Claude Code run.
func fakeRepoSandbox(noChanges *bool) func(cmd map[string]any) string {
	return func(cmd map[string]any) string {
		args, _ := json.Marshal(cmd["args"])
		switch s := string(args); {
		case strings.Contains(s, "clone --quiet"):
			return testBaseSHA + "\n"
		case strings.Contains(s, "git diff-tree"):
			diff := ""
			if !*noChanges {
				diff = ":100644 100644 " + testBaseSHA + " " + testBlobSHA + " M\x00README.md\x00"
			}
			return testBaseTree + "\n" + testHeadTree + "\n" + base64.StdEncoding.EncodeToString([]byte(diff)) + "\n"
		case strings.Contains(s, "cat-file --batch-check"):
			return base64.StdEncoding.EncodeToString([]byte("# widgets, improved\n")) + "\n"
		}
		return fakeClaude(cmd)
	}
}

func withGitHub(t *testing.T, srv *Server, api *fakeGitHubAPI) {
	t.Helper()
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	srv.githubClient = func(context.Context) (*github.Client, string, error) {
		c := github.NewClient(testGitHubToken)
		c.BaseURL = ts.URL
		return c, github.SourceKeychain, nil
	}
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
	noChanges := false
	fake.stdoutFor = fakeRepoSandbox(&noChanges)
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
	if run := fake.cmds[len(fake.cmds)-1]; run["cwd"] != ws.Dir {
		t.Fatalf("claude ran in %v, want %s", run["cwd"], ws.Dir)
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

	// Again with nothing new: same pull request, nothing pushed.
	pushes := len(api.requests)
	rec = do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("second pull request status = %d: %s", rec.Code, rec.Body)
	}
	for _, r := range api.requests[pushes:] {
		if strings.HasPrefix(r, "POST") || strings.HasPrefix(r, "PATCH") {
			t.Fatalf("unexpected write on a no-op publish: %s", r)
		}
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
	noChanges := false
	fake.stdoutFor = fakeRepoSandbox(&noChanges)
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
	noChanges := true
	fake.stdoutFor = fakeRepoSandbox(&noChanges)
	h := srv.Handler()
	var ws testWorkspace
	_ = json.Unmarshal(do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`).Body.Bytes(), &ws)

	rec := do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{"title":"Nothing"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no changes") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(api.pulls) != 0 || api.head != "" {
		t.Fatal("nothing should be pushed or opened")
	}
}

func TestWorkspaceCloneFailureDestroysSandbox(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	noChanges := false
	fake.stdoutFor = fakeRepoSandbox(&noChanges)
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
