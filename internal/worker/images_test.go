package worker

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// testPNG returns a small PNG of one color.
func testPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const testSVG = `<?xml version="1.0"?>
<!-- a chart -->
<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>`

func TestImageType(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want string
	}{
		"png":           {testPNG(t, color.White), "image/png"},
		"jpeg":          {[]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00rest"), "image/jpeg"},
		"gif":           {[]byte("GIF89a\x01\x00\x01\x00"), "image/gif"},
		"webp":          {[]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), "image/webp"},
		"svg":           {[]byte(testSVG), "image/svg+xml"},
		"svg with BOM":  {[]byte("\xef\xbb\xbf<svg></svg>"), "image/svg+xml"},
		"svg script":    {[]byte(`<svg><script>alert(1)</script></svg>`), "image/svg+xml"}, // served sandboxed
		"html":          {[]byte(`<html><body><svg></svg></body></html>`), ""},
		"text with svg": {[]byte(`see <svg> here`), ""},
		"text":          {[]byte("CLAUDE_CODE_OAUTH_TOKEN=secret"), ""},
		"pdf":           {[]byte("%PDF-1.7\n"), ""},
		"empty":         {nil, ""},
	}
	for name, c := range cases {
		if got := ImageType(c.data); got != c.want {
			t.Errorf("%s: ImageType = %q, want %q", name, got, c.want)
		}
	}
}

