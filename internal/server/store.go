package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

// EnvDataDir names the directory Mosso keeps its state in; see DataDir.
const EnvDataDir = "MOSSO_DATA_DIR"

// DataDir is where Mosso saves workspaces: $MOSSO_DATA_DIR, or "mosso" in
// the user's configuration directory (~/Library/Application Support on
// macOS, $XDG_CONFIG_HOME or ~/.config elsewhere).
func DataDir() (string, error) {
	if v := os.Getenv(EnvDataDir); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the data directory: %w (set %s)", err, EnvDataDir)
	}
	return filepath.Join(base, "mosso"), nil
}

// workspaceRecord is what Mosso remembers about a workspace between runs:
// enough to find its sandbox again and continue where it left off. The
// chat is stored next to it, one JSON line per entry. No credentials.
type workspaceRecord struct {
	Name    string         `json:"name"`
	Repo    string         `json:"repo"`
	Base    string         `json:"base"`
	BaseSHA string         `json:"base_sha"`
	Branch  string         `json:"branch"`
	Dir     string         `json:"dir"`
	Sandbox vercel.Sandbox `json:"sandbox"`
	// SandboxState is "running", "stopped" or "gone", as last known.
	SandboxState string              `json:"sandbox_state"`
	ExpiresAt    time.Time           `json:"expires_at,omitzero"`
	PullRequest  *github.PullRequest `json:"pull_request,omitempty"`
	// Pushed is the working branch head Mosso last pushed, so a rebased
	// branch can still be force-pushed after a restart.
	Pushed string `json:"pushed,omitempty"`
	// SessionID is the Claude Code session follow-ups resume.
	SessionID string `json:"claude_session_id,omitempty"`
	// LastSeq is the last worker event applied to the chat; TaskRunning
	// says whether a Claude task was in flight, so a restart knows it
	// may have missed events.
	LastSeq     int64        `json:"last_seq"`
	TaskRunning bool         `json:"task_running"`
	Attachments []attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

var workspaceNamePattern = regexp.MustCompile(`^mosso-[0-9a-f]{12}$`)

// store keeps workspace records and chat logs under a directory:
//
//	<dir>/workspaces/<name>/workspace.json
//	<dir>/workspaces/<name>/chat.jsonl
type store struct {
	dir string
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory: %w", err)
	}
	return &store{dir: dir}, nil
}

func (st *store) workspaceDir(name string) string {
	return filepath.Join(st.dir, "workspaces", name)
}

// save writes the record atomically.
func (st *store) save(rec workspaceRecord) error {
	if !workspaceNamePattern.MatchString(rec.Name) {
		return fmt.Errorf("store: invalid workspace name %q", rec.Name)
	}
	dir := st.workspaceDir(rec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "workspace.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "workspace.json"))
}

// load returns every saved workspace, oldest first.
func (st *store) load() ([]workspaceRecord, error) {
	entries, err := os.ReadDir(filepath.Join(st.dir, "workspaces"))
	if err != nil {
		return nil, err
	}
	var out []workspaceRecord
	for _, e := range entries {
		if !e.IsDir() || !workspaceNamePattern.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(st.workspaceDir(e.Name()), "workspace.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec workspaceRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("read workspace %s: %w", e.Name(), err)
		}
		if rec.Name != e.Name() {
			return nil, fmt.Errorf("read workspace %s: the record is for %q", e.Name(), rec.Name)
		}
		out = append(out, rec)
	}
	return out, nil
}

// appendEntry adds one chat entry to the workspace's log.
func (st *store) appendEntry(name string, e chatEntry) error {
	if !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("store: invalid workspace name %q", name)
	}
	f, err := os.OpenFile(filepath.Join(st.workspaceDir(name), "chat.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// loadEntries returns the workspace's chat log. A truncated last line is
// skipped.
func (st *store) loadEntries(name string) ([]chatEntry, error) {
	f, err := os.Open(filepath.Join(st.workspaceDir(name), "chat.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []chatEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var e chatEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		e.ID = len(out)
		out = append(out, e)
	}
	return out, sc.Err()
}

// remove deletes everything saved about the workspace.
func (st *store) remove(name string) error {
	if !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("store: invalid workspace name %q", name)
	}
	return os.RemoveAll(st.workspaceDir(name))
}
