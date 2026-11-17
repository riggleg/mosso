package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/worker"
)

// toolServer is a server with a ready workspace for octo/widgets, whose
// fake GitHub also has a uat branch.
func toolServer(t *testing.T) (*Server, *fakeVercel, *fakeGitHubAPI, testWorkspace) {
	t.Helper()
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	api := &fakeGitHubAPI{remote: map[string]string{"uat": strings.Repeat("5", 40)}}
	withGitHub(t, srv, api)
	fake.stdoutFor = fakeRepoSandbox()
	return srv, fake, api, createTestWorkspace(t, srv)
}

func createTestWorkspace(t *testing.T, srv *Server) testWorkspace {
	t.Helper()
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	return ws
}

// runTurn sends a message and waits for the turn to end; the fake Claude
// makes the scripted tool calls during it.
func runTurn(t *testing.T, srv *Server, fake *fakeVercel, name string, calls ...worker.CallRequest) []toolAnswer {
	t.Helper()
	fake.claude.script(calls...)
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces/"+name+"/claude/messages", `{"prompt":"do it"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("message status = %d: %s", rec.Code, rec.Body)
	}
	answers := fake.claude.toolAnswers()
	if len(answers) != len(calls) {
		t.Fatalf("%d answers for %d calls", len(answers), len(calls))
	}
	return answers
}

// githubEntries returns the workspace's "github" chat entries.
func githubEntries(srv *Server, name string) []githubAction {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var out []githubAction
	for _, e := range srv.workspaces[name].chat.entries {
		if e.Type == "github" {
			out = append(out, *e.GitHub)
		}
	}
	return out
}

