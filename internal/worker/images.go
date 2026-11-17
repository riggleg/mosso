package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/claude"
)

// Images Claude's activity shows (a screenshot a tool returned, a chart a
// script saved, an image its reply refers to) are copied into the image
// store, ImagesDir under the state directory, named by their content: the
// event log only records their ID, type and where they came from. The
// Mosso server reads them from there to show them. Copies keep what Claude
// showed even if the original file changes or is deleted, and they live
// outside the repository, so nothing is added to Git.

// ImagesDir is the image store in the sandbox.
const ImagesDir = DefaultStateDir + "/images"

const (
	// MaxImageBytes caps an image the worker stores.
	MaxImageBytes = claude.MaxImageBytes
	// maxTaskImages caps the images recorded for one task, and
	// maxEventImages those added to one event.
	maxTaskImages  = 40
	maxEventImages = 10
	// maxScanEntries bounds the files one scan for new images visits.
	maxScanEntries = 20000
)

// imageExts are the file names a scan for new images looks at; the
// content decides whether a file is an image.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true}

// skipDirs are directories a scan for new images does not enter: they hold
// dependencies and build output, not what Claude made for the user.
// Hidden directories (.git, .next, …) are skipped too.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "__pycache__": true, "venv": true, "target": true}

// ImageID returns the ID of an image: the first 32 hex digits of the
// SHA-256 of its content.
func ImageID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidImageID reports whether id looks like an ImageID.
func ValidImageID(id string) bool { return imageIDPattern.MatchString(id) }

// ImageType returns the MIME type of an image Mosso shows, judged by its
// content: PNG, JPEG, GIF, WebP, or SVG (an XML document whose root
// element is svg). It returns "" for anything else.
func ImageType(data []byte) string {
	if t := claude.RasterType(data); t != "" {
		return t
	}
	if isSVG(data) {
		return "image/svg+xml"
	}
	return ""
}

func isSVG(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) || !bytes.Contains(data, []byte("<svg")) {
		return false
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	for i := 0; i < 1000; i++ {
		tok, err := d.Token()
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t.Name.Local == "svg"
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return false
			}
		}
	}
	return false
}

// imageStore keeps images in a directory, by ID.
type imageStore struct {
	dir string
	// roots are the directories image files may be read from (after
	// resolving symlinks); state is the worker's state directory, of which
	// only attachments/ may be read.
	roots []string
	state string
	// tmp is a directory whose files (not subdirectories) are scanned for
	// new images too, "" for none.
	tmp string
}

