package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
)

// A section is a named group of workspaces, listed together in the
// sidebar, with the configuration new workspaces in it start from: a
// repository and base branch, a model, a sandbox environment and agent
// instructions. A workspace copies that configuration when it is created
// (with any changes made in the New workspace dialog), so editing or
// deleting the section later never changes an existing workspace; moving
// a workspace to another section only changes where it is listed.
type section struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Repo is "owner/repo", or empty to choose one for each workspace.
	Repo string `json:"repo"`
	// Base is the branch pull requests target; empty for the
	// repository's default branch.
	Base string `json:"base"`
	// Model is the Claude model; empty for the default model.
	Model string `json:"model"`
	// Image and SetupScript are the sandbox environment; empty for the
	// built-in runtime and no setup script.
	Image       string `json:"image"`
	SetupScript string `json:"setup_script"`
	// Instructions are added to the agent's system prompt on every turn;
	// see workspacePrompt.
	Instructions string    `json:"instructions"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// errNoSection is a section ID that does not exist.
var errNoSection = errors.New("no such section")

const (
	// maxSectionName bounds a section's name, in characters.
	maxSectionName = 80
	// maxInstructionsBytes bounds agent instructions. They reach Claude
	// Code as one command-line argument (--append-system-prompt), which
	// Linux limits to 128 KiB together with Mosso's own instructions.
	maxInstructionsBytes = 16 << 10
	// maxSectionBytes bounds a section request body: a setup script and
	// instructions, JSON escaped, plus the rest.
	maxSectionBytes = 2*(maxSetupScriptBytes+maxInstructionsBytes) + 4096
)

// normalizeInstructions checks agent instructions; ones that are only
// blank space mean none.
func normalizeInstructions(text string) (string, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	switch {
	case len(text) > maxInstructionsBytes:
		return "", fmt.Errorf("they must be at most %d KiB", maxInstructionsBytes>>10)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return "", errors.New("they must be text")
	}
	return text, nil
}

// normalizeSectionName trims a section name and checks it.
func normalizeSectionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errors.New("name is required")
	case utf8.RuneCountInString(name) > maxSectionName:
		return "", fmt.Errorf("name must be at most %d characters", maxSectionName)
	case strings.IndexFunc(name, unicode.IsControl) >= 0 || !utf8.ValidString(name):
		return "", errors.New("name must be a single line of text")
	}
	return name, nil
}

// sectionRequest is the body of POST /sections and PATCH /sections/{id}:
// the fields to set. Absent fields are left as they are (empty for a new
// section).
type sectionRequest struct {
	Name         *string `json:"name"`
	Repo         *string `json:"repo"`
	Base         *string `json:"base"`
	Model        *string `json:"model"`
	Image        *string `json:"image"`
	SetupScript  *string `json:"setup_script"`
	Instructions *string `json:"instructions"`
}

// apply checks the request's fields and sets them on sec. It changes
// nothing if any is invalid.
func (req sectionRequest) apply(sec *section) error {
	out := *sec
	var err error
	if req.Name != nil {
		if out.Name, err = normalizeSectionName(*req.Name); err != nil {
			return err
		}
	}
	if req.Repo != nil {
		out.Repo = strings.TrimSpace(*req.Repo)
		if out.Repo != "" && !github.ValidRepo(out.Repo) {
			return errors.New(`repo must be a GitHub repository as "owner/repo", or empty`)
		}
	}
	if req.Base != nil {
		out.Base = strings.TrimSpace(*req.Base)
		if out.Base != "" && !github.ValidBranch(out.Base) {
			return errors.New("base is not a valid branch name")
		}
	}
	if req.Model != nil {
		out.Model = *req.Model
		if out.Model != "" && !claude.KnownModel(out.Model) {
			return errors.New("model must be one of the models GET /settings lists, or empty for the default model")
		}
	}
	if req.Image != nil {
		if out.Image, err = normalizeImage(*req.Image); err != nil {
			return fmt.Errorf("image: %w", err)
		}
	}
	if req.SetupScript != nil {
		if out.SetupScript, err = normalizeSetupScript(*req.SetupScript); err != nil {
			return fmt.Errorf("setup_script: %w", err)
		}
	}
	if req.Instructions != nil {
		if out.Instructions, err = normalizeInstructions(*req.Instructions); err != nil {
			return fmt.Errorf("instructions: %w", err)
		}
	}
	*sec = out
	return nil
}

// decodeSection reads a section request or writes an error.
func decodeSection(w http.ResponseWriter, r *http.Request) (sectionRequest, bool) {
	var req sectionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSectionBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return req, false
	}
	return req, true
}

// sectionStoreLocked returns the database, or writes an error and returns nil:
// sections only exist in the database. Call with s.mu held.
func (s *Server) sectionStoreLocked(w http.ResponseWriter) *store {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "sections need Mosso's database, which is not open")
	}
	return s.store
}

// sectionID reads the {id} path value or writes an error.
func sectionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "unknown section")
		return 0, false
	}
	return id, true
}

// writeSectionError reports a failed section change.
func writeSectionError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, errNoSection) {
		writeError(w, http.StatusNotFound, "unknown section")
		return
	}
	log.Printf("%s: %v", op, err)
	writeError(w, http.StatusInternalServerError, op+": "+err.Error())
}

// listSections returns every section, by name. Like the other endpoints
// that only touch the database, the section endpoints work without Vercel
// credentials.
func (s *Server) listSections(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	var list []section
	var err error
	if s.store == nil {
		list = []section{}
	} else {
		list, err = s.store.sections()
	}
	s.mu.Unlock()
	if err != nil {
		writeSectionError(w, "list the sections", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": list})
}

// createSection saves a new section. Its name is required and must not be
// another section's (ignoring case).
func (s *Server) createSection(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeSection(w, r)
	if !ok {
		return
	}
	var sec section
	if req.Name == nil {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := req.apply(&sec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.saveSection(w, sec, http.StatusCreated)
}

// updateSection changes the fields given in the body, for example only
// the name to rename it. Existing workspaces are not affected.
func (s *Server) updateSection(w http.ResponseWriter, r *http.Request) {
	id, ok := sectionID(w, r)
	if !ok {
		return
	}
	req, ok := decodeSection(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sectionStoreLocked(w)
	if st == nil {
		s.mu.Unlock()
		return
	}
	sec, err := st.section(id)
	s.mu.Unlock()
	if err != nil {
		writeSectionError(w, "read the section", err)
		return
	}
	if err := req.apply(&sec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.saveSection(w, sec, http.StatusOK)
}

// saveSection saves sec, refusing a name another section has, and writes
// the saved section with status.
func (s *Server) saveSection(w http.ResponseWriter, sec section, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sectionStoreLocked(w)
	if st == nil {
		return
	}
	taken, err := st.sectionNameTaken(sec.Name, sec.ID)
	if err == nil && taken {
		writeError(w, http.StatusConflict, fmt.Sprintf("there is already a section named %q", sec.Name))
		return
	}
	if err == nil {
		sec, err = st.saveSection(sec)
	}
	if err != nil {
		writeSectionError(w, "save the section", err)
		return
	}
	log.Printf("section %d saved: %q", sec.ID, sec.Name)
	writeJSON(w, status, sec)
}

// duplicateSection saves a copy of a section's configuration under a new
// name: the one in the body ({"name": "..."}, optional), or "<name> copy".
// The copy has no workspaces.
func (s *Server) duplicateSection(w http.ResponseWriter, r *http.Request) {
	id, ok := sectionID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name *string `json:"name"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	s.mu.Lock()
	st := s.sectionStoreLocked(w)
	if st == nil {
		s.mu.Unlock()
		return
	}
	sec, err := st.section(id)
	if err == nil && req.Name == nil {
		var name string
		name, err = copyName(st, sec.Name)
		req.Name = &name
	}
	s.mu.Unlock()
	if err != nil {
		writeSectionError(w, "read the section", err)
		return
	}
	sec.ID = 0
	if err := (sectionRequest{Name: req.Name}).apply(&sec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.saveSection(w, sec, http.StatusCreated)
}

