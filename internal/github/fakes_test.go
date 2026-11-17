package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/riggleg/mosso/internal/vercel"
)

const testRepo = "octo/widgets"

// runGit runs git in dir with an isolated configuration.
func runGit(t *testing.T, dir string, stdin string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, stdin, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func gitCmd(dir, stdin string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(isolatedEnv(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

var isolatedHome string

// isolatedEnv keeps the developer's Git configuration out of the tests.
func isolatedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+isolatedHome, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	return append(env, noBackgroundGit...)
}

// noBackgroundGit turns off Git's automatic maintenance and garbage
// collection. Commands such as commit, fetch and cherry-pick otherwise
// start "git maintenance run --auto --detach" (and receive-pack "git gc
// --auto"), which can outlive the test and still be writing in a
// repository when t.TempDir removes it ("unlinkat …/objects/pack:
// directory not empty").
var noBackgroundGit = []string{
	"GIT_CONFIG_COUNT=3",
	"GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
	"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
	"GIT_CONFIG_KEY_2=receive.autogc", "GIT_CONFIG_VALUE_2=false",
}

func requireGit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	isolatedHome = t.TempDir()
	return backend
}

// localSandbox is a fake Vercel Sandbox API that runs each command on this
// machine, so the real Git scripts are exercised.
type localSandbox struct {
	mu      sync.Mutex
	bodies  []map[string]any
	results []vercel.CommandResult
}

func (f *localSandbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cmd"):
		var body struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Cwd     string            `json:"cwd"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		var generic map[string]any
		_ = json.Unmarshal(raw, &generic)
		f.bodies = append(f.bodies, generic)

		cmd := exec.Command(body.Command, body.Args...)
		cmd.Dir = body.Cwd
		cmd.Env = isolatedEnv()
		for k, v := range body.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			code = -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			stderr.WriteString(err.Error())
		}
		id := fmt.Sprintf("cmd_%d", len(f.results))
		f.results = append(f.results, vercel.CommandResult{CommandID: id, ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()})
		_, _ = fmt.Fprintf(w, `{"command":{"id":%q,"exitCode":%d}}`, id, code)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
		parts := strings.Split(r.URL.Path, "/")
		var i int
		_, _ = fmt.Sscanf(parts[len(parts)-2], "cmd_%d", &i)
		res := f.results[i]
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]string{"stream": "stdout", "data": res.Stdout})
		_ = enc.Encode(map[string]string{"stream": "stderr", "data": res.Stderr})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// commandsSince returns the request bodies of commands run after index n.
func (f *localSandbox) commandsSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.bodies[n:] {
		raw, _ := json.Marshal(b)
		out = append(out, string(raw))
	}
	return out
}

// fakeGitHub implements the GitHub REST endpoints Mosso uses on top of a
// real bare repository, and serves that repository over smart HTTP with
// basic auth, like github.com.
type fakeGitHub struct {
	t     *testing.T
	bare  string
	token string
	api   *httptest.Server
	git   *httptest.Server
	mu    sync.Mutex
	pulls []map[string]any
	auths []string // Authorization headers seen by the API
	// gitHit counts authenticated Git HTTP requests.
	gitHit int
	// pushStatus, if set, is the HTTP status for every push request, to
	// simulate a token without write access.
	pushStatus int
	// checkRuns are the check runs of every commit; statusForbidden
	// makes commit statuses unreadable, like a token without the
	// Commit statuses permission.
	checkRuns       []map[string]any
	statusForbidden bool
}

// pull returns pull request number n. Call with f.mu held.
func (f *fakeGitHub) pull(n string) map[string]any {
	for _, p := range f.pulls {
		if fmt.Sprint(p["number"]) == n {
			return p
		}
	}
	return nil
}

// apiPull renders a stored pull request like GitHub's REST API.
func (f *fakeGitHub) apiPull(p map[string]any) map[string]any {
	head, _ := p["head"].(string)
	sha, _ := gitCmd(f.bare, "", nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+head)
	return map[string]any{
		"number": p["number"], "html_url": p["html_url"], "node_id": fmt.Sprintf("PR_%v", p["number"]),
		"title": p["title"], "body": p["body"], "state": p["state"], "draft": p["draft"] == true,
		"merged_at": nil,
		"head":      map[string]any{"ref": head, "sha": sha, "repo": map[string]any{"full_name": testRepo}},
		"base":      map[string]any{"ref": p["base"]},
	}
}

func newFakeGitHub(t *testing.T, backend, token string, private bool) *fakeGitHub {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, testRepo+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, bare, "", "init", "--quiet", "--bare", "--initial-branch=main")
	// Pushes into it over the local transport drop the GIT_CONFIG_*
	// variables of noBackgroundGit on the receiving side, so it keeps the
	// same settings in its own configuration.
	for _, kv := range [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}, {"receive.autogc", "false"}} {
		runGit(t, bare, "", "config", kv[0], kv[1])
	}
	work := t.TempDir()
	runGit(t, work, "", "init", "--quiet", "--initial-branch=main")
	writeFile(t, filepath.Join(work, "README.md"), "# widgets\n")
	writeFile(t, filepath.Join(work, "old.txt"), "remove me\n")
	writeFile(t, filepath.Join(work, "docs/guide.md"), "guide\n")
	runGit(t, work, "", "add", "-A")
	runGit(t, work, "", "commit", "--quiet", "-m", "initial")
	runGit(t, work, "", "push", "--quiet", bare, "main")
	runGit(t, bare, "", "config", "http.receivepack", "true")

	f := &fakeGitHub{t: t, bare: bare, token: token}
	gitHandler := &cgi.Handler{
		Path: backend,
		Env:  append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + isolatedHome}, noBackgroundGit...),
	}
	f.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Like GitHub, pushes always need the token.
		push := strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		f.mu.Lock()
		pushStatus := f.pushStatus
		f.mu.Unlock()
		if push && pushStatus != 0 {
			w.WriteHeader(pushStatus)
			_, _ = io.WriteString(w, "remote: Permission to octo/widgets.git denied to octo.\n")
			return
		}
		if private || push {
			if _, pass, ok := r.BasicAuth(); !ok || pass != token {
				w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			f.gitHit++
			f.mu.Unlock()
		}
		gitHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.git.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, 200, map[string]any{"login": "octo", "id": 42, "name": "Octo Cat"})
	})
	mux.HandleFunc("GET /repos/octo/widgets", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, 200, map[string]any{"full_name": testRepo, "default_branch": "main", "private": private})
	})
	mux.HandleFunc("GET /repos/octo/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for i := len(f.pulls) - 1; i >= 0; i-- {
			p := f.pulls[i]
			if "octo:"+p["head"].(string) != q.Get("head") || (q.Get("base") != "" && p["base"] != q.Get("base")) {
				continue
			}
			if st := q.Get("state"); st != "all" && p["state"] != st {
				continue
			}
			out = append(out, f.apiPull(p))
		}
		f.json(w, 200, out)
	})
	mux.HandleFunc("POST /repos/octo/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		head, _ := in["head"].(string)
		if f.must("rev-parse", "--verify", "--quiet", "refs/heads/"+head) == "" {
			f.json(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"field": "head", "code": "invalid"}}})
			return
		}
		n := len(f.pulls) + 1
		in["number"], in["state"] = n, "open"
		in["html_url"] = fmt.Sprintf("https://github.com/%s/pull/%d", testRepo, n)
		f.pulls = append(f.pulls, in)
		f.json(w, 201, f.apiPull(in))
	})
	mux.HandleFunc("GET /repos/octo/widgets/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := f.pull(r.PathValue("n"))
		if p == nil {
			f.json(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		out := f.apiPull(p)
		out["mergeable"], out["mergeable_state"] = true, "clean"
		f.json(w, 200, out)
	})
	mux.HandleFunc("PATCH /repos/octo/widgets/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		p := f.pull(r.PathValue("n"))
		if p == nil {
			f.json(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		for _, k := range []string{"title", "body"} {
			if v, ok := in[k]; ok {
				p[k] = v
			}
		}
		f.json(w, 200, f.apiPull(p))
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		id, _ := in.Variables["id"].(string)
		p := f.pull(strings.TrimPrefix(id, "PR_"))
		if p == nil {
			f.json(w, 200, map[string]any{"errors": []map[string]string{{"message": "Could not resolve to a node"}}})
			return
		}
		p["draft"] = strings.Contains(in.Query, "convertPullRequestToDraft")
		f.json(w, 200, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("GET /repos/octo/widgets/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		sha, err := gitCmd(f.bare, "", nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.PathValue("branch"))
		if err != nil || sha == "" {
			f.json(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		f.json(w, 200, map[string]any{"ref": "refs/heads/" + r.PathValue("branch"), "object": map[string]any{"sha": sha, "type": "commit"}})
	})
	mux.HandleFunc("GET /repos/octo/widgets/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.json(w, 200, map[string]any{"total_count": len(f.checkRuns), "check_runs": f.checkRuns})
	})
	mux.HandleFunc("GET /repos/octo/widgets/commits/{sha}/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.statusForbidden {
			f.json(w, 403, map[string]any{"message": "Resource not accessible by personal access token"})
			return
		}
		f.json(w, 200, map[string]any{"state": "success", "statuses": []any{}})
	})
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			f.json(w, 401, map[string]any{"message": "Bad credentials"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.api.Close)

	orig := gitBaseURL
	gitBaseURL = f.git.URL
	t.Cleanup(func() { gitBaseURL = orig })
	return f
}

func (f *fakeGitHub) client() *Client {
	c := NewClient(Token(f.token))
	c.BaseURL = f.api.URL
	return c
}

func (f *fakeGitHub) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) must(args ...string) string {
	out, err := gitCmd(f.bare, "", nil, args...)
	if err != nil {
		f.t.Errorf("fake github: git %v: %v", args, err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