// put stores data and returns its ID. Storing an image twice keeps one
// copy.
func (st *imageStore) put(data []byte) (string, error) {
	id := ImageID(data)
	p := filepath.Join(st.dir, id)
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() == int64(len(data)) {
		return id, nil
	}
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(st.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return id, os.Rename(tmp.Name(), p)
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

var errNotImage = errors.New("not an image Mosso shows")

// readImageFile reads an image file Claude made or referred to. The path
// must resolve, symlinks included, to a regular file under one of the
// store's roots (and not in the worker's own state, except attachments),
// of at most MaxImageBytes, whose content is a PNG, JPEG, GIF, WebP or
// SVG image.
func (st *imageStore) readImageFile(p string) ([]byte, string, error) {
	if !filepath.IsAbs(p) {
		return nil, "", errors.New("not an absolute path")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return nil, "", err
	}
	allowed := false
	for _, root := range st.roots {
		if within(real, root) {
			allowed = true
			break
		}
	}
	if st.state != "" && within(real, st.state) && !within(real, filepath.Join(st.state, "attachments")) {
		allowed = false
	}
	if !allowed {
		return nil, "", errors.New("outside the directories images are shown from")
	}
	// O_NOFOLLOW: a symlink swapped in since it was resolved is refused.
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !fi.Mode().IsRegular() {
		return nil, "", errors.New("not a regular file")
	}
	if fi.Size() > MaxImageBytes {
		return nil, "", errors.New("too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > MaxImageBytes {
		return nil, "", errors.New("empty or too large")
	}
	typ := ImageType(data)
	if typ == "" {
		return nil, "", errNotImage
	}
	return data, typ, nil
}

// storeFile copies an image file into the store and describes it.
func (st *imageStore) storeFile(p string) (claude.Image, error) {
	data, typ, err := st.readImageFile(p)
	if err != nil {
		return claude.Image{}, err
	}
	id, err := st.put(data)
	if err != nil {
		return claude.Image{}, err
	}
	return claude.Image{ID: id, Type: typ, Size: int64(len(data)), Name: filepath.Base(p), Path: filepath.Clean(p)}, nil
}

// taskImages adds images to one task's events as they are recorded: it
// stores the images the events carry, and finds the image files Claude's
// tools wrote and the images its text refers to.
type taskImages struct {
	st    *imageStore
	dir   string // the task's working directory
	since time.Time
	// seen are the files already shown, by path, with their modification
	// time; shown are the image IDs already recorded in this task.
	seen  map[string]time.Time
	shown map[string]bool
	count int
}

func (st *imageStore) task(dir string, started time.Time) *taskImages {
	return &taskImages{st: st, dir: dir, since: started, seen: map[string]time.Time{}, shown: map[string]bool{}}
}

// add stores the event's images and adds the image files and Markdown
// images it leads to. Images that cannot be stored are left out; the
// event never carries image bytes afterwards.
func (t *taskImages) add(e *claude.Event) {
	var out []claude.Image
	for _, img := range e.Images {
		if img.ID == "" && img.Data != nil && t.count < maxTaskImages {
			if id, err := t.st.put(img.Data); err == nil {
				img.ID = id
			}
		}
		img.Data = nil
		if img.ID != "" && len(out) < maxEventImages {
			out = append(out, img)
			t.shown[img.ID] = true
			t.count++
		}
	}
	switch e.Type {
	case "text":
		out = append(out, t.markdownImages(e.Text)...)
	case "tool_result":
		out = append(out, t.newFiles(maxEventImages-len(out))...)
	}
	e.Images = out
}

// markdownImage matches ![alt](reference) and ![alt](<reference>), with an
// optional title.
var markdownImage = regexp.MustCompile(`!\[[^\]\n]*\]\(\s*(<[^>\n]+>|[^)\s]+)(?:\s+"[^"\n]*")?\s*\)`)

// markdownImages stores the local image files text refers to with
// Markdown image syntax. Remote and data: references are left alone; the
// web UI does not load them.
func (t *taskImages) markdownImages(text string) []claude.Image {
	var out []claude.Image
	done := map[string]bool{}
	for _, m := range markdownImage.FindAllStringSubmatch(text, -1) {
		ref := strings.TrimSuffix(strings.TrimPrefix(m[1], "<"), ">")
		if done[ref] || len(out) == maxEventImages || t.count >= maxTaskImages {
			continue
		}
		done[ref] = true
		p := strings.TrimPrefix(ref, "file://")
		if p != ref && !strings.HasPrefix(p, "/") {
			continue // file://host/…
		}
		if strings.Contains(p, "://") || strings.HasPrefix(strings.ToLower(p), "data:") || strings.HasPrefix(p, "//") {
			continue
		}
		p, _, _ = strings.Cut(p, "#")
		p, _, _ = strings.Cut(p, "?")
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(t.dir, p)
		}
		img, err := t.st.storeFile(p)
		if err != nil {
			continue
		}
		img.Ref = ref
		out = append(out, img)
		t.count++
	}
	return out
}

// newFiles finds the image files created or changed since the last look:
// anywhere in the working directory except hidden and dependency
// directories, and directly in the temporary directory. Each new version
// of a file is shown once; one already shown in the task (the same
// content) is not shown again.
func (t *taskImages) newFiles(limit int) []claude.Image {
	if limit <= 0 || t.count >= maxTaskImages {
		return nil
	}
	// A little slack for coarse file timestamps; seen avoids repeats.
	since := t.since.Add(-2 * time.Second)
	t.since = time.Now()
	var found []string
	visited := 0
	check := func(p string, d fs.DirEntry) {
		if !d.Type().IsRegular() || !imageExts[strings.ToLower(filepath.Ext(p))] {
			return
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().After(since) || fi.Size() > MaxImageBytes {
			return
		}
		if prev, ok := t.seen[p]; ok && !fi.ModTime().After(prev) {
			return
		}
		t.seen[p] = fi.ModTime()
		found = append(found, p)
	}
	_ = filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if visited++; visited > maxScanEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != t.dir && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		check(p, d)
		return nil
	})
	if tmp := t.st.tmp; tmp != "" && !within(t.dir, tmp) {
		if entries, err := os.ReadDir(tmp); err == nil {
			for _, d := range entries {
				check(filepath.Join(tmp, d.Name()), d)
			}
		}
	}
	var out []claude.Image
	for _, p := range found {
		if len(out) == limit || t.count >= maxTaskImages {
			break
		}
		img, err := t.st.storeFile(p)
		if err != nil || t.shown[img.ID] {
			continue
		}
		t.shown[img.ID] = true
		out = append(out, img)
		t.count++
	}
	return out
}
