package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/keychain"
	"github.com/riggleg/mosso/internal/vercel"
)

// defaultWorkspaceTimeout is the sandbox session length for a workspace
// when the request does not set one. Sessions are the unit Vercel bills
// and limits (45 minutes on Hobby); the workspace itself persists across
// sessions, so the longest allowed session is the most convenient.
const defaultWorkspaceTimeout = vercel.MaxTimeout

// setupTimeout bounds provisioning, cloning and installing Claude Code; a
// setup script gets setupScriptTimeout more.
const setupTimeout = 10 * time.Minute

// publishTimeout bounds committing and pushing a workspace's changes.
const publishTimeout = 5 * time.Minute

// workspace is a GitHub repository checked out on a working branch in a
// persistent sandbox. Its name is the sandbox's name. It outlives the
// sandbox: once terminated, it is kept with its conversation as history.
type workspace struct {
	Name    string         `json:"name"`
	Repo    string         `json:"repo"`
	Base    string         `json:"base"`
	BaseSHA string         `json:"base_sha"`
	Branch  string         `json:"branch"`
	Dir     string         `json:"dir"`
	Sandbox vercel.Sandbox `json:"sandbox"`
	// Model is the Claude model every turn runs with, chosen when the
	// workspace is created and never changed; empty (Claude Code's
	// default) for workspaces from before models could be chosen.
	Model string `json:"model,omitempty"`
	// Image is the Vercel Container Registry image the sandbox was created
	// from, or empty for the built-in runtime. SetupScript ran once after
	// the clone; empty for none. Both are chosen at creation and kept for
	// reference.
	Image       string `json:"image,omitempty"`
	SetupScript string `json:"setup_script,omitempty"`
	// Instructions are the agent instructions added to every turn's
	// system prompt (see workspacePrompt): the section's, or what the
	// workspace was created with. Fixed at creation; empty for none.
	Instructions string `json:"instructions,omitempty"`
	// SectionID is the section the workspace is listed under, 0 for none
	// ("Unsectioned"). Moving the workspace changes only this; editing or
	// deleting the section never changes the workspace's configuration.
	// Guarded by Server.mu.
	SectionID int64 `json:"section_id,omitempty"`
	// Step says what the setup is doing while Status is "creating":
	// "cloning", "setup" (the setup script runs) or "installing" (Claude
	// Code and the worker).
	Step string `json:"step,omitempty"`
	// SandboxState is "running", "stopped" (resumes on the next message),
	// "gone" (deleted elsewhere or expired) or "terminated" (deleted by
	// terminating the workspace); computed by snapshotLocked.
	SandboxState string `json:"sandbox_state"`
	// ExpiresAt is when Vercel stops the current sandbox session; unset
	// while the sandbox is stopped.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	// ArchivedAt is set while the workspace is archived: still readable,
	// but listed apart from the rest of the history.
	ArchivedAt time.Time `json:"archived_at,omitzero"`
	// PullRequest is the pull request from the working branch into the
	// base branch, which Create PR / Update PR publishes to, once it
	// exists. PullRequests are all the workspace's pull requests, opened
	// by the user or the agent, into any base branch. Guarded by Server.mu.
	PullRequest  *github.PullRequest `json:"pull_request,omitempty"`
	PullRequests []pullRequestRecord `json:"pull_requests,omitempty"`
	// Status is "creating", "connecting" (checking the sandbox after a
	// restart), "ready", "working" (Claude or a publish is running),
	// "stopped" (the sandbox session ended; the next message resumes it),
	// "terminated" (history: the sandbox was deleted) or "error" (the last
	// turn or publish failed, or the sandbox is gone); Error says why. Both
	// are computed by snapshotLocked.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Unsaved says the last attempt to save the workspace in the database
	// failed, so what is shown may be lost if Mosso restarts before a
	// later save succeeds; computed by snapshotLocked.
	Unsaved bool `json:"unsaved,omitempty"`
	// Queue holds the messages sent while Claude was working, to be sent
	// in order when it is done (see queue.go); QueuePaused, if set, says
	// why they wait until the user resumes them: "stopped" or "failed".
	// Guarded by Server.mu.
	Queue       []queuedMessage `json:"queue,omitempty"`
	QueuePaused string          `json:"queue_paused,omitempty"`

	creating   bool
	connecting bool
	step       string // see Step
	chat       *chat
	// attachments are the files uploaded to the workspace's sandbox, by
	// ID; attachBytes is their total size. Guarded by Server.mu.
	attachments    map[string]attachment
	attachBytes    int64
	attachDirReady bool
	// git fetches and pushes for the workspace; nil until it is cloned.
	git *github.Mirror
	// pushed is the working branch head Mosso last pushed.
	pushed string
	// branches are the branches the mirror keeps in sync besides the base
	// and working branch: ones the agent fetched, pushed or opened a pull
	// request into. Guarded by Server.mu.
	branches []string
	// toolCalls are the agent's tool calls being run; guarded by
	// Server.mu. toolSem lets one of them run at a time.
	toolCalls map[string]bool
	toolSem   chan struct{}
	// unsaved is set while the database is behind what is in memory.
	unsaved bool
	// queuedPrompt is set while the first message given with the workspace
	// has not reached the worker; see workspaceState.QueuedPrompt.
	queuedPrompt bool

	// Sandbox worker state, guarded by Server.mu. lastSeq is the last
	// worker event applied to the chat; taskRunning says a Claude task is
	// in flight (busy because of Claude, not a publish); sandboxState is
	// one of the sandbox* constants; follower cancels the goroutine that
	// follows the worker's events; interruptedTask is a task Mosso already
	// reported as interrupted.
	lastSeq         int64
	taskRunning     bool
	sandboxState    string
	follower        context.CancelFunc
	interruptedTask string
}

