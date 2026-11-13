package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// remoteCommit commits a file on branch directly on the fake GitHub, as if
// someone else pushed, and returns the new commit.
func (e *workspaceEnv) remoteCommit(t *testing.T, branch, file, content string) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "", "clone", "--quiet", "--branch", branch, e.gh.bare, ".")
	writeFile(t, filepath.Join(work, file), content)
	runGit(t, work, "", "commit", "--quiet", "-am", "upstream: "+file)
	runGit(t, work, "", "push", "--quiet", "origin", branch)
	return runGit(t, work, "", "rev-parse", "HEAD")
}

// noTokenInSandbox fails if any sandbox command since n carried the token.
func (e *workspaceEnv) noTokenInSandbox(t *testing.T, n int) {
	t.Helper()
	for _, c := range e.sandbox.commandsSince(n) {
		if strings.Contains(c, testGitHubToken) {
			t.Fatalf("the token was sent into the sandbox: %.300s", c)
		}
	}
}

func TestSyncFetchesIntoSandboxWithoutToken(t *testing.T) {
	e := newWorkspace(t, true)
	before := len(e.sandbox.bodies)

	// Nothing new on GitHub: nothing to change.
	if s := e.sync(t); s.Changed() || s.After.Base != e.base || s.After.Branch != "" {
		t.Fatalf("unexpected sync: %+v", s)
	}
	// Main advances on GitHub; the sandbox's origin/main follows.
	upstream := e.remoteCommit(t, "main", "README.md", "# widgets\nupstream\n")
	s := e.sync(t)
	if !s.Changed() || s.Before.Base != e.base || s.After.Base != upstream {
		t.Fatalf("unexpected sync: %+v", s)
	}
	if got := e.git(t, "rev-parse", "origin/main"); got != upstream {
		t.Fatalf("origin/main = %s, want %s", got, upstream)
	}
	if got := e.git(t, "show", "origin/main:README.md"); !strings.Contains(got, "upstream") {
		t.Fatalf("origin/main content = %q", got)
	}
	// The working tree and branch are untouched.
	if got := e.git(t, "rev-parse", "HEAD"); got != e.base {
		t.Fatalf("HEAD moved to %s", got)
	}
	e.noTokenInSandbox(t, before)
	if e.gh.gitHit == 0 {
		t.Fatal("the server should have authenticated to the Git server")
	}

	// Close removes the server-side repository and stops further use.
	dir := e.mirror.local
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.mirror.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("server-side repository not removed: %v", err)
	}
	if _, err := e.mirror.Sync(context.Background(), Token(testGitHubToken)); err == nil {
		t.Fatal("sync after Close should fail")
	}
}