// copyName returns the first free name of "<name> copy", "<name> copy 2",
// … Call with s.mu held.
func copyName(st *store, name string) (string, error) {
	// Leave room for the suffix within maxSectionName.
	if r := []rune(name); len(r) > maxSectionName-10 {
		name = strings.TrimSpace(string(r[:maxSectionName-10]))
	}
	for i := 1; ; i++ {
		candidate := name + " copy"
		if i > 1 {
			candidate += " " + strconv.Itoa(i)
		}
		taken, err := st.sectionNameTaken(candidate, 0)
		if err != nil || !taken {
			return candidate, err
		}
	}
}

// deleteSection deletes a section. Its workspaces, including the history,
// stay as they are, unsectioned; if it was the default section, there is
// no default any more.
func (s *Server) deleteSection(w http.ResponseWriter, r *http.Request) {
	id, ok := sectionID(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sectionStoreLocked(w)
	if st == nil {
		return
	}
	if err := st.deleteSection(id); err != nil {
		writeSectionError(w, "delete the section", err)
		return
	}
	// The database cleared these already; keep memory in step, including
	// workspaces still being set up, which are saved later.
	for _, ws := range s.workspaces {
		if ws.SectionID == id {
			ws.SectionID = 0
			ws.chat.notify()
		}
	}
	if s.defaultSection == id {
		s.defaultSection = 0
	}
	log.Printf("section %d deleted", id)
	w.WriteHeader(http.StatusNoContent)
}

// moveWorkspace lists a workspace under another section
// ({"section_id": N}) or under none ({"section_id": 0}). Only that changes:
// the workspace keeps its configuration, sandbox, instructions and
// conversation, whether it is running, in the history or archived. It
// fails if the change cannot be saved.
func (s *Server) moveWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SectionID *int64 `json:"section_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SectionID == nil || *req.SectionID < 0 {
		writeError(w, http.StatusBadRequest, "section_id is required: a section's ID, or 0 for no section")
		return
	}
	id := *req.SectionID
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[r.PathValue("name")]
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	case ws.creating:
		writeError(w, http.StatusConflict, "the workspace is still being set up")
		return
	}
	if id != 0 {
		st := s.sectionStoreLocked(w)
		if st == nil {
			return
		}
		if _, err := st.section(id); err != nil {
			if errors.Is(err, errNoSection) {
				writeError(w, http.StatusBadRequest, "section_id: no such section")
				return
			}
			writeSectionError(w, "read the section", err)
			return
		}
	}
	before, unsaved := ws.SectionID, ws.unsaved
	ws.SectionID = id
	if err := s.saveLocked(ws); err != nil {
		ws.SectionID, ws.unsaved = before, unsaved
		log.Printf("workspace %s: save: %v", ws.Name, err)
		writeError(w, http.StatusInternalServerError, "the change could not be saved: "+err.Error())
		return
	}
	ws.chat.notify()
	writeJSON(w, http.StatusOK, s.snapshotLocked(ws))
}

// formatSectionID is how the default section is kept in the settings.
func formatSectionID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
