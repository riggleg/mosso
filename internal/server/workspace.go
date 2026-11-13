package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

// defaultWorkspaceTimeout is the sandbox lifetime for a workspace when the
// request does not set one: long enough for a coding task and a pull request.
const defaultWorkspaceTimeout = 30 * time.Minute

// setupTimeout bounds provisioning, cloning and installing Claude Code.
const setupTimeout = 10 * time.Minute

// publishTimeout bounds committing and pushing a workspace's changes.
const publishTimeout = 5 * time.Minute

// workspace is a GitHub repository checked out on a working branch in a
// sandbox. Its name is the sandbox's name.
type workspace struct {
	Name    string         `json:"name"`
	Repo    string         `json:"repo"`
	Base    string         `json:"base"`
	BaseSHA string         `json:"base_sha"`
	Branch  string         `json:"branch"`
	Dir     string         `json:"dir"`
	Sandbox vercel.Sandbox `json:"sandbox"`
	// ExpiresAt is roughly when Vercel stops the sandbox.
	ExpiresAt   time.Time           `json:"expires_at"`
	PullRequest *github.PullRequest `json:"pull_request,omitempty"`
	// Status is "creating", "ready", "working" (Claude or a publish is
	// running) or "error" (the last turn or publish failed, or the sandbox
	// expired); Error says why. Both are computed by snapshotLocked.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	creating bool
	chat     *chat
}

// snapshotLocked returns a copy of ws with its current status.
func (s *Server) snapshotLocked(ws *workspace) workspace {
	c := *ws
	switch {
	case ws.creating:
		c.Status = "creating"
	case s.busy[ws.Name]:
		c.Status = "working"
	case time.Now().After(ws.ExpiresAt):
		c.Status, c.Error = "error", "The sandbox has expired. Terminate this workspace and create a new one."
	case ws.chat.err != "":
		c.Status, c.Error = "error", ws.chat.err
	default:
		c.Status = "ready"
	}
	return c
}

func loadGitHubClient(ctx context.Context) (*github.Client, string, error) {
	tok, source, err := github.LoadToken(ctx)
	if err != nil {
		return nil, "", err
	}
	return github.NewClient(tok), source, nil
}

// gitHub returns a GitHub client or writes an error response.
func (s *Server) gitHub(w http.ResponseWriter, r *http.Request) (*github.Client, bool) {
	gh, _, err := s.githubClient(r.Context())
	if err != nil {
		if errors.Is(err, github.ErrNoToken) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			log.Printf("load github token: %v", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return gh, true
}

// githubAuthStatus reports whether a GitHub token is configured and which
// account it belongs to. It never returns the token.
func (s *Server) githubAuthStatus(w http.ResponseWriter, r *http.Request) {
	gh, source, err := s.githubClient(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "error": err.Error()})
		return
	}
	user, err := gh.User(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "source": source, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "source": source, "login": user.Login})
}