// newTestStore returns a store whose files may come from root.
func newTestStore(t *testing.T) (*imageStore, string, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, ".mosso")
	st := &imageStore{dir: filepath.Join(state, "images"), state: state, roots: []string{root}}
	return st, root, state
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStoreFileConfinesReads(t *testing.T) {
	st, root, state := newTestStore(t)
	outside := t.TempDir()
	pngData := testPNG(t, color.White)
	write(t, filepath.Join(root, "repo", "shot.png"), pngData)
	write(t, filepath.Join(outside, "secret.png"), pngData)
	write(t, filepath.Join(state, "events.jsonl"), pngData)
	write(t, filepath.Join(state, "attachments", "a1", "up.png"), pngData)
	write(t, filepath.Join(root, "repo", "notes.png"), []byte("not an image"))
	write(t, filepath.Join(root, "repo", "big.png"), append(bytes.Clone(pngData), make([]byte, MaxImageBytes)...))
	must(t, os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(root, "repo", "escape.png")))
	must(t, os.Symlink(filepath.Join(root, "repo", "shot.png"), filepath.Join(root, "repo", "link.png")))
	must(t, os.Symlink(outside, filepath.Join(root, "repo", "outdir")))
	must(t, syscall.Mkfifo(filepath.Join(root, "repo", "fifo.png"), 0o644))

	ok := []string{"repo/shot.png", "repo/link.png", ".mosso/attachments/a1/up.png"}
	for _, p := range ok {
		img, err := st.storeFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if img.ID != ImageID(pngData) || img.Type != "image/png" || img.Size != int64(len(pngData)) || img.Name != filepath.Base(p) {
			t.Fatalf("%s: %+v", p, img)
		}
		got, err := os.ReadFile(filepath.Join(st.dir, img.ID))
		if err != nil || !bytes.Equal(got, pngData) {
			t.Fatalf("%s: stored copy: %v", p, err)
		}
	}
	bad := []string{
		filepath.Join(root, "repo", "escape.png"),           // symlink out of the root
		filepath.Join(root, "repo", "outdir", "secret.png"), // through a symlinked directory
		filepath.Join(root, "repo", "..", "..", filepath.Base(outside), "secret.png"),
		filepath.Join(outside, "secret.png"),
		filepath.Join(state, "events.jsonl"), // the worker's own state
		filepath.Join(root, "repo", "notes.png"),
		filepath.Join(root, "repo", "big.png"),
		filepath.Join(root, "repo", "fifo.png"),
		filepath.Join(root, "repo", "missing.png"),
		"repo/shot.png", // relative
		"/etc/passwd",
	}
	for _, p := range bad {
		if img, err := st.storeFile(p); err == nil {
			t.Errorf("%s was read: %+v", p, img)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownImagesResolveLocalFilesOnly(t *testing.T) {
	st, root, _ := newTestStore(t)
	dir := filepath.Join(root, "repo")
	shot, chart := testPNG(t, color.White), []byte(testSVG)
	write(t, filepath.Join(dir, "screenshots", "home page.png"), shot)
	write(t, filepath.Join(dir, "out", "chart.svg"), chart)
	write(t, filepath.Join(root, "elsewhere.png"), shot)
	ti := st.task(dir, time.Now())
	text := "Here: ![Home](<screenshots/home page.png>) and ![chart](" + filepath.Join(dir, "out", "chart.svg") + ` "Chart")` +
		"\n![remote](https://example.com/a.png) ![data](data:image/png;base64,AAAA) ![up](../../../../etc/passwd)" +
		" ![file](file://" + filepath.Join(root, "elsewhere.png") + ") ![gone](missing.png) ![again](<screenshots/home page.png>)"
	got := ti.markdownImages(text)
	want := []claude.Image{
		{ID: ImageID(shot), Type: "image/png", Size: int64(len(shot)), Name: "home page.png", Path: filepath.Join(dir, "screenshots", "home page.png"), Ref: "screenshots/home page.png"},
		{ID: ImageID(chart), Type: "image/svg+xml", Size: int64(len(chart)), Name: "chart.svg", Path: filepath.Join(dir, "out", "chart.svg"), Ref: filepath.Join(dir, "out", "chart.svg")},
		{ID: ImageID(shot), Type: "image/png", Size: int64(len(shot)), Name: "elsewhere.png", Path: filepath.Join(root, "elsewhere.png"), Ref: "file://" + filepath.Join(root, "elsewhere.png")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Type != want[i].Type || got[i].Size != want[i].Size || got[i].Name != want[i].Name || got[i].Path != want[i].Path || got[i].Ref != want[i].Ref {
			t.Errorf("image %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNewFilesFindsWhatToolsWrote(t *testing.T) {
	st, root, _ := newTestStore(t)
	dir := filepath.Join(root, "repo")
	tmp := t.TempDir()
	st.tmp, st.roots = tmp, append(st.roots, tmp)
	old := testPNG(t, color.Black)
	write(t, filepath.Join(dir, "docs", "old.png"), old)
	past := time.Now().Add(-time.Hour)
	must(t, os.Chtimes(filepath.Join(dir, "docs", "old.png"), past, past))
	ti := st.task(dir, time.Now())

	shot := testPNG(t, color.White)
	write(t, filepath.Join(dir, "screenshots", "home.png"), shot)
	write(t, filepath.Join(dir, ".playwright-mcp", "page.png"), testPNG(t, color.Gray{1}))
	write(t, filepath.Join(dir, "node_modules", "pkg", "icon.png"), testPNG(t, color.Gray{2}))
	write(t, filepath.Join(dir, "fake.png"), []byte("text"))
	write(t, filepath.Join(tmp, "shot.webp"), []byte("RIFF\x24\x00\x00\x00WEBPVP8 "))
	got := ti.newFiles(maxEventImages)
	if len(got) != 2 || got[0].Path != filepath.Join(dir, "screenshots", "home.png") || got[0].ID != ImageID(shot) ||
		got[1].Path != filepath.Join(tmp, "shot.webp") || got[1].Type != "image/webp" {
		t.Fatalf("first scan: %+v", got)
	}
	if again := ti.newFiles(maxEventImages); len(again) != 0 {
		t.Fatalf("files were shown twice: %+v", again)
	}
	// A new version of a file is shown again; a copy of one already shown
	// is not.
	shot2 := testPNG(t, color.RGBA{R: 255, A: 255})
	write(t, filepath.Join(dir, "screenshots", "home.png"), shot2)
	future := time.Now().Add(time.Second)
	must(t, os.Chtimes(filepath.Join(dir, "screenshots", "home.png"), future, future))
	write(t, filepath.Join(dir, "copy.png"), shot)
	got = ti.newFiles(maxEventImages)
	if len(got) != 1 || got[0].ID != ImageID(shot2) {
		t.Fatalf("second scan: %+v", got)
	}
}

func TestTaskImagesBoundsAndStripsData(t *testing.T) {
	st, root, _ := newTestStore(t)
	ti := st.task(filepath.Join(root, "repo"), time.Now())
	var imgs []claude.Image
	for i := 0; i < maxEventImages+3; i++ {
		data := testPNG(t, color.Gray{uint8(i)})
		imgs = append(imgs, claude.Image{Type: "image/png", Size: int64(len(data)), Data: data})
	}
	e := claude.Event{Type: "tool_result", Images: imgs}
	ti.add(&e)
	if len(e.Images) != maxEventImages {
		t.Fatalf("kept %d images", len(e.Images))
	}
	for _, img := range e.Images {
		if img.Data != nil || !ValidImageID(img.ID) {
			t.Fatalf("image not stored: %+v", img)
		}
		if _, err := os.Stat(filepath.Join(st.dir, img.ID)); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeImageClaude prints what Claude Code prints when it reads an image
// (the tool_result shape of Claude Code 2.1.311's Read tool), writes a
// screenshot with Bash, and refers to it in its reply.
const fakeImageClaude = `#!/bin/bash
cat >/dev/null
echo '{"type":"system","subtype":"init","session_id":"` + testSession + `","model":"claude-test"}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x/orange.png"}}]}}'
echo '{"type":"user","message":{"content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"'"$PNG1"'","media_type":"image/png"}}]}]},"tool_use_result":{"type":"image","file":{"base64":"'"$PNG1"'","type":"image/png"}}}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"node shot.js"}}]}}'
sleep 0.5; mkdir -p screenshots && printf '%s' "$PNG2" | base64 -d > screenshots/home.png
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"saved"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Here it is: ![Home](screenshots/home.png)"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"done","session_id":"` + testSession + `"}'
`

func TestWorkerStoresImagesOutsideTheLog(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude")
	must(t, os.WriteFile(claudePath, []byte(fakeImageClaude), 0o755))
	png1, png2 := testPNG(t, color.RGBA{R: 255, G: 128, A: 255}), testPNG(t, color.White)
	t.Setenv("PNG1", base64.StdEncoding.EncodeToString(png1))
	t.Setenv("PNG2", base64.StdEncoding.EncodeToString(png2))

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	state := filepath.Join(root, ".mosso")
	must(t, os.MkdirAll(repo, 0o755))
	l, err := OpenLog(filepath.Join(state, "events.jsonl"))
	must(t, err)
	t.Cleanup(func() { l.Close() })
	w := New(l, claudePath)
	w.SetImages(filepath.Join(state, "images"), state, "", root)
	resp := post(t, httptestURL(t, w)+"/messages", Message{Text: "shoot", Prompt: "shoot", Dir: repo, Token: testToken})
	resp.Body.Close()
	w.Wait()

	raw, err := os.ReadFile(filepath.Join(state, "events.jsonl"))
	must(t, err)
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(png1)[:40]) {
		t.Fatal("the event log holds image data")
	}
	var read, bash, text *claude.Event
	for _, e := range l.After(0) {
		if e.Event == nil {
			continue
		}
		switch {
		case e.Event.ToolUseID == "toolu_1" && e.Event.Type == "tool_result":
			read = e.Event
		case e.Event.ToolUseID == "toolu_2" && e.Event.Type == "tool_result":
			bash = e.Event
		case e.Event.Type == "text":
			text = e.Event
		}
	}
	if read == nil || len(read.Images) != 1 || read.Images[0].ID != ImageID(png1) || read.Images[0].Type != "image/png" || read.Content != "" {
		t.Fatalf("Read result: %+v", read)
	}
	if bash == nil || len(bash.Images) != 1 || bash.Images[0].ID != ImageID(png2) || bash.Images[0].Path != filepath.Join(repo, "screenshots", "home.png") {
		t.Fatalf("Bash result: %+v", bash)
	}
	if text == nil || len(text.Images) != 1 || text.Images[0].Ref != "screenshots/home.png" || text.Images[0].ID != ImageID(png2) {
		t.Fatalf("text: %+v", text)
	}
	for _, data := range [][]byte{png1, png2} {
		got, err := os.ReadFile(filepath.Join(state, "images", ImageID(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("stored image: %v", err)
		}
	}
	// A restarted worker replays the same references.
	l2, err := OpenLog(filepath.Join(state, "events.jsonl"))
	must(t, err)
	defer l2.Close()
	n := 0
	for _, e := range l2.After(0) {
		if e.Event != nil {
			n += len(e.Event.Images)
		}
	}
	if n != 3 {
		t.Fatalf("replayed %d image references, want 3", n)
	}
}

func httptestURL(t *testing.T, w *Server) string {
	t.Helper()
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}
