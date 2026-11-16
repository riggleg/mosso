package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOwnBranch(t *testing.T) {
	const w = "mosso/0a1b2c3d4e5f"
	for branch, want := range map[string]bool{
		w:                                 true,
		w + "-uat":                        true,
		w + "-uat.2_fix-x":                true,
		w + "-":                           false,
		w + "-/x":                         false,
		w + "-..":                         false,
		w + "-uat.lock":                   false,
		w + "-.x":                         false,
		w + "-" + strings.Repeat("a", 51): false,
		w + "uat":                         false,
		"mosso/0a1b2c3d4e5e":              false,
		"mosso/0a1b2c3d4e5e-uat":          false,
		"main":                            false,
		"uat":                             false,
		"":                                false,
	} {
		if got := OwnBranch(w, branch); got != want {
			t.Errorf("OwnBranch(%q) = %t, want %t", branch, got, want)
		}
	}
}

// addRemoteBranch creates branch on the fake GitHub from main, with one
// commit of its own, and returns its head.
func (e *workspaceEnv) addRemoteBranch(t *testing.T, branch, file, content string) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "", "clone", "--quiet", "--branch", "main", e.gh.bare, ".")
	runGit(t, work, "", "checkout", "--quiet", "-b", branch)
	writeFile(t, filepath.Join(work, file), content)
	runGit(t, work, "", "add", "-A")
	runGit(t, work, "", "commit", "--quiet", "-m", branch+": "+file)
	runGit(t, work, "", "push", "--quiet", "origin", branch)
	return runGit(t, work, "", "rev-parse", "HEAD")
}

func (e *workspaceEnv) push(t *testing.T, branch, source string, secrets ...string) (*Pushed, error) {
	t.Helper()
	return e.mirror.Push(context.Background(), Token(testGitHubToken), PushOptions{Branch: branch, Source: source, Secrets: secrets})
}

