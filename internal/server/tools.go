package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/worker"
)

// Agent-controlled GitHub operations (docs/agent-github.md). Claude calls
// Mosso's tools through the worker's MCP server; each call is a tool_call
// event in the worker's log. The follower that applies the workspace's
// events, or its reconnect, finds the call; Mosso claims it from that same
// sandbox's worker, runs it here, for that workspace only, with the
// GitHub token, and answers. Which workspace a call acts for is decided by
// which sandbox it came from, never by its arguments: the arguments only
// name branches and pull requests, and are checked against the workspace.

const (
	// toolTimeout bounds running one tool call on the server.
	toolTimeout = publishTimeout
	// maxPRBodyBytes bounds a pull request description the agent sets, and
	// the part of one returned to it.
	maxPRBodyBytes = 64 << 10
	maxPRBodyShown = 16 << 10
	// maxAnswerBytes keeps an answer within what one command environment
	// variable carries to the worker.
	maxAnswerBytes = 100 << 10
)

var callIDPattern = regexp.MustCompile(`^call_[0-9a-f]{16}$`)

// pullRequestRecord is a pull request of the workspace, as Mosso last saw
// it: opened by the user with Create PR, or by the agent.
type pullRequestRecord struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Title  string `json:"title,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	// By is "agent" or "user".
	By string `json:"by,omitempty"`
}

// githubAction is a GitHub operation the agent had Mosso run, shown in the
// chat as a "github" entry: what changed on GitHub, or why it failed.
type githubAction struct {
	Tool        string             `json:"tool"`
	Text        string             `json:"text"`
	Error       string             `json:"error,omitempty"`
	Branch      string             `json:"branch,omitempty"`
	Commit      string             `json:"commit,omitempty"`
	PullRequest *pullRequestRecord `json:"pull_request,omitempty"`
}

// recordPullRequestLocked adds or updates one of the workspace's pull
// requests. The one from the working branch into the base branch is also
// the workspace's PullRequest, which Create PR / Update PR publishes to.
// Call with s.mu held.
func recordPullRequestLocked(ws *workspace, pr pullRequestRecord) {
	i := slices.IndexFunc(ws.PullRequests, func(p pullRequestRecord) bool { return p.Number == pr.Number })
	if i < 0 {
		ws.PullRequests = append(ws.PullRequests, pr)
	} else {
		if pr.By == "" {
			pr.By = ws.PullRequests[i].By
		}
		ws.PullRequests[i] = pr
	}
	if pr.Head == ws.Branch && pr.Base == ws.Base {
		ws.PullRequest = &github.PullRequest{Number: pr.Number, URL: pr.URL}
	}
}

// trackBranchLocked remembers a branch the workspace's mirror keeps in
// sync besides the base and working branch. Call with s.mu held.
func trackBranchLocked(ws *workspace, branch string) {
	if branch != ws.Base && branch != ws.Branch && !slices.Contains(ws.branches, branch) {
		ws.branches = append(ws.branches, branch)
	}
}

// handleToolCall runs a tool call from the workspace's sandbox, once: it
// claims the call from the worker (which refuses if it is not waiting, for
// example because it was already run or withdrawn), runs it and answers.
func (s *Server) handleToolCall(ws *workspace, id string) {
	if !callIDPattern.MatchString(id) {
		return
	}
	s.mu.Lock()
	if ws.toolCalls == nil {
		ws.toolCalls = map[string]bool{}
	}
	if ws.toolCalls[id] || ws.sandboxState != sandboxRunning || s.closed {
		s.mu.Unlock()
		return
	}
	ws.toolCalls[id] = true
	if ws.toolSem == nil {
		ws.toolSem = make(chan struct{}, 1)
	}
	session, sem := ws.Sandbox.SessionID, ws.toolSem
	// Shutdown waits for calls in progress, so what they changed on
	// GitHub is saved.
	s.tools.Add(1)
	s.mu.Unlock()
	defer s.tools.Done()
	defer func() {
		s.mu.Lock()
		delete(ws.toolCalls, id)
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(s.ctx, toolTimeout+time.Minute)
	defer cancel()
	// The answer goes back even while Mosso shuts down, so Claude is not
	// left waiting.
	answer := func(ans worker.CallAnswer) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for attempt := 0; ; attempt++ {
			res, err := s.workerCall(actx, session, http.MethodPost, "/tools/calls/"+id+"/result", ans)
			if err == nil && (res.Status == http.StatusNoContent || res.Status == http.StatusConflict) {
				return
			}
			if attempt == 2 || actx.Err() != nil {
				log.Printf("workspace %s: answer tool call %s: %v (HTTP %d)", ws.Name, id, err, res.Status)
				return
			}
			select {
			case <-actx.Done():
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}
	res, err := s.workerCall(ctx, session, http.MethodPost, "/tools/calls/"+id+"/start", nil)
	if err != nil {
		// The worker may have marked the call claimed without Mosso
		// hearing back; then only an answer frees Claude. If it did not,
		// the answer is refused and the call stays waiting.
		log.Printf("workspace %s: claim tool call %s: %v", ws.Name, id, err)
		answer(worker.CallAnswer{Error: "Mosso could not pick up this call (" + err.Error() + "), so nothing was done; try again"})
		return
	}
	if res.Status != http.StatusOK {
		return // not waiting: run, withdrawn or answered already
	}
	var call worker.ToolCall
	if err := json.Unmarshal(res.Body, &call); err != nil || call.ID != id {
		log.Printf("workspace %s: tool call %s: unexpected answer from the worker", ws.Name, id)
		answer(worker.CallAnswer{Error: "Mosso could not read this call from the sandbox, so nothing was done"})
		return
	}
	// One call at a time per workspace, so what they record (such as the
	// pushed commit) lands in the order they ran.
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		answer(worker.CallAnswer{Error: "Mosso stopped before running this call, so nothing was done"})
		return
	}
	runCtx, cancelRun := context.WithTimeout(ctx, toolTimeout)
	result, err := s.runTool(runCtx, ws, call)
	cancelRun()
	<-sem
	ans := worker.CallAnswer{}
	if err == nil {
		ans.Result, err = json.Marshal(result)
		if err == nil && len(ans.Result) > maxAnswerBytes {
			err = errors.New("the result is too large to return; narrow the request")
		}
	}
	if err != nil {
		ans.Result, ans.Error = nil, err.Error()
		log.Printf("workspace %s: %s: %v", ws.Name, call.Name, err)
	} else {
		log.Printf("workspace %s: %s done", ws.Name, call.Name)
	}
	answer(ans)
}

// toolEnv is what a tool call needs from its workspace.
type toolEnv struct {
	ws      *workspace
	repo    string
	base    string
	branch  string
	git     *github.Mirror
	gh      *github.Client
	secrets []string
}

// runTool runs one tool call for the workspace and returns its result.
// Errors are for the agent: they say what to do about them.
func (s *Server) runTool(ctx context.Context, ws *workspace, call worker.ToolCall) (any, error) {
	tool, ok := worker.FindTool(call.Name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
	s.mu.Lock()
	env := toolEnv{ws: ws, repo: ws.Repo, base: ws.Base, branch: ws.Branch, git: ws.git}
	s.mu.Unlock()
	if env.git == nil {
		return nil, errors.New("this workspace has no GitHub checkout set up in Mosso")
	}
	gh, _, err := s.githubClient(ctx)
	if errors.Is(err, github.ErrNoToken) {
		return nil, errors.New("GitHub is not connected in Mosso, so nothing was done: ask the user to connect GitHub in the Mosso web UI")
	}
	if err != nil {
		return nil, fmt.Errorf("Mosso could not load its GitHub token: %w", err)
	}
	env.gh = gh
	if tok, _, err := s.claudeToken(ctx); err == nil {
		env.secrets = append(env.secrets, tok.Reveal())
	}

	var result any
	var action *githubAction
	switch call.Name {
	case worker.ToolPush:
		var a worker.PushArgs
		if err = decodeArgs(call, &a); err == nil {
			result, action, err = s.toolPush(ctx, env, a)
		}
	case worker.ToolCreatePR:
		var a worker.CreatePRArgs
		if err = decodeArgs(call, &a); err == nil {
			result, action, err = s.toolCreatePR(ctx, env, a)
		}
	case worker.ToolListPRs:
		var a worker.ListPRsArgs
		if err = decodeArgs(call, &a); err == nil {
			result, err = s.toolListPRs(ctx, env, a)
		}
	case worker.ToolGetPR:
		var a worker.GetPRArgs
		if err = decodeArgs(call, &a); err == nil {
			result, err = s.toolGetPR(ctx, env, a)
		}
	case worker.ToolUpdatePR:
		var a worker.UpdatePRArgs
		if err = decodeArgs(call, &a); err == nil {
			result, action, err = s.toolUpdatePR(ctx, env, a)
		}
	case worker.ToolChecks:
		var a worker.ChecksArgs
		if err = decodeArgs(call, &a); err == nil {
			result, err = s.toolChecks(ctx, env, a)
		}
	case worker.ToolFetch:
		var a worker.FetchArgs
		if err = decodeArgs(call, &a); err == nil {
			result, err = s.toolFetch(ctx, env, a)
		}
	}
	if ctx.Err() != nil && err != nil {
		err = fmt.Errorf("%w (Mosso stopped waiting after %s; the outcome may be unknown: check with %s before trying again)", err, toolTimeout, worker.ToolListPRs)
	}
	// Changes to GitHub, and failures to make them, show in the chat.
	if tool.Writes {
		if err != nil {
			action = &githubAction{Tool: call.Name, Text: toolLabel(call.Name) + " failed", Error: err.Error()}
		}
		if action != nil {
			action.Tool = call.Name
			s.addEntry(ws.chat, chatEntry{Type: "github", GitHub: action})
		}
	}
	return result, err
}

// toolLabel is how the chat names a tool.
func toolLabel(name string) string {
	switch name {
	case worker.ToolPush:
		return "Push"
	case worker.ToolCreatePR:
		return "Creating a pull request"
	case worker.ToolUpdatePR:
		return "Updating a pull request"
	}
	return name
}

// decodeArgs decodes a call's arguments strictly, so a misspelled field is
// an error rather than ignored.
func decodeArgs(call worker.ToolCall, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments for %s: %v", call.Name, err)
	}
	return nil
}

// ownPR reports whether a pull request is one of the workspace's: from one
// of its own branches, in its repository.
func (e toolEnv) ownPR(pr *github.PullRequestInfo) error {
	if strings.EqualFold(pr.HeadRepo, e.repo) && github.OwnBranch(e.branch, pr.Head) {
		return nil
	}
	return fmt.Errorf("pull request #%d is not one of this workspace's pull requests (its head is %s): Mosso only lets you read and change pull requests from this workspace's branches (%s and %s-<suffix>)", pr.Number, pr.Head, e.branch, e.branch)
}

func (e toolEnv) branchURL(branch string) string {
	return "https://github.com/" + e.repo + "/tree/" + branch
}

func (s *Server) toolPush(ctx context.Context, e toolEnv, a worker.PushArgs) (any, *githubAction, error) {
	if a.Branch == "" {
		a.Branch = e.branch
	}
	p, err := s.gitPush(e.git, ctx, e.gh.Token(), github.PushOptions{Branch: a.Branch, Source: a.Source, Secrets: e.secrets})
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	trackBranchLocked(e.ws, p.Branch)
	if p.Branch == e.branch && p.Updated {
		e.ws.pushed = p.Commit
	}
	var prs []pullRequestRecord
	for _, pr := range e.ws.PullRequests {
		if pr.Head == p.Branch {
			prs = append(prs, pr)
		}
	}
	s.persistLocked(e.ws)
	s.mu.Unlock()
	out := map[string]any{
		"branch": p.Branch, "commit": p.Commit, "updated": p.Updated, "url": e.branchURL(p.Branch),
	}
	if p.Previous != "" {
		out["previous"] = p.Previous
	}
	if p.Dirty {
		out["note"] = "The sandbox has uncommitted changes; they were not pushed. Commit them and push again if they belong in this branch."
	}
	if len(prs) > 0 {
		out["pull_requests"] = prs
	}
	text := fmt.Sprintf("Pushed %s to %s", short(p.Commit), p.Branch)
	if !p.Updated {
		text = fmt.Sprintf("%s is already at %s on GitHub; nothing to push", p.Branch, short(p.Commit))
	}
	return out, &githubAction{Text: text, Branch: p.Branch, Commit: p.Commit}, nil
}

func (s *Server) toolCreatePR(ctx context.Context, e toolEnv, a worker.CreatePRArgs) (any, *githubAction, error) {
	a.Title = strings.TrimSpace(a.Title)
	if a.Head == "" {
		a.Head = e.branch
	}
	if a.Base == "" {
		a.Base = e.base
	}
	switch {
	case a.Title == "":
		return nil, nil, errors.New("title is required")
	case len(a.Title) > 256:
		return nil, nil, errors.New("title must be at most 256 bytes")
	case len(a.Body) > maxPRBodyBytes:
		return nil, nil, fmt.Errorf("body must be at most %d bytes", maxPRBodyBytes)
	case !github.OwnBranch(e.branch, a.Head):
		return nil, nil, fmt.Errorf("head must be one of this workspace's branches: %s, or %s-<suffix> pushed with %s; not %q", e.branch, e.branch, worker.ToolPush, a.Head)
	case !github.ValidBranch(a.Base):
		return nil, nil, fmt.Errorf("base %q is not a branch name Mosso accepts", a.Base)
	case strings.HasPrefix(a.Base, github.BranchPrefix):
		return nil, nil, fmt.Errorf("base must be one of the repository's own branches (such as %s), not a Mosso working branch", e.base)
	}
	// Fetching both checks they exist on GitHub, and gives the sandbox
	// origin/<base> from now on.
	heads, err := s.gitFetch(e.git, ctx, e.gh.Token(), a.Base, a.Head)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case heads[a.Base] == "":
		return nil, nil, fmt.Errorf("the base branch %s does not exist in %s", a.Base, e.repo)
	case heads[a.Head] == "":
		return nil, nil, fmt.Errorf("%s is not on GitHub yet: push it first with %s", a.Head, worker.ToolPush)
	}
	pr, created, err := e.gh.OpenPullRequest(ctx, e.repo, github.NewPullRequest{Title: a.Title, Body: a.Body, Head: a.Head, Base: a.Base, Draft: a.Draft})
	if err != nil {
		return nil, nil, err
	}
	rec := pullRequestRecord{Number: pr.Number, URL: pr.URL, Head: a.Head, Base: a.Base, Title: a.Title, Draft: a.Draft, By: "agent"}
	s.mu.Lock()
	trackBranchLocked(e.ws, a.Base)
	trackBranchLocked(e.ws, a.Head)
	if !created {
		// An existing pull request: keep what Mosso knew of it.
		if i := slices.IndexFunc(e.ws.PullRequests, func(p pullRequestRecord) bool { return p.Number == pr.Number }); i >= 0 {
			rec = e.ws.PullRequests[i]
		} else {
			// Opened outside Mosso: its title and draft status are not
			// known here.
			rec.Title, rec.Draft, rec.By = "", false, ""
		}
	}
	recordPullRequestLocked(e.ws, rec)
	e.ws.chat.err = ""
	s.persistLocked(e.ws)
	s.mu.Unlock()
	text := fmt.Sprintf("Opened pull request #%d: %s → %s", pr.Number, a.Head, a.Base)
	if !created {
		text = fmt.Sprintf("Pull request #%d (%s → %s) was already open", pr.Number, a.Head, a.Base)
	}
	out := map[string]any{"number": pr.Number, "url": pr.URL, "head": a.Head, "base": a.Base, "created": created}
	return out, &githubAction{Text: text, Branch: a.Head, PullRequest: &rec}, nil
}

// prSummary is a pull request as the tools return it.
func prSummary(pr *github.PullRequestInfo, withBody bool) map[string]any {
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	out := map[string]any{
		"number": pr.Number, "url": pr.URL, "title": pr.Title, "state": state, "draft": pr.Draft,
		"head": pr.Head, "base": pr.Base, "head_sha": pr.HeadSHA,
	}
	if withBody {
		body := pr.Body
		if len(body) > maxPRBodyShown {
			cut := maxPRBodyShown
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			body = body[:cut] + "\n[… truncated by Mosso]"
		}
		out["body"] = body
		if pr.Mergeable != nil {
			out["mergeable"] = *pr.Mergeable
		}
		if pr.MergeableState != "" {
			out["mergeable_state"] = pr.MergeableState
		}
	}
	return out
}

func (s *Server) toolListPRs(ctx context.Context, e toolEnv, a worker.ListPRsArgs) (any, error) {
	if a.State == "" {
		a.State = "all"
	}
	if a.State != "open" && a.State != "closed" && a.State != "all" {
		return nil, errors.New(`state must be "open", "closed" or "all"`)
	}
	s.mu.Lock()
	heads := []string{e.branch}
	for _, b := range e.ws.branches {
		if github.OwnBranch(e.branch, b) {
			heads = append(heads, b)
		}
	}
	s.mu.Unlock()
	list := []map[string]any{}
	for _, head := range heads {
		prs, err := e.gh.PullRequests(ctx, e.repo, head, a.State)
		if err != nil {
			return nil, err
		}
		for i := range prs {
			if e.ownPR(&prs[i]) == nil {
				list = append(list, prSummary(&prs[i], false))
			}
		}
	}
	return map[string]any{"pull_requests": list, "branches": heads}, nil
}

func (s *Server) toolGetPR(ctx context.Context, e toolEnv, a worker.GetPRArgs) (any, error) {
	if a.Number <= 0 {
		return nil, errors.New("number must be a pull request number")
	}
	pr, err := e.gh.GetPullRequest(ctx, e.repo, a.Number)
	if err != nil {
		return nil, err
	}
	if err := e.ownPR(pr); err != nil {
		return nil, err
	}
	return prSummary(pr, true), nil
}

func (s *Server) toolUpdatePR(ctx context.Context, e toolEnv, a worker.UpdatePRArgs) (any, *githubAction, error) {
	switch {
	case a.Number <= 0:
		return nil, nil, errors.New("number must be a pull request number")
	case a.Title == nil && a.Body == nil && a.Draft == nil:
		return nil, nil, errors.New("give at least one of title, body or draft to change")
	case a.Title != nil && (strings.TrimSpace(*a.Title) == "" || len(*a.Title) > 256):
		return nil, nil, errors.New("title must be 1 to 256 bytes")
	case a.Body != nil && len(*a.Body) > maxPRBodyBytes:
		return nil, nil, fmt.Errorf("body must be at most %d bytes", maxPRBodyBytes)
	}
	pr, err := e.gh.GetPullRequest(ctx, e.repo, a.Number)
	if err != nil {
		return nil, nil, err
	}
	if err := e.ownPR(pr); err != nil {
		return nil, nil, err
	}
	if pr.State != "open" {
		return nil, nil, fmt.Errorf("pull request #%d is %s; Mosso only changes open pull requests", pr.Number, map[bool]string{true: "merged", false: "closed"}[pr.Merged])
	}
	if a.Title != nil {
		t := strings.TrimSpace(*a.Title)
		a.Title = &t
	}
	pr, err = e.gh.UpdatePullRequest(ctx, e.repo, pr, github.PullRequestUpdate{Title: a.Title, Body: a.Body, Draft: a.Draft})
	if err != nil {
		return nil, nil, err
	}
	rec := pullRequestRecord{Number: pr.Number, URL: pr.URL, Head: pr.Head, Base: pr.Base, Title: pr.Title, Draft: pr.Draft, By: "agent"}
	s.mu.Lock()
	recordPullRequestLocked(e.ws, rec)
	s.persistLocked(e.ws)
	s.mu.Unlock()
	var changed []string
	if a.Title != nil {
		changed = append(changed, "title")
	}
	if a.Body != nil {
		changed = append(changed, "description")
	}
	if a.Draft != nil {
		changed = append(changed, map[bool]string{true: "marked as draft", false: "marked ready for review"}[*a.Draft])
	}
	text := fmt.Sprintf("Updated pull request #%d: %s", pr.Number, strings.Join(changed, ", "))
	return prSummary(pr, false), &githubAction{Text: text, PullRequest: &rec}, nil
}

func (s *Server) toolChecks(ctx context.Context, e toolEnv, a worker.ChecksArgs) (any, error) {
	var sha string
	switch {
	case (a.PR > 0) == (a.Branch != ""):
		return nil, errors.New("give either pr or branch")
	case a.PR > 0:
		pr, err := e.gh.GetPullRequest(ctx, e.repo, a.PR)
		if err != nil {
			return nil, err
		}
		if err := e.ownPR(pr); err != nil {
			return nil, err
		}
		sha = pr.HeadSHA
	default:
		if !github.ValidBranch(a.Branch) {
			return nil, fmt.Errorf("%q is not a branch name Mosso accepts", a.Branch)
		}
		if strings.HasPrefix(a.Branch, github.BranchPrefix) && !github.OwnBranch(e.branch, a.Branch) {
			return nil, fmt.Errorf("%s belongs to another Mosso workspace", a.Branch)
		}
		var err error
		if sha, err = e.gh.BranchHead(ctx, e.repo, a.Branch); err != nil {
			return nil, err
		}
	}
	checks, err := e.gh.CommitChecks(ctx, e.repo, sha)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"commit": checks.Commit, "state": checks.State, "checks": checks.Checks}
	if a.PR > 0 {
		out["pr"] = a.PR
	} else {
		out["branch"] = a.Branch
	}
	if len(checks.Missing) > 0 {
		out["missing"] = checks.Missing
	}
	return out, nil
}

func (s *Server) toolFetch(ctx context.Context, e toolEnv, a worker.FetchArgs) (any, error) {
	if !github.ValidBranch(a.Branch) {
		return nil, fmt.Errorf("%q is not a branch name Mosso accepts", a.Branch)
	}
	if strings.HasPrefix(a.Branch, github.BranchPrefix) && !github.OwnBranch(e.branch, a.Branch) {
		return nil, fmt.Errorf("%s belongs to another Mosso workspace", a.Branch)
	}
	heads, err := s.gitFetch(e.git, ctx, e.gh.Token(), a.Branch)
	if err != nil {
		return nil, err
	}
	if heads[a.Branch] == "" {
		return nil, fmt.Errorf("the branch %s does not exist in %s", a.Branch, e.repo)
	}
	s.mu.Lock()
	trackBranchLocked(e.ws, a.Branch)
	s.persistLocked(e.ws)
	s.mu.Unlock()
	return map[string]any{
		"branch": a.Branch, "commit": heads[a.Branch], "ref": "origin/" + a.Branch,
		"note": "Mosso keeps origin/" + a.Branch + " current before each of your turns.",
	}, nil
}
