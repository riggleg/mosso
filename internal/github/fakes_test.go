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
	return append(env, "HOME="+isolatedHome, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
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
}

func newFakeGitHub(t *testing.T, backend, token string, private bool) *fakeGitHub {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, testRepo+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, bare, "", "init", "--quiet", "--bare", "--initial-branch=main")
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
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + isolatedHome},
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
		for _, p := range f.pulls {
			if "octo:"+p["head"].(string) == q.Get("head") && p["base"] == q.Get("base") && p["state"] == "open" {
				out = append(out, p)
			}
		}
		f.json(w, 200, out)
	})
	mux.HandleFunc("POST /repos/octo/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		n := len(f.pulls) + 1
		in["number"], in["state"] = n, "open"
		in["html_url"] = fmt.Sprintf("https://github.com/%s/pull/%d", testRepo, n)
		f.pulls = append(f.pulls, in)
		f.mu.Unlock()
		f.json(w, 201, in)
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