// snapshotLocked returns a copy of ws with its current status.
func (s *Server) snapshotLocked(ws *workspace) workspace {
	c := *ws
	c.PullRequests = slices.Clone(ws.PullRequests)
	c.Queue = slices.Clone(ws.Queue)
	for i, q := range c.Queue {
		c.Queue[i].Sending = s.sendingLocked(ws, q)
	}
	c.SandboxState = ws.sandboxState
	c.Unsaved = ws.unsaved
	expired := !ws.ExpiresAt.IsZero() && time.Now().After(ws.ExpiresAt)
	switch {
	case ws.creating:
		c.Status, c.Step = "creating", ws.step
	case ws.connecting:
		c.Status = "connecting"
	case ws.sandboxState == sandboxTerminated:
		c.Status = "terminated"
	case ws.sandboxState == sandboxGone:
		c.Status, c.Error = "error", goneMessage
	case s.busy[ws.Name]:
		c.Status = "working"
	case ws.sandboxState == sandboxStopped || expired:
		c.Status = "stopped"
		if expired {
			c.ExpiresAt = time.Time{}
		}
	case ws.chat.err != "":
		c.Status, c.Error = "error", ws.chat.err
	default:
		c.Status = "ready"
	}
	return c
}

// loadGitHubClient returns a client with the current GitHub token: one
// pasted into the web UI this session (without a Keychain), else the
// Keychain's, else MOSSO_GITHUB_TOKEN. It reads the token on every call, so
// a replaced token is used from the next request on.
func (s *Server) loadGitHubClient(ctx context.Context) (*github.Client, string, error) {
	s.mu.Lock()
	mem := s.memoryToken
	s.mu.Unlock()
	if mem != "" {
		return s.newGitHubClient(mem), github.SourceMemory, nil
	}
	tok, source, err := github.LoadToken(ctx)
	if err != nil {
		return nil, "", err
	}
	return s.newGitHubClient(tok), source, nil
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

// githubAuth is the GitHub connection status. It never includes the token.
type githubAuth struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"` // keychain, env or memory
	Login      string `json:"login,omitempty"`
	// ExpiresAt is when the token expires, if GitHub reports it.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	// CreateURL opens GitHub's token form with Mosso's permissions.
	CreateURL string `json:"create_url"`
	// Storage is where a token saved with POST /github/auth goes:
	// "keychain", or "memory" (until Mosso stops) without a Keychain.
	Storage string `json:"storage"`
}

func (s *Server) authStatus(gh *github.Client, source string, user *github.User, err error) githubAuth {
	st := githubAuth{Configured: gh != nil, Source: source, CreateURL: github.NewTokenURL, Storage: "keychain"}
	if !keychain.Available() {
		st.Storage = github.SourceMemory
	}
	if err != nil {
		st.Error = err.Error()
	}
	if user != nil {
		st.Login = user.Login
	}
	if gh != nil {
		if t := gh.TokenExpiration(); !t.IsZero() {
			st.ExpiresAt = &t
		}
	}
	return st
}

