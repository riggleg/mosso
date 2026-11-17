// Package server contains Mosso's HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// maxCommandBytes bounds the size of a command submitted to /exec.
const maxCommandBytes = 64 * 1024

// Server serves Mosso's HTTP API and tracks the sandboxes it has created so
// they can be destroyed on shutdown.
type Server struct {
	vercel *vercel.Client // nil when credentials are not configured

	// claudeToken loads the Claude Code token; replaced in tests.
	claudeToken func(context.Context) (claude.Token, string, error)
	// githubClient returns a GitHub client with the current token and
	// where the token came from; replaced in tests.
	githubClient func(context.Context) (*github.Client, string, error)
	// newGitHubClient returns a client for a token pasted into the web
	// UI, to check it; replaced in tests.
	newGitHubClient func(github.Token) *github.Client
	// saveGitHubToken stores a pasted token in the Keychain, or returns
	// github.ErrNoKeychain; replaced in tests.
	saveGitHubToken func(context.Context, github.Token) error
	// gitSync and gitPublish run a workspace's authenticated Git
	// operations on this server; replaced in tests.
	gitSync    func(*github.Mirror, context.Context, github.Token) (github.Synced, error)
	gitPublish func(*github.Mirror, context.Context, *github.Client, github.PublishOptions) (*github.Published, error)
	// gitPush and gitFetch run the agent's pushes and fetches (see
	// tools.go); replaced in tests.
	gitPush  func(*github.Mirror, context.Context, github.Token, github.PushOptions) (*github.Pushed, error)
	gitFetch func(*github.Mirror, context.Context, github.Token, ...string) (map[string]string, error)
	// workerBinary returns the mosso-worker binary to upload into a
	// sandbox; replaced in tests.
	workerBinary func(context.Context) ([]byte, error)
	// store saves workspaces and chats in the database; nil keeps them
	// in memory only. Guarded by mu.
	store *store

	// ctx is cancelled by Shutdown; background work (Claude turns,
	// workspace setup) derives from it instead of from a request.
	ctx    context.Context
	cancel context.CancelFunc
	// streamsDone is closed by CloseStreams to end event streams.
	streamsDone  chan struct{}
	closeStreams sync.Once

	mu sync.Mutex
	// sandboxes are the sandboxes that exist, as far as Mosso knows; a
	// terminated workspace has none.
	sandboxes map[string]vercel.Sandbox
	closed    bool // set by Shutdown; later creates are destroyed immediately
	// Per-sandbox state, keyed by sandbox name.
	claudeInstalled map[string]bool
	busy            map[string]bool // a Claude turn, clone or publish is running
	// workspaces includes the terminated ones, kept as history.
	workspaces map[string]*workspace
	// defaultModel is the Claude model new workspaces run unless the
	// request names one; saved in the database (see settings.go).
	defaultModel string
	// defaultImage and defaultSetupScript are the sandbox image and setup
	// script new workspaces get unless the request overrides them; empty
	// for the built-in runtime and no setup. Saved in the database.
	defaultImage       string
	defaultSetupScript string
	// defaultSection is the section the web UI preselects for new
	// workspaces, 0 for none. Saved in the database.
	defaultSection int64
	// memoryToken is a GitHub token pasted into the web UI on a system
	// without a Keychain. It lasts until Mosso stops and takes precedence
	// over MOSSO_GITHUB_TOKEN.
	memoryToken github.Token

	// cleanups tracks removal of workspaces' server-side Git repositories.
	cleanups sync.WaitGroup
	// tools tracks the agent's tool calls being run (see tools.go).
	tools sync.WaitGroup

	// plan is the Vercel plan as last checked (see plan.go); guarded by
	// mu. planMu makes checks run one at a time.
	plan         planInfo
	planMu       sync.Mutex
	planForceGap time.Duration
	// idleCheck is how often idle workspaces are looked for (see idle.go).
	idleCheck time.Duration
}