// TestPushToTwoBaseBranches is the agent's workflow for pull requests into
// master and uat: push the working branch and open a pull request into the
// base; fetch uat, port the change onto it on a local branch, push that to
// a second Mosso branch and open a pull request into uat.
func TestPushToTwoBaseBranches(t *testing.T) {
	e := newWorkspace(t, true)
	uat := e.addRemoteBranch(t, "uat", "uat.txt", "uat only\n")
	gh := e.gh.client()
	ctx := context.Background()
	before := len(e.sandbox.bodies)

	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "# widgets\nfixed\n")
	e.git(t, "commit", "--quiet", "-am", "Fix the README")
	fix := e.git(t, "rev-parse", "HEAD")

	p, err := e.push(t, e.opts.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Updated || p.Commit != fix || p.Previous != "" || p.Dirty || p.Branch != e.opts.Branch {
		t.Fatalf("unexpected push: %+v", p)
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+e.opts.Branch); got != fix {
		t.Fatalf("remote %s = %s, want %s", e.opts.Branch, got, fix)
	}
	if got := e.git(t, "rev-parse", "origin/"+e.opts.Branch); got != fix {
		t.Fatalf("sandbox origin/%s = %s", e.opts.Branch, got)
	}
	first, created, err := gh.OpenPullRequest(ctx, testRepo, NewPullRequest{Title: "Fix the README", Head: e.opts.Branch, Base: "main"})
	if err != nil || !created || first.Number != 1 {
		t.Fatalf("first pull request: %+v created=%t err=%v", first, created, err)
	}

	// uat is not in the sandbox until fetched (the repository is private).
	if _, err := gitCmd(e.opts.Dir, "", nil, "rev-parse", "--verify", "--quiet", "origin/uat"); err == nil {
		t.Fatal("test setup: origin/uat should not exist yet")
	}
	heads, err := e.mirror.Fetch(ctx, Token(testGitHubToken), "uat")
	if err != nil || heads["uat"] != uat {
		t.Fatalf("fetch uat: %v %v", heads, err)
	}
	if got := e.git(t, "rev-parse", "origin/uat"); got != uat {
		t.Fatalf("sandbox origin/uat = %s, want %s", got, uat)
	}
	// The agent ports the change onto uat on a local branch.
	e.git(t, "checkout", "--quiet", "-b", "fix-uat", "origin/uat")
	e.git(t, "cherry-pick", fix)
	e.git(t, "checkout", "--quiet", e.opts.Branch)
	ported := e.git(t, "rev-parse", "fix-uat")

	uatBranch := e.opts.Branch + "-uat"
	p, err = e.push(t, uatBranch, "fix-uat")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Updated || p.Commit != ported || p.Branch != uatBranch {
		t.Fatalf("unexpected push: %+v", p)
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+uatBranch); got != ported {
		t.Fatalf("remote %s = %s, want %s", uatBranch, got, ported)
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+uatBranch+"^"); got != uat {
		t.Fatalf("the uat branch should be based on uat, its parent is %s", got)
	}
	second, created, err := gh.OpenPullRequest(ctx, testRepo, NewPullRequest{Title: "Fix the README (uat)", Head: uatBranch, Base: "uat"})
	if err != nil || !created || second.Number != 2 {
		t.Fatalf("second pull request: %+v created=%t err=%v", second, created, err)
	}
	// Asking again returns the same pull request.
	again, created, err := gh.OpenPullRequest(ctx, testRepo, NewPullRequest{Title: "Again", Head: uatBranch, Base: "uat"})
	if err != nil || created || again.Number != 2 || len(e.gh.pulls) != 2 {
		t.Fatalf("duplicate pull request: %+v created=%t err=%v pulls=%d", again, created, err, len(e.gh.pulls))
	}
	// The base branches never change.
	if e.remote(t, "rev-parse", "main") != e.base || e.remote(t, "rev-parse", "uat") != uat {
		t.Fatal("a base branch changed")
	}
	// Pushing the same commit again changes nothing.
	p, err = e.push(t, uatBranch, "fix-uat")
	if err != nil || p.Updated {
		t.Fatalf("repeated push: %+v %v", p, err)
	}
	if got := e.mirror.Branches(); !slices.Equal(got, []string{"uat", uatBranch}) {
		t.Fatalf("tracked branches = %v", got)
	}
	e.noTokenInSandbox(t, before)

	// Both stay current in the sandbox before each turn, also in a Mirror
	// restored after a restart.
	moved := e.remoteCommit(t, "uat", "uat.txt", "uat moved\n")
	restored, err := NewMirror(e.vc, "sbx_1", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: e.base, Branch: e.opts.Branch, Dir: e.opts.Dir, Extra: e.mirror.Branches()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if _, err := restored.Sync(ctx, Token(testGitHubToken)); err != nil {
		t.Fatal(err)
	}
	if got := e.git(t, "rev-parse", "origin/uat"); got != moved {
		t.Fatalf("sandbox origin/uat = %s, want %s after a sync", got, moved)
	}
	if got := e.git(t, "rev-parse", "origin/"+uatBranch); got != ported {
		t.Fatalf("sandbox origin/%s = %s", uatBranch, got)
	}
}

func TestPushRefusesOtherBranchesAndForce(t *testing.T) {
	e := newWorkspace(t, false)
	ctx := context.Background()
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "mine\n")
	e.git(t, "commit", "--quiet", "-am", "Mine")

	for _, branch := range []string{"main", "uat", "mosso/0123456789ab", e.opts.Branch + "-", e.opts.Branch + "-a/b", "--force", ""} {
		if _, err := e.push(t, branch, ""); err == nil || !strings.Contains(err.Error(), "own branches") {
			t.Errorf("push to %q: err = %v, want a refusal", branch, err)
		}
	}
	for _, source := range []string{"--all", "-f", "a..b", "HEAD~1"} {
		if _, err := e.push(t, e.opts.Branch, source); err == nil || !strings.Contains(err.Error(), "source") {
			t.Errorf("push from %q: err = %v, want a refusal", source, err)
		}
	}
	if _, err := e.push(t, e.opts.Branch, "no-such-branch"); err == nil || !strings.Contains(err.Error(), "no local branch no-such-branch") {
		t.Fatalf("err = %v, want a missing-branch error", err)
	}
	if out, _ := gitCmd(e.gh.bare, "", nil, "branch", "--list", "mosso/*"); out != "" {
		t.Fatalf("nothing should be pushed: %q", out)
	}

	// Uncommitted work is reported and left out.
	writeFile(t, filepath.Join(e.opts.Dir, "draft.txt"), "not committed\n")
	p, err := e.push(t, e.opts.Branch, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Dirty || !p.Updated {
		t.Fatalf("unexpected push: %+v", p)
	}
	if _, err := gitCmd(e.gh.bare, "", nil, "cat-file", "-e", p.Commit+":draft.txt"); err == nil {
		t.Fatal("uncommitted work was pushed")
	}
	if got := e.git(t, "status", "--porcelain"); got != "?? draft.txt" {
		t.Fatalf("the checkout changed: %q", got)
	}

	// A rewritten history is never force-pushed for the agent.
	e.git(t, "commit", "--quiet", "--amend", "-m", "Mine, amended")
	_, err = e.push(t, e.opts.Branch, "")
	if !errors.Is(err, ErrNotFastForward) || !strings.Contains(err.Error(), "Update PR") {
		t.Fatalf("err = %v, want ErrNotFastForward", err)
	}
	if got := e.remote(t, "rev-parse", "refs/heads/"+e.opts.Branch); got != p.Commit {
		t.Fatal("the branch was rewritten")
	}
	// Publishing, which the user does, still may replace it.
	pub, err := e.mirror.Publish(ctx, e.gh.client(), PublishOptions{Title: "Rebased"})
	if err != nil || !pub.Forced {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	// After the agent pushed it, so Mosso knows the branch is its own.
	e.git(t, "commit", "--quiet", "--amend", "-m", "Mine, amended twice")
	if _, err := e.push(t, e.opts.Branch, ""); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("err = %v, want ErrNotFastForward", err)
	}

	// Credentials in a commit are refused.
	const claudeToken = "sk-ant-oat01-claude-secret-token"
	e.git(t, "checkout", "--quiet", "-b", "leak")
	writeFile(t, filepath.Join(e.opts.Dir, "leak.env"), "CLAUDE="+claudeToken+"\n")
	e.git(t, "add", "leak.env")
	e.git(t, "commit", "--quiet", "-m", "leak")
	_, err = e.push(t, e.opts.Branch+"-leak", "leak", claudeToken)
	if err == nil || !strings.Contains(err.Error(), "credentials") || strings.Contains(err.Error(), claudeToken) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if out, _ := gitCmd(e.gh.bare, "", nil, "branch", "--list", e.opts.Branch+"-leak"); out != "" {
		t.Fatal("the leaking branch was pushed")
	}
}

func TestFetchMissingBranchIsNotTracked(t *testing.T) {
	e := newWorkspace(t, false)
	heads, err := e.mirror.Fetch(context.Background(), Token(testGitHubToken), "nope")
	if err != nil || heads["nope"] != "" {
		t.Fatalf("fetch: %v %v", heads, err)
	}
	if got := e.mirror.Branches(); len(got) != 0 {
		t.Fatalf("tracked = %v", got)
	}
	if _, err := e.mirror.Fetch(context.Background(), Token(testGitHubToken), "--upload-pack=x"); err == nil {
		t.Fatal("an invalid branch name should be refused")
	}
}

func TestTrackedBranchesAreBounded(t *testing.T) {
	sha := strings.Repeat("a", 40)
	extra := make([]string, MaxExtraBranches+1)
	for i := range extra {
		extra[i] = "b" + strings.Repeat("x", i+1)
	}
	if _, err := NewMirror(nil, "sbx", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "mosso/x", Dir: "/x", Extra: extra}); err == nil {
		t.Fatal("too many extra branches should be refused")
	}
	m, err := NewMirror(nil, "sbx", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: sha, Branch: "mosso/x", Dir: "/x", Extra: []string{"uat", "uat", "main"}})
	if err != nil || !slices.Equal(m.Branches(), []string{"uat"}) {
		t.Fatalf("branches = %v, %v", m.Branches(), err)
	}
}