func TestSyncRecoversFromMissingCommits(t *testing.T) {
	e := newWorkspace(t, false)
	// Mosso believes the sandbox has a commit it does not.
	side := e.remoteCommit(t, "main", "docs/guide.md", "side\n")
	e.sync(t)
	e.git(t, "update-ref", "-d", "refs/remotes/origin/main")
	e.git(t, "reflog", "expire", "--expire=now", "--all")
	e.git(t, "gc", "--quiet", "--prune=now")
	if _, err := gitCmd(e.opts.Dir, "", nil, "cat-file", "-e", side); err == nil {
		t.Fatal("test setup: the sandbox still has the commit")
	}
	upstream := e.remoteCommit(t, "main", "README.md", "next\n")
	e.sync(t)
	if got := e.git(t, "rev-parse", "origin/main"); got != upstream {
		t.Fatalf("origin/main = %s, want %s", got, upstream)
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
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	before := len(e.sandbox.bodies)

	pub, err := e.publish(t, "Add widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Created || pub.Forced || pub.Number != 1 || pub.URL != "https://github.com/octo/widgets/pull/1" || pub.Branch != e.opts.Branch {
		t.Fatalf("unexpected result: %+v", pub)
	}
	ref := "refs/heads/" + e.opts.Branch
	// The sandbox's commits are pushed as they are, history included.
	if got, want := e.remote(t, "rev-parse", ref), e.git(t, "rev-parse", "HEAD"); got != want || pub.Commit != want {
		t.Fatalf("remote branch = %s (result %s), want the sandbox HEAD %s", got, pub.Commit, want)
	}
	if got := e.remote(t, "log", "--format=%s", "main.."+ref); got != "Add widgets\nclaude: update readme" {
		t.Fatalf("pushed commits = %q", got)
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
	if got := e.git(t, "rev-parse", "origin/"+e.opts.Branch); got != pub.Commit {
		t.Fatalf("sandbox origin/%s = %s, want %s", e.opts.Branch, got, pub.Commit)
	}
	pr := e.gh.pulls[0]
	if pr["head"] != e.opts.Branch || pr["base"] != "main" || pr["title"] != "Add widgets" || pr["draft"] != false {
		t.Fatalf("unexpected pull request: %v", pr)
	}
	e.noTokenInSandbox(t, before)
	for _, a := range e.gh.auths {
		if a != "Bearer "+testGitHubToken {
			t.Fatalf("unexpected Authorization header %q", a)
		}
	}

	// More work goes to the same pull request as a fast-forward.
	writeFile(t, filepath.Join(dir, "src/new/widget.go"), "package widget\n\nconst N = 2\n")
	again, err := e.publish(t, "Follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Forced || again.Number != 1 || again.Commit == pub.Commit || len(e.gh.pulls) != 1 {
		t.Fatalf("unexpected follow-up: %+v", again)
	}
	if got := e.remote(t, "rev-parse", ref+"^"); got != pub.Commit {
		t.Fatalf("follow-up parent = %s, want %s", got, pub.Commit)
	}

	// Nothing new: nothing pushed, same pull request.
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

// TestPublishRebasedBranch is the workflow of a pull request whose base
// moved on: Claude rebases onto the fetched base, resolving a conflict, and
// the existing pull request gets the rewritten branch.
func TestPublishRebasedBranch(t *testing.T) {
	e := newWorkspace(t, true)
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "# widgets\nfrom claude\n")
	first, err := e.publish(t, "Claude's change")
	if err != nil {
		t.Fatal(err)
	}

	// Main advances with a conflicting change; Mosso syncs before the next
	// turn, and Claude rebases and resolves the conflict.
	upstream := e.remoteCommit(t, "main", "README.md", "# widgets\nfrom upstream\n")
	e.sync(t)
	if _, err := gitCmd(e.opts.Dir, "", nil, "rebase", "--quiet", "origin/main"); err == nil {
		t.Fatal("test setup: the rebase should conflict")
	}
	// Publishing in the middle of the rebase is refused.
	if _, err := e.publish(t, "Too early"); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("err = %v, want a rebase-in-progress error", err)
	}
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "# widgets\nfrom upstream\nfrom claude\n")
	e.git(t, "add", "README.md")
	if _, err := gitCmd(e.opts.Dir, "", []string{"GIT_EDITOR=true"}, "rebase", "--continue"); err != nil {
		t.Fatal(err)
	}

	pub, err := e.publish(t, "Rebased")
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/" + e.opts.Branch
	if !pub.Forced || pub.Created || pub.Number != first.Number {
		t.Fatalf("unexpected result: %+v", pub)
	}
	if got, want := e.remote(t, "rev-parse", ref), e.git(t, "rev-parse", "HEAD"); got != want || pub.Commit != want {
		t.Fatalf("remote branch = %s, want %s", got, want)
	}
	if got := e.remote(t, "rev-parse", ref+"^"); got != upstream {
		t.Fatalf("rebased commit parent = %s, want the new base %s", got, upstream)
	}
	if got := e.remote(t, "rev-parse", "main"); got != upstream {
		t.Fatal("the base branch must not change")
	}
}

func TestPublishDoesNotOverwriteOthersCommits(t *testing.T) {
	e := newWorkspace(t, false)
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "mine\n")
	if _, err := e.publish(t, "Mine"); err != nil {
		t.Fatal(err)
	}
	// Someone else pushes to the working branch; Claude meanwhile commits
	// more work, so the branches diverge.
	theirs := e.remoteCommit(t, e.opts.Branch, "docs/guide.md", "theirs\n")
	writeFile(t, filepath.Join(e.opts.Dir, "new.txt"), "more of mine\n")

	_, err := e.publish(t, "More")
	if !errors.Is(err, ErrBranchMoved) || !strings.Contains(err.Error(), "origin/"+e.opts.Branch) {
		t.Fatalf("err = %v, want ErrBranchMoved", err)
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+e.opts.Branch); got != theirs {
		t.Fatal("their commit was overwritten")
	}
	// The failed publish fetched their commit; Claude rebases onto it.
	if got := e.git(t, "rev-parse", "origin/"+e.opts.Branch); got != theirs {
		t.Fatalf("sandbox origin/%s = %s, want %s", e.opts.Branch, got, theirs)
	}
	e.git(t, "rebase", "--quiet", "origin/"+e.opts.Branch)
	pub, err := e.publish(t, "Rebased onto theirs")
	if err != nil {
		t.Fatal(err)
	}
	if pub.Forced {
		t.Fatal("a fast-forward must not be forced")
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+e.opts.Branch+"^"); got != theirs {
		t.Fatalf("parent = %s, want their commit %s", got, theirs)
	}
}

func TestPushLeaseRejectsMovedBranch(t *testing.T) {
	e := newWorkspace(t, false)
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "mine\n")
	pub, err := e.publish(t, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	// The branch moves between Mosso's fetch and its push.
	e.remoteCommit(t, e.opts.Branch, "docs/guide.md", "race\n")
	e.mirror.mu.Lock()
	err = e.mirror.push(context.Background(), Token(testGitHubToken), e.base, pub.Commit)
	e.mirror.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "publish again") {
		t.Fatalf("err = %v, want a lease failure", err)
	}
}

