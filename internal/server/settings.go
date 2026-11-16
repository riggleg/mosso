package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
)

// settings is the body of GET and POST /settings: what can be configured
// from the web UI, plus the choices for it.
type settings struct {
	// DefaultModel is the Claude model new workspaces run unless created
	// with another.
	DefaultModel string `json:"default_model"`
	// Models are the models a workspace can run.
	Models []claude.Model `json:"models"`
	// DefaultImage is the Vercel Container Registry image new workspaces'
	// sandboxes boot from unless created with another; empty uses Mosso's
	// built-in runtime (BuiltInRuntime).
	DefaultImage string `json:"default_image"`
	// DefaultSetupScript is the Bash script run once in each new
	// workspace's sandbox, after the clone, unless the workspace is
	// created with another; empty runs nothing.
	DefaultSetupScript string `json:"default_setup_script"`
	// BuiltInRuntime is what a workspace without an image runs, for the
	// web UI to say so.
	BuiltInRuntime string `json:"built_in_runtime"`
}

func (s *Server) currentSettings() settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return settings{
		DefaultModel: s.defaultModel, Models: claude.Models,
		DefaultImage: s.defaultImage, DefaultSetupScript: s.defaultSetupScript,
		BuiltInRuntime: vercel.DefaultRuntime,
	}
}

// getSettings returns the settings. Like the other endpoints that only
// touch the database, it works without Vercel credentials.
func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.currentSettings())
}

// maxSettingsBytes bounds a POST /settings body: a setup script, JSON
// escaped, plus the rest.
const maxSettingsBytes = 2*maxSetupScriptBytes + 4096

// updateSettings changes the settings given in the body (the others stay
// as they are) and saves them in the database, all or nothing, so they
// survive restarts. It fails if they cannot be saved. Existing workspaces
// keep their model, image and setup script.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModel       *string `json:"default_model"`
		DefaultImage       *string `json:"default_image"`
		DefaultSetupScript *string `json:"default_setup_script"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	changes := map[string]string{}
	if req.DefaultModel != nil {
		if !claude.KnownModel(*req.DefaultModel) {
			writeError(w, http.StatusBadRequest, "default_model must be one of the models GET /settings lists")
			return
		}
		changes[settingDefaultModel] = *req.DefaultModel
	}
	if req.DefaultImage != nil {
		image, err := normalizeImage(*req.DefaultImage)
		if err != nil {
			writeError(w, http.StatusBadRequest, "default_image: "+err.Error())
			return
		}
		changes[settingDefaultImage] = image
	}
	if req.DefaultSetupScript != nil {
		script, err := normalizeSetupScript(*req.DefaultSetupScript)
		if err != nil {
			writeError(w, http.StatusBadRequest, "default_setup_script: "+err.Error())
			return
		}
		changes[settingDefaultSetupScript] = script
	}
	if len(changes) == 0 {
		writeError(w, http.StatusBadRequest, "set default_model, default_image or default_setup_script")
		return
	}
	s.mu.Lock()
	if s.store != nil {
		if err := s.store.setSettings(changes); err != nil {
			s.mu.Unlock()
			log.Printf("save the settings: %v", err)
			writeError(w, http.StatusInternalServerError, "the settings could not be saved: "+err.Error())
			return
		}
	}
	for key, v := range changes {
		switch key {
		case settingDefaultModel:
			s.defaultModel = v
			log.Printf("default model: %s", v)
		case settingDefaultImage:
			s.defaultImage = v
			log.Printf("default image: %s", imageLabel(v))
		case settingDefaultSetupScript:
			s.defaultSetupScript = v
			log.Printf("default setup script: %d bytes", len(v))
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.currentSettings())
}

// maxSetupScriptBytes bounds a setup script.
const maxSetupScriptBytes = 64 << 10

// normalizeImage trims an image reference and checks it; "" means no
// custom image.
func normalizeImage(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref != "" && !vercel.ValidImage(ref) {
		return "", errors.New(`not a Vercel Container Registry image reference such as "my-image:v1", "team/project/my-image@sha256:…" or "vercel/sandbox/universal"`)
	}
	return ref, nil
}

// normalizeSetupScript checks a setup script; one that is only blank
// space means no setup.
func normalizeSetupScript(script string) (string, error) {
	switch {
	case strings.TrimSpace(script) == "":
		return "", nil
	case len(script) > maxSetupScriptBytes:
		return "", fmt.Errorf("the script must be at most %d KiB", maxSetupScriptBytes>>10)
	case strings.ContainsRune(script, 0):
		return "", errors.New("the script must be text")
	}
	// Scripts pasted from Windows editors would fail on "\r".
	return strings.ReplaceAll(script, "\r\n", "\n"), nil
}

// imageLabel describes a workspace's image for logs and messages.
func imageLabel(image string) string {
	if image == "" {
		return "the built-in " + vercel.DefaultRuntime + " runtime"
	}
	return image
}
