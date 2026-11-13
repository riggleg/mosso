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
)

// maxCommandBytes bounds the size of a command submitted to /exec.
const maxCommandBytes = 64 * 1024

// maxSandboxes caps how many sandboxes may exist (or be in creation) at once,
// bounding the cost of a runaway or buggy local client.
const maxSandboxes = 5

// Server serves Mosso's HTTP API and tracks the sandboxes it has created so
// they can be destroyed on shutdown.
type Server struct {
	vercel *vercel.Client // nil when credentials are not configured

	// claudeToken loads the Claude Code token; replaced in tests.
	claudeToken func(context.Context) (claude.Token, string, error)
	// githubClient returns a GitHub client with the current token and
	// where the token came from; replaced in tests.
	githubClient func(context.Context) (*github.Client, string, error)

	mu        sync.Mutex
	sandboxes map[string]vercel.Sandbox
	creating  int  // creates in flight, counted against maxSandboxes
	closed    bool // set by Shutdown; later creates are destroyed immediately
	// Per-sandbox state, keyed by sandbox name.
	claudeInstalled map[string]bool
	busy            map[string]bool // a Claude turn, clone or publish is running
	workspaces      map[string]*workspace
}

// New returns a Server. client may be nil, in which case sandbox endpoints
// respond with 503 Service Unavailable.
func New(client *vercel.Client) *Server {
	return &Server{
		vercel:          client,
		claudeToken:     claude.LoadToken,
		githubClient:    loadGitHubClient,
		sandboxes:       map[string]vercel.Sandbox{},
		claudeInstalled: map[string]bool{},
		busy:            map[string]bool{},
		workspaces:      map[string]*workspace{},
	}
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
	mux.HandleFunc("GET /claude/auth", s.claudeAuthStatus)
	mux.HandleFunc("POST /sandboxes/{name}/claude/messages", s.requireVercel(s.claudeMessage))
	mux.HandleFunc("GET /github/auth", s.githubAuthStatus)
	mux.HandleFunc("GET /workspaces", s.requireVercel(s.listWorkspaces))
	mux.HandleFunc("POST /workspaces", s.requireVercel(s.createWorkspace))
	mux.HandleFunc("GET /workspaces/{name}", s.requireVercel(s.getWorkspace))
	mux.HandleFunc("POST /workspaces/{name}/claude/messages", s.requireVercel(s.workspaceClaudeMessage))
	mux.HandleFunc("POST /workspaces/{name}/pull-request", s.requireVercel(s.workspacePullRequest))
	mux.HandleFunc("DELETE /workspaces/{name}", s.requireVercel(s.destroySandbox))
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
// from non-loopback pages, and POSTs without a JSON content type (which would
// otherwise be "simple" cross-site requests sent without a CORS preflight).
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
		if r.Method == http.MethodPost {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" {
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
	if maxSeconds := int(vercel.MaxTimeout.Seconds()); req.TimeoutSeconds < 0 || req.TimeoutSeconds > maxSeconds {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeout_seconds must be between 0 and %d", maxSeconds))
		return
	}
	sb, status, err := s.provision(r.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// provision creates a sandbox and tracks it so it is destroyed on shutdown.
// On error it returns the HTTP status to report.
func (s *Server) provision(ctx context.Context, timeout time.Duration) (*vercel.Sandbox, int, error) {
	// Reserve a slot before calling Vercel so concurrent requests cannot
	// exceed the limit.
	s.mu.Lock()
	if len(s.sandboxes)+s.creating >= maxSandboxes {
		s.mu.Unlock()
		return nil, http.StatusTooManyRequests,
			fmt.Errorf("sandbox limit reached (%d); destroy one before creating another", maxSandboxes)
	}
	s.creating++
	s.mu.Unlock()

	name := vercel.NewSandboxName()
	sb, err := s.vercel.Create(ctx, name, vercel.CreateOptions{Timeout: timeout})
	s.mu.Lock()
	s.creating--
	if err != nil {
		s.mu.Unlock()
		log.Printf("create sandbox %s: %v", name, err)
		return nil, http.StatusBadGateway, err
	}
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

func (s *Server) destroySandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
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
	s.forget(sb.Name)
	log.Printf("destroyed sandbox %s", sb.Name)
	w.WriteHeader(http.StatusNoContent)
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
	return true
}

func (s *Server) release(name string) {
	s.mu.Lock()
	delete(s.busy, name)
	s.mu.Unlock()
}

func writeBusy(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "another request is still running in this sandbox; wait for it to finish")
}

func (s *Server) forget(name string) {
	s.mu.Lock()
	delete(s.sandboxes, name)
	delete(s.claudeInstalled, name)
	delete(s.workspaces, name)
	s.mu.Unlock()
}

// Shutdown destroys every sandbox this server created and has not yet
// destroyed. Call it before the process exits.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.vercel == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	pending := make([]vercel.Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		pending = append(pending, sb)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