func TestPublishRefusesCredentials(t *testing.T) {
	const claudeToken = "sk-ant-oat01-claude-secret-token"
	for name, tc := range map[string]struct {
		file, content string
		removeLater   bool
		message       string
	}{
		"github token":         {file: "leak.env", content: "token=" + testGitHubToken + "\n"},
		"claude token":         {file: "leak.env", content: "CLAUDE=" + claudeToken + "\n"},
		"in an earlier commit": {file: "leak.env", content: "CLAUDE=" + claudeToken + "\n", removeLater: true},
		"in a commit message":  {file: "ok.txt", content: "fine\n", message: "oops " + testGitHubToken},
	} {
		t.Run(name, func(t *testing.T) {
			e := newWorkspace(t, false)
			writeFile(t, filepath.Join(e.opts.Dir, tc.file), tc.content)
			if tc.removeLater || tc.message != "" {
				msg := tc.message
				if msg == "" {
					msg = "add"
				}
				e.git(t, "add", "-A")
				e.git(t, "commit", "--quiet", "-m", msg)
			}
			if tc.removeLater {
				e.git(t, "rm", "--quiet", tc.file)
				e.git(t, "commit", "--quiet", "-m", "remove")
			}
			_, err := e.publish(t, "Leak", claudeToken)
			if err == nil || !strings.Contains(err.Error(), "credentials") ||
				strings.Contains(err.Error(), testGitHubToken) || strings.Contains(err.Error(), claudeToken) {
				t.Fatalf("err = %v", err)
			}
			if tc.message == "" && !strings.Contains(err.Error(), tc.file) {
				t.Fatalf("err = %v, want it to name %s", err, tc.file)
			}
			if out, _ := gitCmd(e.gh.bare, "", nil, "branch", "--list", "mosso/*"); out != "" {
				t.Fatalf("nothing should be pushed: %q", out)
			}
		})
	}
}

func TestPublishPermissionAndTokenErrors(t *testing.T) {
	e := newWorkspace(t, false)
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "change\n")

	e.gh.mu.Lock()
	e.gh.pushStatus = 403
	e.gh.mu.Unlock()
	_, err := e.publish(t, "No write access")
	if err == nil || !strings.Contains(err.Error(), "Contents and Pull requests") {
		t.Fatalf("err = %v, want a permissions hint", err)
	}
	e.gh.mu.Lock()
	e.gh.pushStatus = 0
	e.gh.mu.Unlock()

	// An expired or revoked token: Git is refused, and the API too.
	bad := e.gh.client()
	bad.token = "github_pat_expired000000000000000000"
	_, err = e.mirror.Publish(context.Background(), bad, PublishOptions{Title: "Expired"})
	if err == nil || !strings.Contains(err.Error(), "invalid, expired or revoked") || strings.Contains(err.Error(), "github_pat_expired") {
		t.Fatalf("err = %v, want an expired-token hint without the token", err)
	}
	if _, err := bad.User(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid, expired or revoked") {
		t.Fatalf("API err = %v", err)
	}

	// The work is still there; with access restored, the publish succeeds.
	pub, err := e.publish(t, "Works now")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.remote(t, "show", pub.Commit+":README.md"); got != "change" {
		t.Fatalf("published README = %q", got)
	}
}

func TestPublishWorkflowChangeRejected(t *testing.T) {
	e := newWorkspace(t, false)
	// GitHub's response to a token without the Workflows permission.
	hook := filepath.Join(e.gh.bare, "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\necho \"refusing to allow a Personal Access Token to create or update workflow \\`.github/workflows/ci.yml\\` without \\`workflow\\` scope\" >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.opts.Dir, ".github/workflows/ci.yml"), "on: push\n")
	_, err := e.publish(t, "Add CI")
	if err == nil || !strings.Contains(err.Error(), "Workflows permission") {
		t.Fatalf("err = %v, want a Workflows hint", err)
	}
}

func TestNewMirrorValidatesInput(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for name, opts := range map[string]MirrorOptions{
		"repo":               {Repo: "octo/../x", Base: "main", BaseSHA: sha, Branch: "mosso/x", Dir: "/x"},
		"not a mosso branch": {Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "main", Dir: "/x"},
		"bad base":           {Repo: testRepo, Base: "--upload-pack=x", BaseSHA: sha, Branch: "mosso/x", Dir: "/x"},
		"bad base sha":       {Repo: testRepo, Base: "main", BaseSHA: "HEAD", Branch: "mosso/x", Dir: "/x"},
		"relative dir":       {Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "mosso/x", Dir: "x"},
	} {
		if _, err := NewMirror(nil, "sbx", opts); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	m, _ := NewMirror(nil, "sbx", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "mosso/x", Dir: "/x"})
	if _, err := m.Publish(context.Background(), NewClient("t"), PublishOptions{Title: " "}); err == nil {
		t.Error("a publish without a title should fail")
	}
}
