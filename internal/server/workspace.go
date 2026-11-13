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

// workspace is a GitHub repository checked out on a working branch in a
// sandbox. Its name is the sandbox's name.
type workspace struct {
	Name        string              `json:"name"`
	Repo        string              `json:"repo"`
	Base        string              `json:"base"`
	BaseSHA     string              `json:"base_sha"`
	Branch      string              `json:"branch"`
	Dir         string              `json:"dir"`
	Sandbox     vercel.Sandbox      `json:"sandbox"`
	PullRequest *github.PullRequest `json:"pull_request,omitempty"`
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
		list = append(list, *ws)
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
		copied = *ws
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

	sb, status, err := s.provision(r.Context(), timeout)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	// Hold the sandbox while cloning so nothing else runs in it, and destroy
	// it if the workspace does not come up.
	s.acquire(sb.Name)
	defer s.release(sb.Name)
	ready := false
	defer func() {
		if !ready {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
			defer cancel()
			if err := s.vercel.Destroy(ctx, *sb); err != nil {
				log.Printf("destroy sandbox %s after failed workspace setup: %v (check the Vercel dashboard)", sb.Name, err)
				return
			}
			s.forget(sb.Name)
		}
	}()

	ws := &workspace{
		Name:    sb.Name,
		Repo:    repo.FullName,
		Base:    base,
		Branch:  github.NewBranchName(),
		Dir:     path.Join(claude.WorkDir, req.Repo[strings.IndexByte(req.Repo, '/')+1:]),
		Sandbox: *sb,
	}
	if !github.ValidRepo(ws.Repo) {
		ws.Repo = req.Repo
	}
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
	ws.BaseSHA, err = github.Clone(r.Context(), s.vercel, sb.SessionID, opts)
	if err != nil {
		log.Printf("workspace %s: %v", sb.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.mu.Lock()
	_, live := s.sandboxes[sb.Name]
	if live {
		s.workspaces[sb.Name] = ws
		ready = true
	}
	s.mu.Unlock()
	if !live {
		writeError(w, http.StatusServiceUnavailable, "the sandbox was destroyed while the repository was being cloned")
		return
	}
	log.Printf("workspace %s: cloned %s@%s (%s) onto %s in %s",
		sb.Name, ws.Repo, base, ws.BaseSHA[:12], ws.Branch, time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusCreated, ws)
}

// workspaceClaudeMessage runs a Claude Code turn in the workspace checkout.
// The GitHub token is not available to Claude.
func (s *Server) workspaceClaudeMessage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.lookupWorkspace(w, r)
	if !ok {
		return
	}
	s.sendClaude(w, r, ws.Sandbox, ws.Dir)
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

	pub, err := github.Publish(r.Context(), s.vercel, ws.Sandbox.SessionID, gh, github.PublishOptions{
		Repo: ws.Repo, Base: ws.Base, BaseSHA: ws.BaseSHA, Branch: ws.Branch, Dir: ws.Dir,
		Title: req.Title, Body: req.Body, Draft: req.Draft, Secrets: secrets,
	})
	if errors.Is(err, github.ErrNoChanges) {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		log.Printf("workspace %s: publish: %v", ws.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	if cur, ok := s.workspaces[ws.Name]; ok {
		cur.PullRequest = &pub.PullRequest
	}
	s.mu.Unlock()
	log.Printf("workspace %s: pushed %s to %s, pull request %s (created=%t)", ws.Name, pub.Commit, ws.Branch, pub.URL, pub.Created)
	status := http.StatusOK
	if pub.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, pub)
}
