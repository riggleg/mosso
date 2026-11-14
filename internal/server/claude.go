package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/riggleg/mosso/internal/claude"
)

// claudeAuthStatus reports whether a Claude Code token is available and where
// it comes from. It never returns the token.
func (s *Server) claudeAuthStatus(w http.ResponseWriter, r *http.Request) {
	_, source, err := s.claudeToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "source": source})
}

// messageRequest is the body of the Claude message endpoints.
type messageRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	// Attachments are IDs of files uploaded to the workspace.
	Attachments []string `json:"attachments"`
}

// decodeMessage reads and validates a message request or writes an error.
func decodeMessage(w http.ResponseWriter, r *http.Request) (messageRequest, bool) {
	var req messageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*claude.MaxPromptBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return req, false
	}
	switch {
	case strings.TrimSpace(req.Prompt) == "" && len(req.Attachments) == 0:
		writeError(w, http.StatusBadRequest, "prompt is required")
		return req, false
	case len(req.Attachments) > maxMessageFiles:
		writeError(w, http.StatusBadRequest, "too many attachments")
		return req, false
	case len(req.Prompt) > claude.MaxPromptBytes-promptReserve:
		writeError(w, http.StatusBadRequest, "prompt is too long")
		return req, false
	case req.SessionID != "" && !claude.ValidSessionID(req.SessionID):
		writeError(w, http.StatusBadRequest, "session_id must be a session ID returned by a previous message")
		return req, false
	}
	return req, true
}

// loadClaudeToken returns the Claude Code token or writes an error.
func (s *Server) loadClaudeToken(w http.ResponseWriter, r *http.Request) (claude.Token, bool) {
	token, _, err := s.claudeToken(r.Context())
	if err != nil {
		if errors.Is(err, claude.ErrNoToken) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			log.Printf("load claude token: %v", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return "", false
	}
	return token, true
}

// ensureClaude installs Claude Code in the sandbox unless Mosso already has.
func (s *Server) ensureClaude(ctx context.Context, name, sandboxSession string) error {
	s.mu.Lock()
	installed := s.claudeInstalled[name]
	s.mu.Unlock()
	if installed {
		return nil
	}
	version, err := claude.Install(ctx, s.vercel, sandboxSession)
	if err != nil {
		log.Printf("install claude code in sandbox %s: %v", name, err)
		return err
	}
	s.mu.Lock()
	if _, live := s.sandboxes[name]; live {
		s.claudeInstalled[name] = true
	}
	s.mu.Unlock()
	log.Printf("installed %s in sandbox %s", version, name)
	return nil
}

// claudeMessage sends one prompt to Claude Code in a sandbox, installing the
// CLI first if needed, and returns the turn once Claude has finished. If the
// client disconnects, the turn is stopped.
func (s *Server) claudeMessage(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	req, ok := decodeMessage(w, r)
	if !ok {
		return
	}
	token, ok := s.loadClaudeToken(w, r)
	if !ok {
		return
	}
	if len(req.Attachments) > 0 {
		writeError(w, http.StatusBadRequest, "attachments are only supported in workspaces")
		return
	}
	// One turn at a time per sandbox: concurrent turns could resume the same
	// session and interleave its transcript.
	if !s.acquire(sb.Name) {
		writeBusy(w)
		return
	}
	defer s.release(sb.Name)
	if err := s.ensureClaude(r.Context(), sb.Name, sb.SessionID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	turn, err := claude.Send(r.Context(), s.vercel, sb.SessionID, token, claude.Request{
		Prompt:    req.Prompt,
		SessionID: req.SessionID,
	})
	if err != nil {
		// Prompt text is deliberately not logged.
		log.Printf("claude in sandbox %s: %v", sb.Name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("claude in sandbox %s: session %s, %d events, is_error=%t", sb.Name, turn.SessionID, len(turn.Events), turn.IsError)
	writeJSON(w, http.StatusOK, turn)
}

// workspaceClaudeMessage runs a Claude Code turn in the workspace checkout
// and waits for it, like claudeMessage. The turn is recorded in the
// workspace's chat, so the web UI shows it too. The GitHub token is not
// available to Claude. If the client disconnects, the turn is stopped.
func (s *Server) workspaceClaudeMessage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.lookupWorkspace(w, r)
	if !ok {
		return
	}
	req, ok := decodeMessage(w, r)
	if !ok {
		return
	}
	token, ok := s.loadClaudeToken(w, r)
	if !ok {
		return
	}
	run, ok := s.startTurnWithAttachments(w, ws.Name, token, req, &req.SessionID)
	if !ok {
		return
	}
	select {
	case <-run.done:
	case <-r.Context().Done():
		s.mu.Lock()
		run.stopped = true
		taskID, session := run.taskID, ws.Sandbox.SessionID
		if live, ok := s.workspaces[ws.Name]; ok {
			session = live.Sandbox.SessionID
		}
		s.mu.Unlock()
		if taskID != "" {
			s.stopTask(session)
		}
		<-run.done
		return
	}
	s.mu.Lock()
	turn, err := run.turn, run.err
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if turn == nil {
		turn = &claude.Turn{Events: []claude.Event{}}
	}
	writeJSON(w, http.StatusOK, turn)
}