// githubAuthStatus reports whether a GitHub token is configured and which
// account it belongs to. It never returns the token.
func (s *Server) githubAuthStatus(w http.ResponseWriter, r *http.Request) {
	gh, source, err := s.githubClient(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, s.authStatus(nil, "", nil, err))
		return
	}
	user, err := gh.User(r.Context())
	writeJSON(w, http.StatusOK, s.authStatus(gh, source, user, err))
}

// githubConnect stores a GitHub token pasted into the web UI, after
// checking it with GitHub: in the Keychain on macOS, otherwise in memory
// until Mosso stops. It replaces the previous token for every workspace,
// without a restart. The token is never logged or echoed back.
func (s *Server) githubConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tok, err := github.ParseToken(req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	gh := s.newGitHubClient(tok)
	user, err := gh.User(r.Context())
	if err != nil {
		var apiErr *github.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
			writeError(w, http.StatusUnprocessableEntity, "GitHub rejected this token: it is invalid, expired or revoked. Generate a new one.")
			return
		}
		writeError(w, http.StatusBadGateway, "could not check the token with GitHub: "+err.Error())
		return
	}
	source := github.SourceKeychain
	switch err := s.saveGitHubToken(r.Context(), tok); {
	case errors.Is(err, github.ErrNoKeychain):
		source = github.SourceMemory
	case err != nil:
		log.Printf("save github token: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the token: "+err.Error())
		return
	}
	s.mu.Lock()
	if source == github.SourceMemory {
		s.memoryToken = tok
	} else {
		s.memoryToken = ""
	}
	s.mu.Unlock()
	log.Printf("GitHub connected as %s (token saved: %s)", user.Login, source)
	writeJSON(w, http.StatusOK, s.authStatus(gh, source, user, nil))
}

