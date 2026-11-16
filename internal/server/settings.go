package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/riggleg/mosso/internal/claude"
)

// settings is the body of GET and POST /settings: what can be configured
// from the web UI, plus the choices for it.
type settings struct {
	// DefaultModel is the Claude model new workspaces run unless created
	// with another.
	DefaultModel string `json:"default_model"`
	// Models are the models a workspace can run.
	Models []claude.Model `json:"models"`
}

func (s *Server) currentSettings() settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return settings{DefaultModel: s.defaultModel, Models: claude.Models}
}

// getSettings returns the settings. Like the other endpoints that only
// touch the database, it works without Vercel credentials.
func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.currentSettings())
}

// updateSettings changes the default model and saves it in the database,
// so it survives restarts. It fails if it cannot be saved. Existing
// workspaces keep their model.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !claude.KnownModel(req.DefaultModel) {
		writeError(w, http.StatusBadRequest, "default_model must be one of the models GET /settings lists")
		return
	}
	s.mu.Lock()
	if s.store != nil {
		if err := s.store.setSetting(settingDefaultModel, req.DefaultModel); err != nil {
			s.mu.Unlock()
			log.Printf("save the default model: %v", err)
			writeError(w, http.StatusInternalServerError, "the default model could not be saved: "+err.Error())
			return
		}
	}
	s.defaultModel = req.DefaultModel
	s.mu.Unlock()
	log.Printf("default model: %s", req.DefaultModel)
	writeJSON(w, http.StatusOK, s.currentSettings())
}
