package github

import (
	"context"
	"errors"
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
	return &workspaceEnv{gh: gh, sandbox: sb, vc: vc, opts: opts, base: base}
}

func (e *workspaceEnv) publish(t *testing.T, title string, secrets ...string) (*Published, error) {
	t.Helper()
	return Publish(context.Background(), e.vc, "sbx_1", e.gh.client(), PublishOptions{
		Repo: testRepo, Base: "main", BaseSHA: e.base, Branch: e.opts.Branch, Dir: e.opts.Dir,
		Title: title, Body: "Body text.", Secrets: secrets,
	})
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

func TestPublishCreatesAndUpdatesPullRequest(t *testing.T) {
	e := newWorkspace(t, true)
	dir := e.opts.Dir

	// Claude commits some work itself and leaves the rest uncommitted.
	writeFile(t, filepath.Join(dir, "README.md"), "# widgets\r\n\nNow with CRLF and ünïcode.\n")
	e.git(t, "commit", "--quiet", "-am", "claude: update readme")
	if err := os.Remove(filepath.Join(dir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "src/new/widget.go"), "package widget\n")
	writeFile(t, filepath.Join(dir, "bin/data.bin"), "\x00\x01\xff\xfebinary")
	writeFile(t, filepath.Join(dir, "tool.sh"), "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(dir, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "empty.txt"), "")
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	before := len(e.sandbox.bodies)

	pub, err := e.publish(t, "Add widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Created || pub.Number != 1 || pub.URL != "https://github.com/octo/widgets/pull/1" || pub.Branch != e.opts.Branch {
		t.Fatalf("unexpected result: %+v", pub)
	}
	ref := "refs/heads/" + e.opts.Branch
	if got := e.remote(t, "rev-parse", ref); got != pub.Commit {
		t.Fatalf("remote branch = %s, want %s", got, pub.Commit)
	}
	if got, want := e.remote(t, "rev-parse", ref+"^{tree}"), e.git(t, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("remote tree %s != sandbox tree %s", got, want)
	}
	if got := e.remote(t, "rev-parse", ref+"^"); got != e.base {
		t.Fatalf("first commit parent = %s, want the base %s", got, e.base)
	}
	if got := e.remote(t, "log", "-1", "--format=%B", ref); got != "Add widgets\n\nBody text." {
		t.Fatalf("commit message = %q", got)
	}
	if got := e.remote(t, "rev-parse", "main"); got != e.base {
		t.Fatal("the base branch must not change")
	}
	if got := e.git(t, "status", "--porcelain"); got != "" {
		t.Fatalf("sandbox has uncommitted work after publish: %q", got)
	}
	pr := e.gh.pulls[0]
	if pr["head"] != e.opts.Branch || pr["base"] != "main" || pr["title"] != "Add widgets" || pr["draft"] != false {
		t.Fatalf("unexpected pull request: %v", pr)
	}
	for _, c := range e.sandbox.commandsSince(before) {
		if strings.Contains(c, testGitHubToken) {
			t.Fatalf("publish sent the token into the sandbox: %s", c)
		}
	}
	for _, a := range e.gh.auths {
		if a != "Bearer "+testGitHubToken {
			t.Fatalf("unexpected Authorization header %q", a)
		}
	}

	// More work goes to the same pull request as a new commit.
	writeFile(t, filepath.Join(dir, "src/new/widget.go"), "package widget\n\nconst N = 2\n")
	again, err := e.publish(t, "Follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Number != 1 || again.Commit == pub.Commit || len(e.gh.pulls) != 1 {
		t.Fatalf("unexpected follow-up: %+v", again)
	}
	if got := e.remote(t, "rev-parse", ref+"^"); got != pub.Commit {
		t.Fatalf("follow-up parent = %s, want %s", got, pub.Commit)
	}
	if got, want := e.remote(t, "rev-parse", ref+"^{tree}"), e.git(t, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("remote tree %s != sandbox tree %s", got, want)
	}

	// Nothing new: no new commit, same pull request.
	same, err := e.publish(t, "Nothing new")
	if err != nil {
		t.Fatal(err)
	}
	if same.Created || same.Commit != again.Commit || same.Number != 1 {
		t.Fatalf("unexpected no-op publish: %+v", same)
	}
}

func TestPublishWithoutChanges(t *testing.T) {
	e := newWorkspace(t, false)
	if _, err := e.publish(t, "Nothing"); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges", err)
	}
	if out, _ := gitCmd(e.gh.bare, "", nil, "branch", "--list", "mosso/*"); out != "" {
		t.Fatalf("no branch should be created: %q", out)
	}
	if len(e.gh.pulls) != 0 {
		t.Fatal("no pull request should be opened")
	}
}

func TestPublishRefusesCredentials(t *testing.T) {
	const claudeToken = "sk-ant-oat01-claude-secret-token"
	for name, content := range map[string]string{
		"github token": "token=" + testGitHubToken + "\n",
		"claude token": "CLAUDE=" + claudeToken + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newWorkspace(t, false)
			writeFile(t, filepath.Join(e.opts.Dir, "leak.env"), content)
			_, err := e.publish(t, "Leak", claudeToken)
			if err == nil || !strings.Contains(err.Error(), "leak.env") ||
				strings.Contains(err.Error(), testGitHubToken) || strings.Contains(err.Error(), claudeToken) {
				t.Fatalf("err = %v", err)
			}
			if out, _ := gitCmd(e.gh.bare, "", nil, "branch", "--list", "mosso/*"); out != "" {
				t.Fatalf("nothing should be pushed: %q", out)
			}
		})
	}
}

func TestPublishValidatesInput(t *testing.T) {
	c := NewClient("tok")
	vc := vercel.NewClient(vercel.Config{BaseURL: "http://127.0.0.1:1"})
	sha := strings.Repeat("a", 40)
	for name, opts := range map[string]PublishOptions{
		"not a mosso branch": {Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "main", Title: "t"},
		"bad base sha":       {Repo: testRepo, Base: "main", BaseSHA: "HEAD", Branch: "mosso/x", Title: "t"},
		"no title":           {Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "mosso/x", Title: " "},
	} {
		if _, err := Publish(context.Background(), vc, "sbx", c, opts); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestParseDiffTreeRejectsSubmodules(t *testing.T) {
	raw := ":000000 160000 " + strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " A\x00vendor/lib\x00"
	if _, err := parseDiffTree([]byte(raw)); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("err = %v", err)
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