// listWorkspaces lists every workspace, oldest first: the ones with a
// sandbox, the terminated ones kept as history, and the archived ones
// (archived_at is set). Like the other endpoints that only read saved
// workspaces, it works without Vercel credentials.
func (s *Server) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	list := make([]workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		list = append(list, s.snapshotLocked(ws))
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
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

// createWorkspace provisions a persistent sandbox from the requested image
// (or the default one, or the built-in runtime), clones the repository's
// base branch into it, checks out a new working branch, runs the setup
// script (the requested or default one, if any), installs Claude Code and
// starts the Mosso worker. The workspace runs the requested Claude model,
// or the default model. The sandbox is destroyed if any step fails,
// including the setup script.
//
// With a section_id, the section's configuration replaces the defaults:
// its repository, base branch, model, image, setup script and agent
// instructions apply unless the request gives its own, and the workspace
// is listed under the section. The workspace keeps the configuration it
// was created with; later changes to the section do not affect it.
//
// With a prompt, the chat shows it at once and Claude starts on it as soon
// as the workspace is ready, whether or not the client is still connected.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Repo is required, unless the section has one.
		Repo           string `json:"repo"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		// Prompt is the first message for Claude; optional.
		Prompt string `json:"prompt"`
		// SectionID is the section to start from and list the workspace
		// under; 0 for none.
		SectionID int64 `json:"section_id"`
		// Base, Model, Image, SetupScript and Instructions override the
		// section's (or, without one, the defaults) when present. ""
		// asks for the repository's default branch, the default model,
		// the built-in runtime, no setup script or no instructions.
		Base         *string `json:"base"`
		Model        *string `json:"model"`
		Image        *string `json:"image"`
		SetupScript  *string `json:"setup_script"`
		Instructions *string `json:"instructions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*(claude.MaxPromptBytes+maxSetupScriptBytes+maxInstructionsBytes))).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SectionID < 0 {
		writeError(w, http.StatusBadRequest, "section_id: no such section")
		return
	}
	// The starting point: the section, or the defaults without one.
	s.mu.Lock()
	cfg := section{Model: s.defaultModel, Image: s.defaultImage, SetupScript: s.defaultSetupScript}
	var sectionErr error
	if req.SectionID != 0 {
		sectionErr = errNoSection
		if s.store != nil {
			cfg, sectionErr = s.store.section(req.SectionID)
		}
		if sectionErr == nil && cfg.Model == "" {
			cfg.Model = s.defaultModel
		}
	}
	defaultModel := s.defaultModel
	s.mu.Unlock()
	switch {
	case errors.Is(sectionErr, errNoSection):
		writeError(w, http.StatusBadRequest, "section_id: no such section")
		return
	case sectionErr != nil:
		writeSectionError(w, "read the section", sectionErr)
		return
	}
	// Then what the request overrides.
	repoName, base, model := strings.TrimSpace(req.Repo), cfg.Base, cfg.Model
	if repoName == "" {
		repoName = cfg.Repo
	}
	if req.Base != nil {
		base = *req.Base
	}
	if req.Model != nil {
		if model = *req.Model; model == "" {
			model = defaultModel
		}
	}
	image, script, instructions := cfg.Image, cfg.SetupScript, cfg.Instructions
	switch maxSeconds := int(vercel.MaxTimeout.Seconds()); {
	case !github.ValidRepo(repoName):
		writeError(w, http.StatusBadRequest, `repo must be a GitHub repository as "owner/repo"`)
		return
	case base != "" && !github.ValidBranch(base):
		writeError(w, http.StatusBadRequest, "base is not a valid branch name")
		return
	case req.TimeoutSeconds < 0 || req.TimeoutSeconds > maxSeconds:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeout_seconds must be between 0 and %d", maxSeconds))
		return
	case !claude.KnownModel(model) && req.Model == nil && req.SectionID != 0:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the section's model %s is no longer offered: choose another model, or edit the section", model))
		return
	case !claude.KnownModel(model):
		writeError(w, http.StatusBadRequest, "model is not one of the models GET /settings lists")
		return
	case len(req.Prompt) > claude.MaxPromptBytes-promptReserve:
		writeError(w, http.StatusBadRequest, "prompt is too long")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		req.Prompt = ""
	}
	var err error
	if req.Image != nil {
		if image, err = normalizeImage(*req.Image); err != nil {
			writeError(w, http.StatusBadRequest, "image: "+err.Error())
			return
		}
	}
	if req.SetupScript != nil {
		if script, err = normalizeSetupScript(*req.SetupScript); err != nil {
			writeError(w, http.StatusBadRequest, "setup_script: "+err.Error())
			return
		}
	}
	if req.Instructions != nil {
		if instructions, err = normalizeInstructions(*req.Instructions); err != nil {
			writeError(w, http.StatusBadRequest, "instructions: "+err.Error())
			return
		}
	}
	// Without a Claude token the first message could not run: say so
	// before paying for a sandbox.
	var token claude.Token
	if req.Prompt != "" {
		var ok bool
		if token, ok = s.loadClaudeToken(w, r); !ok {
			return
		}
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
	repo, err := gh.Repo(r.Context(), repoName)
	if err != nil {
		status := http.StatusBadGateway
		if github.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if base == "" {
		base = repo.DefaultBranch
	}
	if !github.ValidBranch(base) {
		writeError(w, http.StatusBadRequest, "the repository's default branch name is not supported; pass base")
		return
	}

	// Setup continues if the client disconnects (for example, the browser
	// reloads), so the workspace still appears; Shutdown cancels it.
	budget := setupTimeout
	if script != "" {
		budget += setupScriptTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, budget)
	defer cancel()
	// Persistent: the sandbox snapshots itself when a session stops and
	// resumes later; one kept snapshot keeps storage flat.
	sb, status, err := s.provision(ctx, vercel.CreateOptions{Image: image, Timeout: timeout, Persistent: true, KeepLastSnapshots: 1})
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	ws := &workspace{
		Name:         sb.Name,
		Repo:         repo.FullName,
		Base:         base,
		Branch:       github.NewBranchName(),
		Dir:          path.Join(claude.WorkDir, repoName[strings.IndexByte(repoName, '/')+1:]),
		Model:        model,
		Image:        image,
		SetupScript:  script,
		Instructions: instructions,
		SectionID:    req.SectionID,
		Sandbox:      *sb,
		ExpiresAt:    sb.ExpiresAt(),
		CreatedAt:    time.Now().UTC(),
		creating:     true,
		step:         "cloning",
		chat:         newChat(),
		sandboxState: sandboxRunning,

		attachments: map[string]attachment{},
	}
	if ws.ExpiresAt.IsZero() {
		ws.ExpiresAt = sb.CreatedAt.Add(timeout)
	}
	if !github.ValidRepo(ws.Repo) {
		ws.Repo = repoName
	}
	ws.chat.persist = s.persister(ws)
	firstIdx := -1
	if req.Prompt != "" {
		firstIdx = ws.chat.add(chatEntry{Type: "user", Text: req.Prompt}).ID
		ws.queuedPrompt = true
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
	log.Printf("workspace %s: cloned %s@%s (%s) onto %s in %s (sandbox: %s)",
		sb.Name, ws.Repo, base, baseSHA[:12], ws.Branch, time.Since(start).Round(time.Millisecond), imageLabel(image))
	mirror, err := github.NewMirror(s.vercel, sb.SessionID, github.MirrorOptions{
		Repo: ws.Repo, Base: base, BaseSHA: baseSHA, Branch: ws.Branch, Dir: ws.Dir,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Meanwhile, fetch the base branch into the server's Git repository
	// so the first sync and publish are quick. Failures are retried then.
	primed := make(chan struct{})
	go func() {
		defer close(primed)
		if _, err := s.gitSync(mirror, ctx, gh.Token()); err != nil {
			log.Printf("workspace %s: %v", sb.Name, err)
			s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Could not fetch from GitHub on the Mosso server: " + err.Error() + " Mosso tries again before each message and when publishing."})
		}
	}()
	// The setup script runs once, before anything else runs in the
	// checkout. If it fails, the workspace is not created and the deferred
	// cleanup destroys the sandbox.
	if script != "" {
		s.setStep(ws, "setup")
		if err := s.runSetup(ctx, ws, sb.SessionID); err != nil {
			log.Printf("workspace %s: setup script: %v", sb.Name, err)
			cancel()
			<-primed
			_ = mirror.Close()
			status := http.StatusBadGateway
			if errors.Is(err, errSetupFailed) {
				status = http.StatusUnprocessableEntity
			}
			writeError(w, status, err.Error()+"\nThe workspace was not created and its sandbox is destroyed.")
			return
		}
	}
	// Install Claude Code and start the worker now so the first message is
	// quick. If either fails, the first message tries again.
	s.setStep(ws, "installing")
	_ = s.ensureClaude(ctx, sb.Name, sb.SessionID)
	if _, err := s.ensureWorker(ctx, sb.Name, sb.SessionID); err != nil {
		log.Printf("workspace %s: %v", sb.Name, err)
	}
	<-primed

	s.mu.Lock()
	_, live := s.sandboxes[sb.Name]
	var snapshot workspace
	var saveErr error
	if live && s.workspaces[sb.Name] == ws {
		ws.BaseSHA = baseSHA
		// deleteSection unsections the workspaces it finds; this one may
		// have been read from its section just before it was listed.
		if ws.SectionID != 0 && s.store != nil {
			if _, err := s.store.section(ws.SectionID); errors.Is(err, errNoSection) {
				ws.SectionID = 0
			}
		}
		// The workspace exists once it is saved, with the chat so far. If
		// it cannot be saved it is not created: Mosso would lose track of
		// its sandbox at the next restart.
		if saveErr = s.saveLocked(ws); saveErr == nil {
			ws.git = mirror
			ws.creating, ws.step = false, ""
			delete(s.busy, sb.Name)
			ws.chat.notify()
			snapshot = s.snapshotLocked(ws)
			ready = true
		}
	}
	s.mu.Unlock()
	if saveErr != nil {
		// The deferred cleanup removes the workspace and destroys the sandbox.
		_ = mirror.Close()
		log.Printf("workspace %s: save: %v", sb.Name, saveErr)
		writeError(w, http.StatusInternalServerError, "the workspace could not be saved, so it was not created and its sandbox is destroyed: "+saveErr.Error())
		return
	}
	if !ready {
		_ = mirror.Close()
		writeError(w, http.StatusServiceUnavailable, "the sandbox was destroyed while the workspace was being set up")
		return
	}
	// Follow the worker's events from now on.
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, resumeTimeout)
		defer cancel()
		if err := s.connect(ctx, ws); err != nil && ctx.Err() == nil {
			log.Printf("workspace %s: %v", ws.Name, err)
		}
	}()
	if req.Prompt != "" {
		if _, ok := s.startTurn(ws, token, req.Prompt, nil, nil, firstIdx); ok {
			s.mu.Lock()
			snapshot = s.snapshotLocked(ws)
			s.mu.Unlock()
		} else {
			// Only if another message got in first, which the web UI
			// does not do.
			s.mu.Lock()
			ws.queuedPrompt = false
			ws.chat.add(chatEntry{Type: "error", Text: "Your first message was not sent because another one was already running. Send it again."})
			s.persistLocked(ws)
			s.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

// setStep records what a workspace's setup is doing, for the web UI.
func (s *Server) setStep(ws *workspace, step string) {
	s.mu.Lock()
	ws.step = step
	ws.chat.notify()
	s.mu.Unlock()
}

// archiveWorkspace archives a terminated workspace (POST …/archive) or
// brings it back (POST …/unarchive). An archived workspace keeps its
// conversation and stays readable; the web UI lists it apart from the rest
// of the history. A workspace that still has a sandbox cannot be archived:
// out of sight, it would keep costing money. Archiving only touches the
// database, so it works without Vercel credentials; it fails if the change
// cannot be saved.
func (s *Server) archiveWorkspace(archive bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ws, ok := s.workspaces[r.PathValue("name")]
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "unknown workspace")
			return
		}
		if archive && ws.sandboxState != sandboxTerminated {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "terminate the workspace before archiving it: its sandbox still exists")
			return
		}
		if ws.creating {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "the workspace is still being set up")
			return
		}
		before, unsaved := ws.ArchivedAt, ws.unsaved
		switch {
		case !archive:
			ws.ArchivedAt = time.Time{}
		case ws.ArchivedAt.IsZero():
			ws.ArchivedAt = time.Now().UTC()
		}
		if err := s.saveLocked(ws); err != nil {
			// Not saved is not archived: it would come back after a restart.
			ws.ArchivedAt, ws.unsaved = before, unsaved
			s.mu.Unlock()
			log.Printf("workspace %s: save: %v", ws.Name, err)
			writeError(w, http.StatusInternalServerError, "the change could not be saved: "+err.Error())
			return
		}
		ws.chat.notify()
		snapshot := s.snapshotLocked(ws)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, snapshot)
	}
}

