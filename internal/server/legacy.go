package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var workspaceNamePattern = regexp.MustCompile(`^mosso-[0-9a-f]{12}$`)

// importFiles copies the workspaces an earlier Mosso saved as files into
// the database, so their sandboxes are not forgotten (and left to bill) by
// the move to SQLite:
//
//	<dir>/<name>/workspace.json   the record
//	<dir>/<name>/chat.jsonl       one chat entry per line
//
// Workspaces already in the database are skipped. Once everything is in,
// dir is renamed to <dir>.imported, which Mosso never reads again and can
// be deleted. It returns how many workspaces it imported.
func (st *store) importFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !workspaceNamePattern.MatchString(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name, "workspace.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return imported, err
		}
		var rec workspaceRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return imported, fmt.Errorf("read workspace %s: %w", name, err)
		}
		if rec.Name != name {
			return imported, fmt.Errorf("read workspace %s: the record is for %q", name, rec.Name)
		}
		if saved, err := st.exists(name); err != nil {
			return imported, err
		} else if saved {
			continue
		}
		chat, err := readChatFile(filepath.Join(dir, name, "chat.jsonl"))
		if err != nil {
			return imported, fmt.Errorf("read the chat of workspace %s: %w", name, err)
		}
		if err := st.save(rec, chat...); err != nil {
			return imported, fmt.Errorf("import workspace %s: %w", name, err)
		}
		imported++
	}
	return imported, os.Rename(dir, dir+".imported")
}

// readChatFile reads a chat.jsonl. A truncated last line is skipped.
func readChatFile(path string) ([]chatEntry, error) {
	f, err := os.Open(path)
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
