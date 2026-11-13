package github

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/vercel"
)

const testGitHubToken = "github_pat_11TEST0000000000000000_secretvalue"

type workspaceEnv struct {
	gh      *fakeGitHub
	sandbox *localSandbox
	vc      *vercel.Client
	opts    CloneOptions
	base    string
	mirror  *Mirror
}

// newWorkspace clones the fake repository into a local "sandbox" directory.
func newWorkspace(t *testing.T, private bool) *workspaceEnv {
	t.Helper()
	backend := requireGit(t)
	gh := newFakeGitHub(t, backend, testGitHubToken, private)
	sb := &localSandbox{}
	api := httptest.NewServer(sb)
	t.Cleanup(api.Close)
	vc := vercel.NewClient(vercel.Config{Token: "vercel", TeamID: "team", ProjectID: "prj", BaseURL: api.URL})

	user, err := gh.client().User(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name, email := user.CommitIdentity()
	opts := CloneOptions{
		Repo: testRepo, Base: "main", Branch: NewBranchName(),
		Dir:        filepath.Join(t.TempDir(), "sandbox", "widgets"),
		AuthorName: name, AuthorEmail: email,
	}
	if private {
		opts.Token = Token(testGitHubToken)
	}
	base, err := Clone(context.Background(), vc, "sbx_1", opts)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMirror(vc, "sbx_1", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: base, Branch: opts.Branch, Dir: opts.Dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return &workspaceEnv{gh: gh, sandbox: sb, vc: vc, opts: opts, base: base, mirror: m}
}

func (e *workspaceEnv) publish(t *testing.T, title string, secrets ...string) (*Published, error) {
	t.Helper()
	return e.mirror.Publish(context.Background(), e.gh.client(), PublishOptions{
		Title: title, Body: "Body text.", Secrets: secrets,
	})
}

func (e *workspaceEnv) sync(t *testing.T) Synced {
	t.Helper()
	s, err := e.mirror.Sync(context.Background(), Token(testGitHubToken))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *workspaceEnv) git(t *testing.T, args ...string) string {
	t.Helper()
	return runGit(t, e.opts.Dir, "", args...)
}

func (e *workspaceEnv) remote(t *testing.T, args ...string) string {
	t.Helper()
	return runGit(t, e.gh.bare, "", args...)
}

func TestCloneUsesTokenOnlyInEnvironment(t *testing.T) {
	e := newWorkspace(t, true)
	if e.base != e.remote(t, "rev-parse", "main") {
		t.Fatalf("base = %s, want the remote main commit", e.base)
	}
	if e.gh.gitHit == 0 {
		t.Fatal("the clone should have authenticated to the Git server")
	}
	if got := e.git(t, "branch", "--show-current"); got != e.opts.Branch || !strings.HasPrefix(got, BranchPrefix) {
		t.Fatalf("checked out %q, want %q", got, e.opts.Branch)
	}
	if got := e.git(t, "config", "user.email"); got != "42+octo@users.noreply.github.com" {
		t.Fatalf("user.email = %q", got)
	}
	if got := e.git(t, "config", "user.name"); got != "Octo Cat" {
		t.Fatalf("user.name = %q", got)
	}
	if got := e.git(t, "remote", "get-url", "origin"); got != e.gh.git.URL+"/"+testRepo+".git" {
		t.Fatalf("remote URL = %q", got)
	}
	// Nothing under .git (config, remotes, credentials) keeps the token.
	_ = filepath.Walk(filepath.Join(e.opts.Dir, ".git"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), testGitHubToken) {
				t.Errorf("token written to %s", path)
			}
		}
		return nil
	})
	cmds := e.sandbox.commandsSince(0)
	if len(cmds) != 1 {
		t.Fatalf("ran %d commands, want 1", len(cmds))
	}
	body := e.sandbox.bodies[0]
	if env, _ := body["env"].(map[string]any); env[envCloneToken] != testGitHubToken {
		t.Fatal("token not passed to the clone through its environment")
	}
	if args := strings.Join(toStrings(body["args"]), " "); strings.Contains(args, testGitHubToken) {
		t.Fatal("token leaked into command arguments")
	}
}

func TestClonePublicRepositoryWithoutToken(t *testing.T) {
	e := newWorkspace(t, false)
	if env, _ := e.sandbox.bodies[0]["env"].(map[string]any); env[envCloneToken] != nil {
		t.Fatal("a public clone must not send the token into the sandbox")
	}
}

func TestCloneFailureDoesNotLeakToken(t *testing.T) {
	backend := requireGit(t)
	newFakeGitHub(t, backend, "the-real-token-value", true)
	api := httptest.NewServer(&localSandbox{})
	t.Cleanup(api.Close)
	vc := vercel.NewClient(vercel.Config{Token: "v", TeamID: "t", ProjectID: "p", BaseURL: api.URL})
	_, err := Clone(context.Background(), vc, "sbx_1", CloneOptions{
		Repo: testRepo, Base: "main", Branch: NewBranchName(), Dir: filepath.Join(t.TempDir(), "w"),
		AuthorName: "x", AuthorEmail: "x@example.com", Token: Token(testGitHubToken),
	})
	if err == nil || strings.Contains(err.Error(), testGitHubToken) {
		t.Fatalf("err = %v, want a clone failure without the token", err)
	}
	_, err = Clone(context.Background(), vc, "sbx_1", CloneOptions{
		Repo: testRepo, Base: "no-such-branch", Branch: NewBranchName(), Dir: filepath.Join(t.TempDir(), "w"),
		AuthorName: "x", AuthorEmail: "x@example.com", Token: Token("the-real-token-value"),
	})
	if err == nil || !strings.Contains(err.Error(), "no-such-branch") {
		t.Fatalf("err = %v, want Git's missing-branch error", err)
	}
}

func TestCloneValidatesInput(t *testing.T) {
	vc := vercel.NewClient(vercel.Config{BaseURL: "http://127.0.0.1:1"})
	for name, opts := range map[string]CloneOptions{
		"repo":     {Repo: "octo/../x", Base: "main", Branch: "mosso/x", Dir: "/x"},
		"base":     {Repo: testRepo, Base: "--upload-pack=evil", Branch: "mosso/x", Dir: "/x"},
		"branch":   {Repo: testRepo, Base: "main", Branch: "a..b", Dir: "/x"},
		"relative": {Repo: testRepo, Base: "main", Branch: "mosso/x", Dir: "x"},
	} {
		if _, err := Clone(context.Background(), vc, "sbx", opts); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, i := range items {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}