// workspacePullRequest commits the workspace's changes, pushes them to its
// working branch on GitHub and opens a pull request into the base branch.
// Calling it again pushes new work to the same pull request. A stopped
// sandbox is resumed first.
func (s *Server) workspacePullRequest(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByName(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
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

	s.mu.Lock()
	git, gone := ws.git, noSandbox(ws.sandboxState)
	s.mu.Unlock()
	if gone != "" {
		writeError(w, http.StatusGone, gone)
		return
	}
	if git == nil {
		writeError(w, http.StatusConflict, "the workspace is still being set up")
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
	fail := func(err error) {
		log.Printf("workspace %s: publish: %v", ws.Name, err)
		s.mu.Lock()
		ws.chat.err = "Publishing failed: " + err.Error()
		ws.chat.add(chatEntry{Type: "error", Text: ws.chat.err})
		s.mu.Unlock()
		writeError(w, http.StatusBadGateway, err.Error())
	}
	if err := s.connect(ctx, ws); err != nil {
		fail(err)
		return
	}
	pub, err := s.gitPublish(git, ctx, gh, github.PublishOptions{
		Title: req.Title, Body: req.Body, Draft: req.Draft, Secrets: secrets,
	})
	if errors.Is(err, github.ErrNoChanges) {
		s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Nothing to publish yet: the branch has no commits or changes beyond " + ws.Base + "."})
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		fail(err)
		return
	}
	s.mu.Lock()
	rec := pullRequestRecord{Number: pub.Number, URL: pub.URL, Head: ws.Branch, Base: ws.Base, By: "user"}
	if pub.Created {
		rec.Title, rec.Draft = req.Title, req.Draft
	} else if i := slices.IndexFunc(ws.PullRequests, func(p pullRequestRecord) bool { return p.Number == pub.Number }); i >= 0 {
		rec = ws.PullRequests[i]
	}
	recordPullRequestLocked(ws, rec)
	ws.pushed = pub.Commit
	ws.chat.err = ""
	ws.chat.add(chatEntry{Type: "pull_request", PullRequest: pub})
	s.persistLocked(ws)
	s.mu.Unlock()
	log.Printf("workspace %s: pushed %s to %s (forced=%t), pull request %s (created=%t)", ws.Name, pub.Commit, ws.Branch, pub.Forced, pub.URL, pub.Created)
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