// New returns a Server. client may be nil, in which case sandbox endpoints
// respond with 503 Service Unavailable.
func New(client *vercel.Client) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ctx:             ctx,
		cancel:          cancel,
		streamsDone:     make(chan struct{}),
		vercel:          client,
		claudeToken:     claude.LoadToken,
		newGitHubClient: github.NewClient,
		saveGitHubToken: github.SaveToken,
		gitSync:         (*github.Mirror).Sync,
		gitPublish:      (*github.Mirror).Publish,
		gitPush:         (*github.Mirror).Push,
		gitFetch:        (*github.Mirror).Fetch,
		workerBinary:    worker.Binary,
		sandboxes:       map[string]vercel.Sandbox{},
		claudeInstalled: map[string]bool{},
		busy:            map[string]bool{},
		workspaces:      map[string]*workspace{},
		defaultModel:    claude.DefaultModel,
		idleCheck:       idleCheckInterval,
		planForceGap:    planForceGap,
	}
	s.githubClient = s.loadGitHubClient
	if client != nil {
		go s.idleLoop() // until Shutdown; see idle.go
	}
	return s
}

// NewHandler returns a handler without sandbox support.
func NewHandler() http.Handler { return New(nil).Handler() }

// Handler returns the HTTP routes. Do not expose the server publicly until
// authentication and authorization have been implemented.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /sandboxes", s.requireVercel(s.listSandboxes))
	mux.HandleFunc("POST /sandboxes", s.requireVercel(s.createSandbox))
	mux.HandleFunc("POST /sandboxes/{name}/exec", s.requireVercel(s.execSandbox))
	mux.HandleFunc("DELETE /sandboxes/{name}", s.requireVercel(s.destroySandbox))
	mux.HandleFunc("GET /vercel/auth", s.vercelAuthStatus)
	mux.HandleFunc("GET /claude/auth", s.claudeAuthStatus)
	mux.HandleFunc("POST /sandboxes/{name}/claude/messages", s.requireVercel(s.claudeMessage))
	mux.HandleFunc("GET /github/auth", s.githubAuthStatus)
	mux.HandleFunc("POST /github/auth", s.githubConnect)
	mux.HandleFunc("GET /settings", s.getSettings)
	mux.HandleFunc("POST /settings", s.updateSettings)
	// Sections only need the database, like the history below.
	mux.HandleFunc("GET /sections", s.listSections)
	mux.HandleFunc("POST /sections", s.createSection)
	mux.HandleFunc("PATCH /sections/{id}", s.updateSection)
	mux.HandleFunc("DELETE /sections/{id}", s.deleteSection)
	mux.HandleFunc("POST /sections/{id}/duplicate", s.duplicateSection)
	// Reading saved workspaces and their conversations, and archiving,
	// only need the database, so they work without Vercel credentials.
	// Everything that touches a sandbox requires them.
	mux.HandleFunc("GET /workspaces", s.listWorkspaces)
	mux.HandleFunc("GET /workspaces/{name}", s.getWorkspace)
	mux.HandleFunc("GET /workspaces/{name}/events", s.workspaceEvents)
	mux.HandleFunc("POST /workspaces/{name}/archive", s.archiveWorkspace(true))
	mux.HandleFunc("POST /workspaces/{name}/unarchive", s.archiveWorkspace(false))
	mux.HandleFunc("POST /workspaces/{name}/section", s.moveWorkspace)
	mux.HandleFunc("POST /workspaces", s.requireVercel(s.createWorkspace))
	mux.HandleFunc("POST /workspaces/{name}/claude/messages", s.requireVercel(s.workspaceClaudeMessage))
	mux.HandleFunc("POST /workspaces/{name}/pull-request", s.requireVercel(s.workspacePullRequest))
	mux.HandleFunc("DELETE /workspaces/{name}", s.requireVercel(s.destroySandbox))
	mux.HandleFunc("POST /workspaces/{name}/chat", s.requireVercel(s.workspaceChat))
	mux.HandleFunc("POST /workspaces/{name}/attachments", s.requireVercel(s.uploadAttachments))
	mux.HandleFunc("GET /workspaces/{name}/attachments/{id}", s.requireVercel(s.downloadAttachment))
	mux.HandleFunc("POST /workspaces/{name}/chat/stop", s.requireVercel(s.workspaceStop))
	mux.HandleFunc("POST /workspaces/{name}/connect", s.requireVercel(s.workspaceConnect))
	mux.HandleFunc("POST /workspaces/{name}/sandbox/stop", s.requireVercel(s.workspaceSandboxStop))
	// The idle timeout is only saved, so it can be changed without Vercel.
	mux.HandleFunc("PATCH /workspaces/{name}", s.updateWorkspace)
	mux.HandleFunc("GET /github/repos", s.githubRepos)
	mux.HandleFunc("GET /github/repos/{owner}/{repo}/branches", s.githubBranches)
	ui := webUI()
	mux.Handle("GET /{$}", ui)
	mux.Handle("GET /assets/", ui)
	return localOnly(mux)
}