func TestPullRequestDetailsUpdatesAndChecks(t *testing.T) {
	e := newWorkspace(t, false)
	ctx := context.Background()
	gh := e.gh.client()
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "change\n")
	e.git(t, "commit", "--quiet", "-am", "Change")
	p, err := e.push(t, e.opts.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	pr, _, err := gh.OpenPullRequest(ctx, testRepo, NewPullRequest{Title: "Change", Body: "Why.", Head: e.opts.Branch, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}

	info, err := gh.GetPullRequest(ctx, testRepo, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Change" || info.Body != "Why." || info.Head != e.opts.Branch || info.HeadRepo != testRepo ||
		info.Base != "main" || info.HeadSHA != p.Commit || info.State != "open" || info.Draft || info.Mergeable == nil {
		t.Fatalf("unexpected details: %+v", info)
	}
	if _, err := gh.GetPullRequest(ctx, testRepo, 99); !IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}

	title, draft := "Change, retitled", true
	info, err = gh.UpdatePullRequest(ctx, testRepo, info, PullRequestUpdate{Title: &title, Draft: &draft})
	if err != nil || info.Title != title || !info.Draft || info.Body != "Why." {
		t.Fatalf("update: %+v %v", info, err)
	}
	draft = false
	if info, err = gh.UpdatePullRequest(ctx, testRepo, info, PullRequestUpdate{Draft: &draft}); err != nil || info.Draft {
		t.Fatalf("ready for review: %+v %v", info, err)
	}

	list, err := gh.PullRequests(ctx, testRepo, e.opts.Branch, "all")
	if err != nil || len(list) != 1 || list[0].Number != pr.Number {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list, _ := gh.PullRequests(ctx, testRepo, e.opts.Branch, "closed"); len(list) != 0 {
		t.Fatalf("closed: %+v", list)
	}

	checks, err := gh.CommitChecks(ctx, testRepo, p.Commit)
	if err != nil || checks.State != "none" || len(checks.Missing) != 0 {
		t.Fatalf("no checks: %+v %v", checks, err)
	}
	e.gh.mu.Lock()
	e.gh.checkRuns = []map[string]any{
		{"name": "test", "status": "completed", "conclusion": "success", "html_url": "https://example.test/1"},
		{"name": "lint", "status": "in_progress", "conclusion": nil},
	}
	e.gh.mu.Unlock()
	if checks, _ = gh.CommitChecks(ctx, testRepo, p.Commit); checks.State != "pending" || len(checks.Checks) != 2 {
		t.Fatalf("pending: %+v", checks)
	}
	e.gh.mu.Lock()
	e.gh.checkRuns[1] = map[string]any{"name": "lint", "status": "completed", "conclusion": "failure", "output": map[string]any{"title": "2 problems"}}
	e.gh.statusForbidden = true
	e.gh.mu.Unlock()
	checks, err = gh.CommitChecks(ctx, testRepo, p.Commit)
	if err != nil || checks.State != "failure" || checks.Checks[1].Summary != "2 problems" ||
		len(checks.Missing) != 1 || !strings.Contains(checks.Missing[0], "Commit statuses: Read-only") {
		t.Fatalf("failure: %+v %v", checks, err)
	}
	if _, err := gh.CommitChecks(ctx, testRepo, "main"); err == nil {
		t.Fatal("checks need a commit SHA")
	}
	if sha, err := gh.BranchHead(ctx, testRepo, e.opts.Branch); err != nil || sha != p.Commit {
		t.Fatalf("branch head = %s, %v", sha, err)
	}
	if _, err := gh.BranchHead(ctx, testRepo, "nope"); !IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestPushAfterRestoreKeepsTheWorkingBranchSafe(t *testing.T) {
	e := newWorkspace(t, false)
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "mine\n")
	e.git(t, "commit", "--quiet", "-am", "Mine")
	p, err := e.push(t, e.opts.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	// After a restart, Mosso remembers what it pushed, so the user's
	// Update PR can still replace a rebased branch.
	restored, err := NewMirror(e.vc, "sbx_1", MirrorOptions{Repo: testRepo, Base: "main", BaseSHA: e.base, Branch: e.opts.Branch, Dir: e.opts.Dir, Pushed: p.Commit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	e.git(t, "commit", "--quiet", "--amend", "-m", "Mine, amended")
	if _, err := restored.Push(context.Background(), Token(testGitHubToken), PushOptions{Branch: e.opts.Branch}); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("err = %v, want ErrNotFastForward", err)
	}
	pub, err := restored.Publish(context.Background(), e.gh.client(), PublishOptions{Title: "Amended"})
	if err != nil || !pub.Forced {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	if _, err := os.Stat(e.opts.Dir); err != nil {
		t.Fatal(err)
	}
}

// TestExtraBranchThatCannotBeSentDoesNotBreakTheWorkspace: if an extra
// branch can no longer be sent to the sandbox, syncs, publishes and pushes
// still work, and a branch that cannot be sent when fetched is not kept.
func TestExtraBranchThatCannotBeSentDoesNotBreakTheWorkspace(t *testing.T) {
	e := newWorkspace(t, false)
	ctx := context.Background()
	tok := Token(testGitHubToken)
	e.addRemoteBranch(t, "rel/a", "a.txt", "a\n")
	e.addRemoteBranch(t, "next/b", "b.txt", "b\n")
	if _, err := e.mirror.Fetch(ctx, tok, "rel/a"); err != nil {
		t.Fatal(err)
	}
	// Break both in the sandbox: a ref where their directories must go.
	e.git(t, "update-ref", "-d", "refs/remotes/origin/rel/a")
	e.git(t, "update-ref", "refs/remotes/origin/rel", e.base)
	e.git(t, "update-ref", "refs/remotes/origin/next", e.base)
	e.remoteCommit(t, "rel/a", "a.txt", "a moved\n")

	if _, err := e.mirror.Fetch(ctx, tok, "next/b"); err == nil {
		t.Fatal("fetching a branch the sandbox cannot take should fail")
	}
	if got := e.mirror.Branches(); !slices.Equal(got, []string{"rel/a"}) {
		t.Fatalf("tracked = %v", got)
	}
	upstream := e.remoteCommit(t, "main", "README.md", "# widgets\nupstream\n")
	if s := e.sync(t); s.After.Base != upstream {
		t.Fatalf("sync: %+v", s)
	}
	if got := e.git(t, "rev-parse", "origin/main"); got != upstream {
		t.Fatalf("origin/main = %s", got)
	}
	writeFile(t, filepath.Join(e.opts.Dir, "README.md"), "mine\n")
	if _, err := e.publish(t, "Still publishes"); err != nil {
		t.Fatal(err)
	}
	e.git(t, "commit", "--quiet", "--allow-empty", "-m", "more")
	if _, err := e.push(t, e.opts.Branch, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRefusesOtherWorkspacesBranches(t *testing.T) {
	e := newWorkspace(t, false)
	other := "mosso/0123456789ab"
	e.addRemoteBranch(t, other, "theirs.txt", "theirs\n")
	if _, err := e.mirror.Fetch(context.Background(), Token(testGitHubToken), other); err == nil || !strings.Contains(err.Error(), "another Mosso workspace") {
		t.Fatalf("err = %v", err)
	}
	if _, err := gitCmd(e.opts.Dir, "", nil, "rev-parse", "--verify", "--quiet", "origin/"+other); err == nil {
		t.Fatal("another workspace's branch reached the sandbox")
	}
}
