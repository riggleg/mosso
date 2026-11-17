package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/worker"
)

// chartSVG is a bar chart with scripts, which must never run.
const chartSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="240" height="160" viewBox="0 0 120 80" onload="alert(1)"><script>alert(2)</script>` +
	`<rect width="120" height="80" fill="#fff"/><rect x="10" y="40" width="20" height="35" fill="#2563eb"/><rect x="40" y="20" width="20" height="55" fill="#2563eb"/><rect x="70" y="50" width="20" height="25" fill="#2563eb"/></svg>`

// bigPNG is a PNG (by its signature) too large to cache locally.
var bigPNG = append(append([]byte(nil), onePixelPNG[:16]...), make([]byte, maxCachedImageBytes+1)...)

// screenshotEvents is what Claude's activity looks like when a tool
// returned a screenshot and a script wrote a chart and a large image: the
// worker stores the bytes and records references.
func screenshotEvents() []claude.Event {
	return []claude.Event{
		{Type: "tool_use", ToolUseID: "toolu_shot", Name: "mcp__playwright__browser_take_screenshot", Input: json.RawMessage(`{}`)},
		{Type: "tool_result", ToolUseID: "toolu_shot", Content: "Took a screenshot", Images: []claude.Image{{Type: "image/png", Size: int64(len(onePixelPNG)), Data: onePixelPNG}}},
		{Type: "tool_use", ToolUseID: "toolu_chart", Name: "Bash", Input: json.RawMessage(`{"command":"python chart.py"}`)},
		{Type: "tool_result", ToolUseID: "toolu_chart", Content: "saved", Images: []claude.Image{
			{Type: "image/svg+xml", Size: int64(len(chartSVG)), Data: []byte(chartSVG), Name: "chart.svg", Path: "/vercel/sandbox/repo/out/chart.svg"},
			{Type: "image/png", Size: int64(len(bigPNG)), Data: bigPNG, Name: "big.png"},
		}},
	}
}

func imageURL(base, ws, id string) string { return base + "/workspaces/" + ws + "/images/" + id }

func waitForFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not created", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestImagesAreShownServedAndKept follows a screenshot from Claude's
// activity to the browser, through a Mosso restart, a stopped and resumed
// sandbox and termination.
func TestImagesAreShownServedAndKept(t *testing.T) {
	h := newPersistentHarness(t)
	h.fake.claude.mu.Lock()
	h.fake.claude.events = screenshotEvents()
	h.fake.claude.mu.Unlock()
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("take a screenshot")
	entries := events.until("result")

	var images []claude.Image
	for _, e := range entries {
		if e.Event != nil {
			images = append(images, e.Event.Images...)
		}
	}
	shotID, svgID, bigID := worker.ImageID(onePixelPNG), worker.ImageID([]byte(chartSVG)), worker.ImageID(bigPNG)
	if len(images) != 3 || images[0].ID != shotID || images[1].ID != svgID || images[2].ID != bigID || images[1].Name != "chart.svg" {
		t.Fatalf("images in the chat: %+v", images)
	}
	if b, _ := json.Marshal(entries); bytes.Contains(b, []byte("iVBORw0KGgo")) {
		t.Fatal("image data in the chat")
	}

	// Served from the sandbox, with safe headers.
	resp, body := get(t, imageURL(h.ts.URL, h.ws.Name, shotID))
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, onePixelPNG) || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("screenshot: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body = get(t, imageURL(h.ts.URL, h.ws.Name, svgID))
	if resp.StatusCode != http.StatusOK || string(body) != chartSVG || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("svg: %d %v", resp.StatusCode, resp.Header)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || strings.Contains(csp, "script") {
		t.Fatalf("an SVG must not run scripts: %q", csp)
	}
	resp, _ = get(t, imageURL(h.ts.URL, h.ws.Name, svgID)+"?download=1")
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Content-Disposition") != `attachment; filename=chart.svg` {
		t.Fatalf("download: %v", resp.Header)
	}
	resp, _ = get(t, imageURL(h.ts.URL, h.ws.Name, shotID)+"?download=1")
	if resp.Header.Get("Content-Disposition") != `attachment; filename=image-`+shotID[:8]+`.png` {
		t.Fatalf("download of an unnamed image: %v", resp.Header)
	}
	if resp, body := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusOK || !bytes.Equal(body, bigPNG) {
		t.Fatalf("large image: %d", resp.StatusCode)
	}

	// Only images the chat shows are served, and only valid IDs.
	for _, id := range []string{worker.ImageID([]byte("other")), "..%2F..%2Fevents.jsonl", "ZZ", strings.Repeat("a", 64)} {
		if resp, _ := get(t, imageURL(h.ts.URL, h.ws.Name, id)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("image %q: status %d", id, resp.StatusCode)
		}
	}

	// Small images were cached on this machine as they arrived; large
	// ones are not.
	cache := filepath.Join(h.dataDir, "images", h.ws.Name)
	waitForFile(t, filepath.Join(cache, shotID))
	waitForFile(t, filepath.Join(cache, svgID))
	if _, err := os.Stat(filepath.Join(cache, bigID)); err == nil {
		t.Fatal("a large image was cached")
	}
	if info, err := os.Stat(cache); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory: %v %v", info.Mode(), err)
	}

	// Mosso restarts and the sandbox session ends: the chat still has the
	// images; cached ones are shown without resuming the sandbox, others
	// say the sandbox is stopped.
	h.stop()
	h.fake.timeout(h.ws.Name)
	stopped := h.fake.session(h.ws.Name)
	h.start()
	h.waitStatus("stopped")
	n := 0
	for _, e := range h.history() {
		if e.Event != nil {
			n += len(e.Event.Images)
		}
	}
	if n != 3 {
		t.Fatalf("%d images in the restored chat", n)
	}
	if resp, body := get(t, imageURL(h.ts.URL, h.ws.Name, shotID)); resp.StatusCode != http.StatusOK || !bytes.Equal(body, onePixelPNG) {
		t.Fatalf("cached screenshot while stopped: %d", resp.StatusCode)
	}
	if resp, _ := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusGone {
		t.Fatalf("uncached image while stopped: %d", resp.StatusCode)
	}
	if h.fake.session(h.ws.Name) != stopped {
		t.Fatal("showing an image resumed the sandbox")
	}

	// Resumed, the large one is readable again, unless the sandbox's copy
	// was changed: content that does not match its ID is refused.
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/connect", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	if resp, body := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusOK || !bytes.Equal(body, bigPNG) {
		t.Fatalf("large image after resume: %d", resp.StatusCode)
	}
	stored := filepath.Join(h.fake.worker(h.ws.Name).dir, "images", bigID)
	if err := os.WriteFile(stored, []byte("CLAUDE_CODE_OAUTH_TOKEN=leaked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp, body := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusNotFound || bytes.Contains(body, []byte("leaked")) {
		t.Fatalf("changed image: %d %s", resp.StatusCode, body)
	}
	if err := os.Remove(stored); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted image: %d", resp.StatusCode)
	}

	// Terminated, the history keeps showing what was cached.
	h.terminate()
	if resp, _ := get(t, imageURL(h.ts.URL, h.ws.Name, svgID)); resp.StatusCode != http.StatusOK {
		t.Fatalf("cached image in the history: %d", resp.StatusCode)
	}
	if resp, _ := get(t, imageURL(h.ts.URL, h.ws.Name, bigID)); resp.StatusCode != http.StatusGone {
		t.Fatalf("uncached image in the history: %d", resp.StatusCode)
	}
}

func TestImagesAreIsolatedBetweenWorkspaces(t *testing.T) {
	srv, fake, ts, a := newChatServer(t)
	fake.claude.mu.Lock()
	fake.claude.events = screenshotEvents()
	fake.claude.mu.Unlock()
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	var b testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	events := openEvents(t, ts.URL, a.Name, "")
	postJSON(t, ts.URL+"/workspaces/"+a.Name+"/chat", `{"prompt":"shoot"}`)
	events.until("result")
	id := worker.ImageID(onePixelPNG)
	if resp, _ := get(t, imageURL(ts.URL, a.Name, id)); resp.StatusCode != http.StatusOK {
		t.Fatalf("A's own image: %d", resp.StatusCode)
	}
	// B's chat never showed it, so B's endpoint does not serve it, even
	// though the same content could exist in B's sandbox.
	if resp, _ := get(t, imageURL(ts.URL, b.Name, id)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B served A's image: %d", resp.StatusCode)
	}
	if resp, _ := get(t, imageURL(ts.URL, "nope", id)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown workspace: %d", resp.StatusCode)
	}
}

func TestImageFilename(t *testing.T) {
	cases := []struct {
		img  claude.Image
		typ  string
		want string
	}{
		{claude.Image{ID: "0123456789abcdef0123456789abcdef", Name: "home.png"}, "image/png", "home.png"},
		{claude.Image{ID: "0123456789abcdef0123456789abcdef", Name: "photo.jpeg"}, "image/jpeg", "photo.jpeg"},
		{claude.Image{ID: "0123456789abcdef0123456789abcdef", Name: "fake.png"}, "image/svg+xml", "fake.png.svg"},
		{claude.Image{ID: "0123456789abcdef0123456789abcdef", Name: "../../x.gif"}, "image/gif", "x.gif"},
		{claude.Image{ID: "0123456789abcdef0123456789abcdef"}, "image/webp", "image-01234567.webp"},
	}
	for _, c := range cases {
		if got := imageFilename(c.img, c.typ); got != c.want {
			t.Errorf("imageFilename(%+v) = %q, want %q", c.img, got, c.want)
		}
	}
}