// CheckListenAddr rejects listen addresses that are not loopback. The server
// has no authentication, so it must not be reachable from other machines.
func CheckListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("refusing to listen on non-loopback address %q: the server has no authentication", addr)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// localOnly protects the unauthenticated API from browsers: it rejects
// requests whose Host is not loopback (DNS rebinding), cross-origin requests
// from non-loopback pages, and POSTs and PATCHes without a JSON content type
// (a POST would otherwise be a "simple" cross-site request sent without a
// CORS preflight).
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !isLoopbackHost(host) {
			writeError(w, http.StatusForbidden, "requests must be addressed to a loopback host")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !isLoopbackHost(u.Hostname()) {
				writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			// Uploads are multipart; the Host and Origin checks above
			// still apply to them.
			upload := mt == "multipart/form-data" && strings.HasSuffix(r.URL.Path, "/attachments") && strings.HasPrefix(r.URL.Path, "/workspaces/")
			if mt != "application/json" && !upload {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireVercel(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.vercel == nil {
			writeError(w, http.StatusServiceUnavailable,
				"Vercel Sandbox is not configured: set VERCEL_TOKEN, VERCEL_TEAM_ID and VERCEL_PROJECT_ID")
			return
		}
		h(w, r)
	}
}

func (s *Server) listSandboxes(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	list := make([]vercel.Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		list = append(list, sb)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list})
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if plan := s.maxSessionFor(r.Context(), timeout); req.TimeoutSeconds < 0 || timeout > plan.maxSession() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeout_seconds must be between 0 and %d, the longest session %s allows", plan.MaxSessionSeconds, planLabel(plan.Plan)))
		return
	}
	sb, status, err := s.provision(r.Context(), vercel.CreateOptions{Timeout: timeout})
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// provision creates a sandbox and tracks it so it is destroyed on shutdown.
// On error it returns the HTTP status to report. How many sandboxes may
// run at once is Vercel's to decide, per account: a refusal is reported as
// 429 (see capacityError).
func (s *Server) provision(ctx context.Context, opts vercel.CreateOptions) (*vercel.Sandbox, int, error) {
	name := vercel.NewSandboxName()
	sb, err := s.vercel.Create(ctx, name, opts)
	if err != nil {
		log.Printf("create sandbox %s: %v", name, err)
		if vercel.OverCapacity(err) {
			return nil, http.StatusTooManyRequests, capacityError(err)
		}
		if opts.Timeout > vercel.HobbyMaxTimeout {
			// Perhaps the plan changed since it was checked.
			go s.vercelPlan(context.WithoutCancel(ctx), true)
		}
		return nil, http.StatusBadGateway, err
	}
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.sandboxes[sb.Name] = *sb
	}
	s.mu.Unlock()
	if closed {
		// Shutdown already ran its destroy pass; don't leave this one behind.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := s.vercel.Destroy(dctx, *sb); err != nil {
			log.Printf("destroy sandbox %s created during shutdown: %v", sb.Name, err)
		}
		return nil, http.StatusServiceUnavailable, errors.New("server is shutting down")
	}
	log.Printf("created sandbox %s (session %s)", sb.Name, sb.SessionID)
	return sb, 0, nil
}

// capacityError explains Vercel refusing to start a sandbox session because
// the account runs as many sandboxes as its plan allows, or allocates them
// too quickly.
func capacityError(err error) error {
	return fmt.Errorf("Vercel refused to start another sandbox: your account has reached its limit of concurrent sandboxes or how quickly they can be started (%w). Stop or terminate a workspace you are not using, or wait a minute, and try again", err)
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (vercel.Sandbox, bool) {
	name := r.PathValue("name")
	s.mu.Lock()
	sb, ok := s.sandboxes[name]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown sandbox")
	}
	return sb, ok
}

