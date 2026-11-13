package server

import (
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

// claudeMessage sends one prompt to Claude Code in a sandbox, installing the
// CLI first if needed, and returns the turn once Claude has finished.
func (s *Server) claudeMessage(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*claude.MaxPromptBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch {
	case strings.TrimSpace(req.Prompt) == "":
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	case len(req.Prompt) > claude.MaxPromptBytes:
		writeError(w, http.StatusBadRequest, "prompt is too long")
		return
	case req.SessionID != "" && !claude.ValidSessionID(req.SessionID):
		writeError(w, http.StatusBadRequest, "session_id must be a session ID returned by a previous message")
		return
	}

	token, _, err := s.claudeToken(r.Context())
	if err != nil {
		if errors.Is(err, claude.ErrNoToken) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			log.Printf("load claude token: %v", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// One turn at a time per sandbox: concurrent turns could resume the same
	// session and interleave its transcript.
	s.mu.Lock()
	if s.claudeBusy[sb.Name] {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "Claude is already working in this sandbox; wait for the current message to finish")
		return
	}
	s.claudeBusy[sb.Name] = true
	installed := s.claudeInstalled[sb.Name]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.claudeBusy, sb.Name)
		s.mu.Unlock()
	}()

	if !installed {
		version, err := claude.Install(r.Context(), s.vercel, sb.SessionID)
		if err != nil {
			log.Printf("install claude code in sandbox %s: %v", sb.Name, err)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.mu.Lock()
		if _, live := s.sandboxes[sb.Name]; live {
			s.claudeInstalled[sb.Name] = true
		}
		s.mu.Unlock()
		log.Printf("installed %s in sandbox %s", version, sb.Name)
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