func args(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAgentOpensPullRequestsIntoTwoBases is the workflow the feature is
// for: in one turn, the agent pushes the working branch and opens a pull
// request into the base, fetches uat, pushes its port to a second branch
// and opens a pull request into uat, and reads them back. Mosso decides
// nothing: each step is a tool call.
func TestAgentOpensPullRequestsIntoTwoBases(t *testing.T) {
	srv, fake, api, ws := toolServer(t)
	uatHead := ws.Branch + "-uat"
	answers := runTurn(t, srv, fake, ws.Name,
		call(worker.ToolPush, `{}`),
		call(worker.ToolCreatePR, `{"title":"Fix the README","body":"Why."}`),
		call(worker.ToolFetch, `{"branch":"uat"}`),
		call(worker.ToolPush, args(t, map[string]any{"branch": uatHead, "source": "fix-uat"})),
		call(worker.ToolCreatePR, args(t, map[string]any{"title": "Fix the README (uat)", "head": uatHead, "base": "uat", "draft": true})),
		call(worker.ToolCreatePR, args(t, map[string]any{"title": "Again", "head": uatHead, "base": "uat"})),
		call(worker.ToolListPRs, `{"state":"open"}`),
		call(worker.ToolGetPR, `{"number":2}`),
		call(worker.ToolUpdatePR, `{"number":2,"draft":false,"body":"Ported to uat."}`),
		call(worker.ToolChecks, `{"pr":1}`),
		call(worker.ToolChecks, `{"branch":"uat"}`),
	)
	if r := answers[0].result(t); r["branch"] != ws.Branch || r["commit"] != testCommitSHA || r["updated"] != true {
		t.Fatalf("push: %v", r)
	}
	first := answers[1].result(t)
	if first["number"] != float64(1) || first["created"] != true || first["base"] != "main" || first["head"] != ws.Branch {
		t.Fatalf("first pull request: %v", first)
	}
	if r := answers[2].result(t); r["ref"] != "origin/uat" || r["commit"] != strings.Repeat("5", 40) {
		t.Fatalf("fetch: %v", r)
	}
	if r := answers[3].result(t); r["branch"] != uatHead {
		t.Fatalf("second push: %v", r)
	}
	second := answers[4].result(t)
	if second["number"] != float64(2) || second["created"] != true || second["base"] != "uat" || second["head"] != uatHead {
		t.Fatalf("second pull request: %v", second)
	}
	// Asking for the same pull request again returns it.
	if again := answers[5].result(t); again["number"] != float64(2) || again["created"] != false {
		t.Fatalf("duplicate: %v", again)
	}
	if len(api.pulls) != 2 {
		t.Fatalf("GitHub has %d pull requests, want 2", len(api.pulls))
	}
	list := answers[6].result(t)["pull_requests"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["base"] != "main" || list[1].(map[string]any)["base"] != "uat" {
		t.Fatalf("list: %v", list)
	}
	if pr := answers[7].result(t); pr["draft"] != true || pr["title"] != "Fix the README (uat)" || pr["body"] != "Created with Mosso." || pr["mergeable"] != true {
		t.Fatalf("get: %v", pr)
	}
	if pr := answers[8].result(t); pr["draft"] != false {
		t.Fatalf("update: %v", pr)
	}
	if api.pulls[1]["body"] != "Ported to uat." || api.pulls[1]["draft"] != false {
		t.Fatalf("GitHub's pull request #2: %v", api.pulls[1])
	}
	if c := answers[9].result(t); c["state"] != "none" || c["complete"] != true || c["note"] != nil || c["commit"] != testCommitSHA {
		t.Fatalf("checks: %v", c)
	}
	if c := answers[10].result(t); c["branch"] != "uat" || c["commit"] != strings.Repeat("5", 40) {
		t.Fatalf("checks of a branch: %v", c)
	}

	// Both pull requests are the workspace's; the one into the base is
	// also the one Create PR updates.
	rec := do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "")
	var got struct {
		PullRequest  *github.PullRequest `json:"pull_request"`
		PullRequests []pullRequestRecord `json:"pull_requests"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.PullRequest == nil || got.PullRequest.Number != 1 || len(got.PullRequests) != 2 ||
		got.PullRequests[0].Base != "main" || got.PullRequests[1].Base != "uat" || got.PullRequests[1].Head != uatHead ||
		got.PullRequests[1].By != "agent" || got.PullRequests[1].Draft {
		t.Fatalf("workspace: %s", rec.Body)
	}
	// The chat shows what changed on GitHub, with the pull requests.
	var texts []string
	for _, a := range githubEntries(srv, ws.Name) {
		texts = append(texts, a.Tool+": "+a.Text)
	}
	want := []string{
		"github_push: Pushed 4444444 to " + ws.Branch,
		"github_create_pr: Opened pull request #1: " + ws.Branch + " → main",
		"github_push: Pushed " + short(fmt.Sprintf("%040x", len("fix-uat"))) + " to " + uatHead,
		"github_create_pr: Opened pull request #2: " + uatHead + " → uat",
		"github_create_pr: Pull request #2 (" + uatHead + " → uat) was already open",
		"github_update_pr: Updated pull request #2: description, marked ready for review",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("github entries:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	// Saved: the pull requests and the extra branches the mirror syncs.
	srv.mu.Lock()
	recs, err := srv.store.load()
	srv.mu.Unlock()
	if err != nil || len(recs) != 1 || len(recs[0].PullRequests) != 2 || strings.Join(recs[0].Branches, " ") != "uat "+uatHead {
		t.Fatalf("saved: %+v %v", recs, err)
	}
	// The secret check covers the Claude token, and no token entered the
	// sandbox.
	if p := api.pushes[0]; len(p.Secrets) != 1 || p.Secrets[0] != testClaudeToken {
		t.Fatalf("push options: %+v", p)
	}
	for _, c := range fake.commands() {
		if b, _ := json.Marshal(c); strings.Contains(string(b), testGitHubToken) {
			t.Fatalf("GitHub token sent into the sandbox: %s", b)
		}
	}

	// The user's Create PR / Update PR still works, on the same pull
	// request the agent opened.
	rec = do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/pull-request", `{"title":"From the user"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"number":1`) {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.PullRequest.Number != 1 || len(got.PullRequests) != 2 || got.PullRequests[0].By != "agent" {
		t.Fatalf("after publishing: %s", rec.Body)
	}
}

// TestAgentToolsStayInTheirWorkspace: a workspace's agent cannot touch
// another workspace's branches or pull requests, other branches, or another
// repository; the tools have no repository argument at all.
func TestAgentToolsStayInTheirWorkspace(t *testing.T) {
	srv, fake, api, a := toolServer(t)
	b := createTestWorkspace(t, srv)
	// Workspace A opens a pull request.
	runTurn(t, srv, fake, a.Name, call(worker.ToolPush, `{}`), call(worker.ToolCreatePR, `{"title":"A's work"}`))
	api.mu.Lock()
	api.addPull(map[string]any{"head": "feature/x", "base": "main", "title": "A human's work"})
	api.remote["feature/x"] = strings.Repeat("6", 40)
	api.addPull(map[string]any{"head": b.Branch, "base": "main", "title": "From a fork", "head_repo": "evil/widgets"})
	api.mu.Unlock()

	answers := runTurn(t, srv, fake, b.Name,
		call(worker.ToolGetPR, `{"number":1}`),
		call(worker.ToolUpdatePR, `{"number":1,"title":"Mine now"}`),
		call(worker.ToolChecks, `{"pr":1}`),
		call(worker.ToolGetPR, `{"number":2}`),
		call(worker.ToolGetPR, `{"number":3}`),
		call(worker.ToolChecks, args(t, map[string]any{"branch": a.Branch})),
		call(worker.ToolPush, args(t, map[string]any{"branch": a.Branch})),
		call(worker.ToolPush, `{"branch":"main"}`),
		call(worker.ToolCreatePR, args(t, map[string]any{"title": "x", "head": a.Branch})),
		call(worker.ToolCreatePR, `{"title":"x","head":"feature/x"}`),
		call(worker.ToolCreatePR, args(t, map[string]any{"title": "x", "base": a.Branch})),
		call(worker.ToolCreatePR, `{"title":"x","repo":"evil/repo"}`),
		call(worker.ToolPush, `{"repository":"evil/repo"}`),
		call(worker.ToolCreatePR, `{"title":"x","base":"--force"}`),
		call(worker.ToolListPRs, `{}`),
		call(worker.ToolFetch, args(t, map[string]any{"branch": a.Branch})),
	)
	for i, want := range []string{
		"is not one of this workspace's pull requests",
		"is not one of this workspace's pull requests",
		"is not one of this workspace's pull requests",
		"is not one of this workspace's pull requests",
		"is not one of this workspace's pull requests", // a fork's branch with B's name
		"belongs to another Mosso workspace",
		"only pushes this workspace's own branches",
		"only pushes this workspace's own branches",
		"head must be one of this workspace's branches",
		"head must be one of this workspace's branches",
		"base must be one of the repository's own branches",
		`unknown field "repo"`,
		`unknown field "repository"`,
		"not a branch name Mosso accepts",
	} {
		if got := answers[i].failure(); !strings.Contains(got, want) {
			t.Errorf("call %d (%s): %q, want %q", i, answers[i].name, got, want)
		}
	}
	if got := answers[15].failure(); !strings.Contains(got, "belongs to another Mosso workspace") {
		t.Errorf("fetch of A's branch: %q", got)
	}
	if list := answers[14].result(t)["pull_requests"].([]any); len(list) != 0 {
		t.Fatalf("B lists %v", list)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.pulls[0]["title"] != "A's work" || len(api.pulls) != 3 {
		t.Fatalf("pull requests changed: %v", api.pulls)
	}
	if api.remote[a.Branch] != testCommitSHA || api.remote["main"] != "" {
		t.Fatalf("branches changed: %v", api.remote)
	}
	// B's failed writes are in B's chat; A's chat only has A's.
	if n := len(githubEntries(srv, a.Name)); n != 2 {
		t.Fatalf("A has %d github entries", n)
	}
	for _, e := range githubEntries(srv, b.Name) {
		if e.Error == "" {
			t.Fatalf("B's entry without an error: %+v", e)
		}
	}
}

// TestAgentToolFailuresAreReportedAndCanBeRetried.
func TestAgentToolFailuresAreReportedAndCanBeRetried(t *testing.T) {
	srv, fake, api, ws := toolServer(t)
	api.mu.Lock()
	api.pushErr = errors.New("push to GitHub: a branch protection rule or ruleset rejected the push to the workspace's branch")
	api.mu.Unlock()
	answers := runTurn(t, srv, fake, ws.Name,
		call(worker.ToolPush, `{}`),
		call(worker.ToolCreatePR, `{"title":"Too early"}`),
		call(worker.ToolCreatePR, `{"title":"x","base":"nope"}`),
		call(worker.ToolFetch, `{"branch":"nope"}`),
		call(worker.ToolUpdatePR, `{"number":1}`),
		call(worker.ToolChecks, `{}`),
	)
	for i, want := range []string{
		"ruleset rejected the push",
		"is not on GitHub yet: push it first with github_push",
		"the base branch nope does not exist",
		"the branch nope does not exist",
		"give at least one of title, body or draft",
		"give either pr or branch",
	} {
		if got := answers[i].failure(); !strings.Contains(got, want) {
			t.Errorf("call %d: %q, want %q", i, got, want)
		}
	}
	entries := githubEntries(srv, ws.Name)
	if len(entries) != 4 || entries[0].Text != "Push failed" || !strings.Contains(entries[0].Error, "ruleset") {
		t.Fatalf("entries: %+v", entries)
	}

	// Fixed on GitHub's side: the agent retries.
	api.mu.Lock()
	api.pushErr = nil
	api.mu.Unlock()
	answers = runTurn(t, srv, fake, ws.Name, call(worker.ToolPush, `{}`), call(worker.ToolCreatePR, `{"title":"Now"}`))
	if pr := answers[1].result(t); pr["created"] != true {
		t.Fatalf("retry: %v", pr)
	}

	// Without a GitHub connection, nothing is done and the agent is told.
	srv.githubClient = func(context.Context) (*github.Client, string, error) { return nil, "", github.ErrNoToken }
	answers = runTurn(t, srv, fake, ws.Name, call(worker.ToolPush, `{}`))
	if got := answers[0].failure(); !strings.Contains(got, "GitHub is not connected in Mosso") {
		t.Fatalf("without GitHub: %q", got)
	}
}

// TestToolCallWhileMossoIsStoppedRunsAfterRestart: Claude calls a tool
// while Mosso is not running; the call waits in the sandbox, and the
// restarted Mosso picks it up on reconnecting, runs it once and answers.
// The pull request it opens survives the next restart, and so do the
// branches the mirror keeps in sync.
func TestToolCallWhileMossoIsStoppedRunsAfterRestart(t *testing.T) {
	h := newPersistentHarness(t)
	h.api = &fakeGitHubAPI{remote: map[string]string{"uat": strings.Repeat("5", 40)}}
	fc := h.fake.claude
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	fc.mu.Lock()
	fc.gate, fc.started = gate, started
	fc.mu.Unlock()
	fc.script(call(worker.ToolFetch, `{"branch":"uat"}`), call(worker.ToolPush, `{}`), call(worker.ToolCreatePR, `{"title":"While you were away"}`))
	if resp := h.chat("go"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	<-started
	h.stop()
	close(gate) // Claude calls the tools now, with Mosso stopped.
	fw := h.fake.worker(h.ws.Name)
	waitFor(t, func() bool {
		_, out, _ := fw.call(http.MethodGet, workerBase+"/status", "")
		return strings.Contains(out, `"pending_calls":["call_`)
	})

	api := &fakeGitHubAPI{remote: map[string]string{"uat": strings.Repeat("5", 40)}}
	h.api = api
	h.start()
	waitFor(t, func() bool { return len(fc.toolAnswers()) == 3 })
	answers := fc.toolAnswers()
	if pr := answers[2].result(t); pr["created"] != true || pr["number"] != float64(1) {
		t.Fatalf("answers: %+v", answers)
	}
	h.waitStatus("ready")
	if len(api.pushes) != 1 || len(api.pulls) != 1 {
		t.Fatalf("ran %d pushes and opened %d pull requests, want 1 each", len(api.pushes), len(api.pulls))
	}
	if rec := h.record(); len(rec.PullRequests) != 1 || rec.PullRequests[0].Number != 1 || rec.PullRequest == nil ||
		strings.Join(rec.Branches, " ") != "uat" || rec.Pushed != testCommitSHA {
		t.Fatalf("record: %+v", rec)
	}

	h.stop()
	h.start()
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	prs, mirrored := ws.PullRequests, ws.git.Branches()
	h.srv.mu.Unlock()
	if len(prs) != 1 || prs[0].URL != "https://github.com/octo/widgets/pull/1" || strings.Join(mirrored, " ") != "uat" {
		t.Fatalf("after restart: prs %+v, mirror tracks %v", prs, mirrored)
	}
}

// TestOldWorkerIsReplacedWhenIdle: a sandbox still running the worker of an
// earlier Mosso, which offers no tools, gets this Mosso's worker before the
// next turn, over the same event log.
func TestOldWorkerIsReplacedWhenIdle(t *testing.T) {
	h := newPersistentHarness(t)
	if resp := h.chat("first"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	h.waitStatus("ready")
	h.stop()
	fw := h.fake.worker(h.ws.Name)
	fw.stop()
	fw.mu.Lock()
	fw.version = "1"
	fw.mu.Unlock()
	fw.start() // the old worker, still running
	before := fw.starts

	h.start()
	h.waitStatus("ready")
	waitFor(t, func() bool {
		_, out, _ := fw.call(http.MethodGet, workerBase+"/status", "")
		return strings.Contains(out, `"version":"`+worker.Version+`"`)
	})
	fw.mu.Lock()
	starts := fw.starts
	fw.mu.Unlock()
	if starts != before+1 {
		t.Fatalf("worker started %d times, want once more", starts-before)
	}
	answers := runTurn(t, h.srv, h.fake, h.ws.Name, call(worker.ToolListPRs, `{}`))
	answers[0].result(t)
	if n := countTypes(h.history())["user"]; n != 2 {
		t.Fatalf("history has %d user entries, want 2", n)
	}
}

// TestChecksNeverClaimSuccessWhenIncomplete: with check runs all green but
// commit statuses unreadable (a token without that permission), the agent
// is told the result is incomplete, not that CI passed.
func TestChecksNeverClaimSuccessWhenIncomplete(t *testing.T) {
	srv, fake, api, ws := toolServer(t)
	api.mu.Lock()
	api.checkRuns = []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}}
	api.statusForbidden = true
	api.mu.Unlock()
	answers := runTurn(t, srv, fake, ws.Name,
		call(worker.ToolPush, `{}`),
		call(worker.ToolCreatePR, `{"title":"Fix"}`),
		call(worker.ToolChecks, `{"pr":1}`),
	)
	c := answers[2].result(t)
	missing, _ := c["missing"].([]any)
	note, _ := c["note"].(string)
	if c["state"] != "unknown" || c["complete"] != false || len(missing) != 1 ||
		!strings.Contains(missing[0].(string), "Commit statuses") || !strings.Contains(note, "does not show that all checks passed") {
		t.Fatalf("checks: %v", c)
	}
}