func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCommandBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	res, err := s.vercel.Exec(r.Context(), sb.SessionID, req.Command)
	if err != nil {
		// Command text is deliberately not logged: it may contain secrets.
		log.Printf("exec in sandbox %s: %v", sb.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// destroySandbox terminates a sandbox or workspace: the session is stopped
// and the sandbox and its snapshots are deleted. A workspace stays in
// Mosso, with its conversation, as history (see terminateLocked); one that
// is still being set up is removed altogether. The branch and pull request
// stay on GitHub.
//
// It only reports success once the termination is saved. If the sandbox
// was deleted but that could not be saved, it answers 500 and says so;
// calling it again saves it. (Left unsaved, the workspace would show as
// "gone" after a restart, when Mosso finds no sandbox: the conversation is
// kept either way and nothing is left running.)
func (s *Server) destroySandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	sb, ok := s.sandboxes[name]
	ws := s.workspaces[name]
	if ok && ws != nil && ws.follower != nil {
		ws.follower() // stop following before the session goes away
		ws.follower = nil
	}
	var saveErr error
	if !ok && ws != nil && ws.unsaved {
		// Already terminated, so there is no sandbox left to delete, but an
		// earlier attempt could not save that: try again.
		saveErr = s.saveLocked(ws)
	}
	s.mu.Unlock()
	if !ok {
		switch {
		case ws == nil:
			writeError(w, http.StatusNotFound, "unknown sandbox")
		case saveErr != nil:
			s.terminateUnsaved(w, name, saveErr)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	// Finish cleanup even if the client disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	if err := s.vercel.Destroy(ctx, sb); err != nil {
		log.Printf("destroy sandbox %s: %v", sb.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	if cur := s.workspaces[name]; cur != nil && cur == ws && !ws.creating {
		saveErr = s.terminateLocked(ws)
	} else {
		s.forgetLocked(name)
	}
	s.mu.Unlock()
	log.Printf("destroyed sandbox %s", sb.Name)
	if saveErr != nil {
		s.terminateUnsaved(w, name, saveErr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// terminateUnsaved answers a terminate whose sandbox is deleted but whose
// outcome could not be saved.
func (s *Server) terminateUnsaved(w http.ResponseWriter, name string, err error) {
	log.Printf("workspace %s: terminated, but not saved: %v", name, err)
	writeError(w, http.StatusInternalServerError,
		"the sandbox and its snapshots were deleted, but Mosso could not save that the workspace is terminated: "+err.Error()+
			". Terminate it again to save it; if Mosso restarts first, the workspace shows as gone, with its conversation.")
}

// terminateLocked records that the workspace's sandbox has been deleted.
// The workspace and its conversation stay, in memory and in the database,
// as history: it can be read and archived, but nothing runs in it any
// more. It returns an error if that could not be saved; the workspace is
// terminated in memory regardless, because its sandbox no longer exists.
// Call with s.mu held.
func (s *Server) terminateLocked(ws *workspace) error {
	delete(s.sandboxes, ws.Name)
	delete(s.claudeInstalled, ws.Name)
	if ws.follower != nil {
		ws.follower()
		ws.follower = nil
	}
	ws.sandboxState = sandboxTerminated
	ws.Sandbox.Status = "stopped"
	ws.ExpiresAt = time.Time{}
	if ws.taskRunning {
		if run := ws.chat.running; run != nil {
			run.err = errors.New("the workspace was terminated")
		}
		s.finishTurnLocked(ws, ws.chat.running)
	}
	ws.chat.err = ""
	ws.chat.add(chatEntry{Type: "notice", Text: "Workspace terminated: its sandbox and snapshots were deleted. This conversation is kept as history."})
	if ws.git != nil {
		s.closeGit(ws.Name, ws.git)
		ws.git = nil
	}
	return s.saveLocked(ws)
}

// closeGit removes a workspace's server-side Git repository in the
// background: Close waits for a fetch or push in progress, so it must not
// run under mu.
func (s *Server) closeGit(name string, git *github.Mirror) {
	s.cleanups.Add(1)
	go func() {
		defer s.cleanups.Done()
		if err := git.Close(); err != nil {
			log.Printf("workspace %s: remove the server-side Git repository: %v", name, err)
		}
	}()
}

// acquire marks a sandbox busy so that Claude turns, clones and publishes in
// it run one at a time. It reports false if something else is running.
func (s *Server) acquire(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[name] {
		return false
	}
	s.busy[name] = true
	s.changedLocked(name)
	return true
}

func (s *Server) release(name string) {
	s.mu.Lock()
	delete(s.busy, name)
	if ws, ok := s.workspaces[name]; ok {
		s.touchLocked(ws) // a publish or setup just ended
	}
	s.changedLocked(name)
	s.mu.Unlock()
}

// changedLocked wakes the event streams of a workspace whose status changed.
func (s *Server) changedLocked(name string) {
	if ws, ok := s.workspaces[name]; ok {
		ws.chat.notify()
	}
}

func writeBusy(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "another request is still running in this sandbox; wait for it to finish")
}

// forget stops tracking a sandbox that no longer exists, and removes the
// workspace being set up in it, if any.
func (s *Server) forget(name string) {
	s.mu.Lock()
	s.forgetLocked(name)
	s.mu.Unlock()
}

func (s *Server) forgetLocked(name string) {
	delete(s.sandboxes, name)
	delete(s.claudeInstalled, name)
	s.dropWorkspaceLocked(name)
}

// dropWorkspaceLocked removes a workspace that did not finish setting up
// (nothing of it has been saved yet), and ends its event streams.
func (s *Server) dropWorkspaceLocked(name string) {
	ws, ok := s.workspaces[name]
	if !ok {
		return
	}
	delete(s.workspaces, name)
	if ws.follower != nil {
		ws.follower()
		ws.follower = nil
	}
	if ws.chat.running != nil {
		run := ws.chat.running
		run.err = errors.New("the workspace was terminated")
		ws.chat.running = nil
		select {
		case <-run.done:
		default:
			close(run.done)
		}
	}
	ws.chat.notify()
	if ws.git != nil {
		s.closeGit(name, ws.git)
	}
}

// Shutdown prepares for the process to exit. Workspaces are left alone:
// their sandboxes keep running (so Claude can finish what it is doing) and
// Mosso reconnects to them on the next start; their saved state is already
// in the database, which is closed here. Plain sandboxes made with
// POST /sandboxes are destroyed, as they are tracked in memory only.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel() // stop following workers; cancel workspace setup, publishes and tool calls
	defer s.closeStore()
	defer s.cleanups.Wait()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	// Tool calls save what they did on GitHub before the database closes.
	s.tools.Wait()
	if s.vercel == nil {
		return nil
	}
	s.mu.Lock()
	pending := make([]vercel.Sandbox, 0, len(s.sandboxes))
	for name, sb := range s.sandboxes {
		ws, ok := s.workspaces[name]
		if !ok {
			pending = append(pending, sb)
			continue
		}
		s.persistLocked(ws)
		if ws.git != nil {
			s.closeGit(name, ws.git) // the server-side Git repository is recreated on demand
		}
	}
	// One more try for workspaces whose last save failed (for example a
	// terminated one, which has no sandbox to be listed under above).
	for name, ws := range s.workspaces {
		if _, hasSandbox := s.sandboxes[name]; !hasSandbox && ws.unsaved {
			s.persistLocked(ws)
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, sb := range pending {
		if err := s.vercel.Destroy(ctx, sb); err != nil {
			errs = append(errs, err)
			log.Printf("shutdown: destroy sandbox %s: %v", sb.Name, err)
			continue
		}
		s.forget(sb.Name)
		log.Printf("shutdown: destroyed sandbox %s", sb.Name)
	}
	return errors.Join(errs...)
}

// closeStore closes the database; nothing is saved after it.
func (s *Server) closeStore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return
	}
	if err := s.store.close(); err != nil {
		log.Printf("close the database: %v", err)
	}
	s.store = nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