func (s *Server) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	list := make([]workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		list = append(list, s.snapshotLocked(ws))
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Sandbox.CreatedAt.Before(list[j].Sandbox.CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": list})
}

func (s *Server) lookupWorkspace(w http.ResponseWriter, r *http.Request) (workspace, bool) {
	s.mu.Lock()
	ws, ok := s.workspaces[r.PathValue("name")]
	var copied workspace
	if ok {
		copied = s.snapshotLocked(ws)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
	}
	return copied, ok
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	if ws, ok := s.lookupWorkspace(w, r); ok {
		writeJSON(w, http.StatusOK, ws)
	}
}

// createWorkspace provisions a sandbox, clones the repository's base branch
// into it and checks out a new working branch. The sandbox is destroyed if
// any step fails.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo           string `json:"repo"`
		Base           string `json:"base"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch maxSeconds := int(vercel.MaxTimeout.Seconds()); {
	case !github.ValidRepo(req.Repo):
		writeError(w, http.StatusBadRequest, `repo must be a GitHub repository as "owner/repo"`)
		return
	case req.Base != "" && !github.ValidBranch(req.Base):
		writeError(w, http.StatusBadRequest, "base is not a valid branch name")
		return
	case req.TimeoutSeconds < 0 || req.TimeoutSeconds > maxSeconds:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeout_seconds must be between 0 and %d", maxSeconds))
		return
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = defaultWorkspaceTimeout
	}

	gh, ok := s.gitHub(w, r)
	if !ok {
		return
	}
	// Check access before paying for a sandbox.
	user, err := gh.User(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	repo, err := gh.Repo(r.Context(), req.Repo)
	if err != nil {
		status := http.StatusBadGateway
		if github.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	base := req.Base
	if base == "" {
		base = repo.DefaultBranch
	}
	if !github.ValidBranch(base) {
		writeError(w, http.StatusBadRequest, "the repository's default branch name is not supported; pass base")
		return
	}

	// Setup continues if the client disconnects (for example, the browser
	// reloads), so the workspace still appears; Shutdown cancels it.
	ctx, cancel := context.WithTimeout(s.ctx, setupTimeout)
	defer cancel()
	sb, status, err := s.provision(ctx, timeout)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	ws := &workspace{
		Name:      sb.Name,
		Repo:      repo.FullName,
		Base:      base,
		Branch:    github.NewBranchName(),
		Dir:       path.Join(claude.WorkDir, req.Repo[strings.IndexByte(req.Repo, '/')+1:]),
		Sandbox:   *sb,
		ExpiresAt: sb.CreatedAt.Add(timeout),
		creating:  true,
		chat:      newChat(),
	}
	if !github.ValidRepo(ws.Repo) {
		ws.Repo = req.Repo
	}
	// List the workspace as "creating" and hold it while cloning so nothing
	// else runs in it. Destroy it if it does not come up.
	s.mu.Lock()
	s.busy[sb.Name] = true
	s.workspaces[sb.Name] = ws
	s.mu.Unlock()
	ready := false
	defer func() {
		if ready {
			return
		}
		s.mu.Lock()
		s.dropWorkspaceLocked(sb.Name)
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
		defer cancel()
		if err := s.vercel.Destroy(ctx, *sb); err != nil {
			// Still tracked, so Shutdown tries again.
			log.Printf("destroy sandbox %s after failed workspace setup: %v (check the Vercel dashboard)", sb.Name, err)
		} else {
			s.forget(sb.Name)
		}
		s.release(sb.Name)
	}()

	name, email := user.CommitIdentity()
	opts := github.CloneOptions{
		Repo: ws.Repo, Base: base, Branch: ws.Branch, Dir: ws.Dir,
		AuthorName: name, AuthorEmail: email,
	}
	// Public repositories clone anonymously so the token never enters the
	// sandbox. Private ones need it, but only for this command, which runs
	// before Claude or any repository code has run in the sandbox.
	if repo.Private {
		opts.Token = gh.Token()
	}
	start := time.Now()
	baseSHA, err := github.Clone(ctx, s.vercel, sb.SessionID, opts)
	if err != nil {
		log.Printf("workspace %s: %v", sb.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("workspace %s: cloned %s@%s (%s) onto %s in %s",
		sb.Name, ws.Repo, base, baseSHA[:12], ws.Branch, time.Since(start).Round(time.Millisecond))
	// Install Claude Code now so the first message is quick. If this
	// fails, the first message tries again.
	_ = s.ensureClaude(ctx, sb.Name, sb.SessionID)

	s.mu.Lock()
	_, live := s.sandboxes[sb.Name]
	var snapshot workspace
	if live && s.workspaces[sb.Name] == ws {
		ws.BaseSHA = baseSHA
		ws.creating = false
		delete(s.busy, sb.Name)
		ws.chat.notify()
		snapshot = s.snapshotLocked(ws)
		ready = true
	}
	s.mu.Unlock()
	if !ready {
		writeError(w, http.StatusServiceUnavailable, "the sandbox was destroyed while the workspace was being set up")
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

// workspacePullRequest commits the workspace's changes, pushes them to its
// working branch on GitHub and opens a pull request into the base branch.
// Calling it again pushes new work to the same pull request.
func (s *Server) workspacePullRequest(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.lookupWorkspace(w, r)
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = "Changes from Mosso workspace " + ws.Name
	}
	if len(req.Title) > 256 {
		writeError(w, http.StatusBadRequest, "title must be at most 256 bytes")
		return
	}

	gh, ok := s.gitHub(w, r)
	if !ok {
		return
	}
	// Refuse to publish files that contain the Claude token too.
	var secrets []string
	if tok, _, err := s.claudeToken(r.Context()); err == nil {
		secrets = append(secrets, tok.Reveal())
	}

	if !s.acquire(ws.Name) {
		writeBusy(w)
		return
	}
	defer s.release(ws.Name)
	s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Publishing changes to GitHub…"})

	// Finish the publish even if the client disconnects, so the branch and
	// pull request are not left half-updated; Shutdown cancels it.
	ctx, cancel := context.WithTimeout(s.ctx, publishTimeout)
	defer cancel()
	pub, err := github.Publish(ctx, s.vercel, ws.Sandbox.SessionID, gh, github.PublishOptions{
		Repo: ws.Repo, Base: ws.Base, BaseSHA: ws.BaseSHA, Branch: ws.Branch, Dir: ws.Dir,
		Title: req.Title, Body: req.Body, Draft: req.Draft, Secrets: secrets,
	})
	if errors.Is(err, github.ErrNoChanges) {
		s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Nothing to publish yet: there are no changes against " + ws.Base + "."})
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		log.Printf("workspace %s: publish: %v", ws.Name, err)
		s.mu.Lock()
		ws.chat.err = "Publishing failed: " + err.Error()
		ws.chat.add(chatEntry{Type: "error", Text: ws.chat.err})
		s.mu.Unlock()
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	if cur, ok := s.workspaces[ws.Name]; ok {
		cur.PullRequest = &pub.PullRequest
	}
	ws.chat.err = ""
	ws.chat.add(chatEntry{Type: "pull_request", PullRequest: pub})
	s.mu.Unlock()
	log.Printf("workspace %s: pushed %s to %s, pull request %s (created=%t)", ws.Name, pub.Commit, ws.Branch, pub.URL, pub.Created)
	status := http.StatusOK
	if pub.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, pub)
}

// githubRepos lists repositories the GitHub token can access, for the web
// UI's repository picker.
func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.gitHub(w, r)
	if !ok {
		return
	}
	repos, err := gh.Repos(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// githubBranches lists a repository's branches, for the base branch picker.
func (s *Server) githubBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	if !github.ValidRepo(repo) {
		writeError(w, http.StatusBadRequest, `repository must be "owner/repo"`)
		return
	}
	gh, ok := s.gitHub(w, r)
	if !ok {
		return
	}
	branches, err := gh.Branches(r.Context(), repo)
	if err != nil {
		status := http.StatusBadGateway
		if github.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
}
