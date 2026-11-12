// Package server contains Mosso's HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/riggleg/mosso/internal/vercel"
)

// maxCommandBytes bounds the size of a command submitted to /exec.
const maxCommandBytes = 64 * 1024

// Server serves Mosso's HTTP API and tracks the sandboxes it has created so
// they can be destroyed on shutdown.
type Server struct {
	vercel *vercel.Client // nil when credentials are not configured

	mu        sync.Mutex
	sandboxes map[string]vercel.Sandbox
}

// New returns a Server. client may be nil, in which case sandbox endpoints
// respond with 503 Service Unavailable.
func New(client *vercel.Client) *Server {
	return &Server{vercel: client, sandboxes: map[string]vercel.Sandbox{}}
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
	return mux
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
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 5*60*60 {
		writeError(w, http.StatusBadRequest, "timeout_seconds must be between 0 and 18000")
		return
	}
	name := vercel.NewSandboxName()
	sb, err := s.vercel.Create(r.Context(), name, vercel.CreateOptions{
		Timeout: time.Duration(req.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		log.Printf("create sandbox %s: %v", name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	s.sandboxes[sb.Name] = *sb
	s.mu.Unlock()
	log.Printf("created sandbox %s (session %s)", sb.Name, sb.SessionID)
	writeJSON(w, http.StatusCreated, sb)
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

func (s *Server) forget(name string) {
	s.mu.Lock()
	delete(s.sandboxes, name)
	s.mu.Unlock()
}

// Shutdown destroys every sandbox this server created and has not yet
// destroyed. Call it before the process exits.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.vercel == nil {
		return nil
	}
	s.mu.Lock()
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
