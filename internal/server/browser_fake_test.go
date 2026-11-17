package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"testing"

	"github.com/riggleg/mosso/internal/claude"
)

// TestServeFakeForBrowser serves the real web UI and API over a fake
// sandbox, for driving the UI in a real browser by hand or with a browser
// automation script. It does nothing unless MOSSO_FAKE_ADDR is set, for
// example: MOSSO_FAKE_ADDR=127.0.0.1:8099 go test ./internal/server -run
// TestServeFakeForBrowser -timeout 1h
//
// With MOSSO_FAKE_SCENARIO=stopped it also creates a workspace with one
// finished turn whose sandbox session has ended, to look at the stopped
// state and the Resume button. With MOSSO_FAKE_SCENARIO=history it creates
// a running workspace, a terminated one and an archived one, to look at
// the sidebar's History and Archived sections. MOSSO_FAKE_SCENARIO=novercel
// creates the same and then serves them from a Mosso without Vercel
// credentials: history is readable, nothing else works.
// MOSSO_FAKE_SCENARIO=unsaved creates the same and makes the database
// refuse updates, to look at a termination that cannot be saved.
// MOSSO_FAKE_SCENARIO=setup configures a default image and setup script
// whose output new workspaces show; with MOSSO_FAKE_SETUP_FAIL=1 the
// script fails, to look at how that is reported. MOSSO_FAKE_SCENARIO=sections
// creates sections (one of them the default) with workspaces in them, an
// unsectioned one and one in the history, to look at the sidebar's
// sections and the New workspace dialog. MOSSO_FAKE_SCENARIO=github creates
// a workspace in which Claude pushed and opened pull requests into master
// and uat with Mosso's GitHub tools, and one push failed.
// MOSSO_FAKE_SCENARIO=images creates a workspace in which Claude took a
// screenshot, wrote a chart and a large image, and showed the screenshot
// in its reply (next to a missing and a remote image); with
// MOSSO_FAKE_STOP=1 the sandbox session then stops, so only the images
// cached locally can be shown.
func TestServeFakeForBrowser(t *testing.T) {
	addr := os.Getenv("MOSSO_FAKE_ADDR")
	if addr == "" {
		t.Skip("set MOSSO_FAKE_ADDR to serve the UI over a fake sandbox")
	}
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	gh := &fakeGitHubAPI{remote: map[string]string{"uat": "5555555555555555555555555555555555555555"}}
	withGitHub(t, srv, gh)
	fake.stdoutFor = fakeRepoSandbox()
	if os.Getenv("MOSSO_FAKE_SCENARIO") == "github" {
		rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets","base":"main","instructions":"Run the relevant tests. When done, create pull requests targeting both main and uat."}`)
		var ws testWorkspace
		_ = json.Unmarshal(rec.Body.Bytes(), &ws)
		uat := ws.Branch + "-uat"
		fake.claude.script(
			call("github_push", `{}`),
			call("github_create_pr", `{"title":"Fix the flaky widget test","body":"Waits for the widget to render."}`),
			call("github_fetch", `{"branch":"uat"}`),
			call("github_push", `{"branch":"`+uat+`","source":"fix-uat"}`),
			call("github_create_pr", `{"title":"Fix the flaky widget test (uat)","head":"`+uat+`","base":"uat"}`),
			call("github_checks", `{"pr":1}`),
		)
		do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"Fix the flaky widget test"}`)
		gh.mu.Lock()
		gh.pushErr = errors.New("push: the push is not a fast-forward: " + uat + " on GitHub (555555555555) has commits that fix-uat (4444444) does not contain, and Mosso does not force-push for agents. Mosso has fetched it as origin/" + uat + " in the sandbox: rebase onto or merge it, then push again")
		gh.mu.Unlock()
		fake.claude.script(call("github_push", `{"branch":"`+uat+`","source":"fix-uat"}`))
		do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"Push the uat fix again"}`)
		t.Logf("workspace %s", ws.Name)
	}
	if os.Getenv("MOSSO_FAKE_SCENARIO") == "setup" {
		body, _ := json.Marshal(map[string]string{
			"default_image":        "my-team/my-project/dev-image:v1",
			"default_setup_script": "curl https://mise.run | sh\nmise trust && mise install\necho 'export GOFLAGS=-mod=mod' >> \"$MOSSO_ENV\"\n",
		})
		do(t, srv.Handler(), http.MethodPost, "/settings", string(body))
		const output = "mise: installing mise...\nmise: installed successfully to ~/.local/bin/mise\n" +
			"mise ✓ trusted /vercel/sandbox/widgets/mise.toml\nmise go@1.27.4 ✓ installed\nmise node@24.11.0 ✓ installed\n"
		fail := os.Getenv("MOSSO_FAKE_SETUP_FAIL") == "1"
		fake.stdoutFor = func(cmd map[string]any) string {
			if isSetup(cmd) {
				if fail {
					return output + "mise ERROR Failed to install go@1.99: no such version\n"
				}
				return output
			}
			return fakeRepoSandbox()(cmd)
		}
		fake.exitFor = func(cmd map[string]any) int {
			if fail && isSetup(cmd) {
				return 1
			}
			return 0
		}
	}
	if os.Getenv("MOSSO_FAKE_SCENARIO") == "stopped" {
		rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
		var ws testWorkspace
		_ = json.Unmarshal(rec.Body.Bytes(), &ws)
		do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"Add a README badge"}`)
		fake.timeout(ws.Name)
		t.Logf("workspace %s: sandbox session stopped", ws.Name)
	}
	scenario := os.Getenv("MOSSO_FAKE_SCENARIO")
	if scenario == "history" || scenario == "novercel" || scenario == "unsaved" {
		for i, prompt := range []string{"Fix the flaky test", "Add a README badge", "Tidy the Makefile"} {
			rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
			var ws testWorkspace
			_ = json.Unmarshal(rec.Body.Bytes(), &ws)
			do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"`+prompt+`"}`)
			if i < 2 {
				do(t, srv.Handler(), http.MethodDelete, "/workspaces/"+ws.Name, "")
			}
			if i == 0 {
				do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/archive", `{}`)
			}
			t.Logf("workspace %s: %q", ws.Name, prompt)
		}
	}
	if scenario == "sections" {
		fakeSections(t, srv.Handler())
	}
	if scenario == "images" {
		fakeImages(t, srv, fake)
	}
	switch scenario {
	case "novercel":
		// The same database, opened by a server with no Vercel client.
		srv.mu.Lock()
		var dbPath string
		err := srv.store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath)
		srv.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		srv = New(nil)
		withToken(srv, testClaudeToken, nil)
		withGitHub(t, srv, &fakeGitHubAPI{})
		if err := srv.Restore(context.Background(), dbPath, ""); err != nil {
			t.Fatal(err)
		}
	case "unsaved":
		failWrites(t, srv, "UPDATE ON workspaces")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ts := &httptest.Server{Listener: l, Config: &http.Server{Handler: srv.Handler()}}
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(srv.CloseStreams)
	t.Logf("serving on http://%s", addr)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}

// fakeSections fills a fake Mosso with sections and workspaces in them.
func fakeSections(t *testing.T, h http.Handler) {
	const perxInstructions = "Follow the repository's existing conventions and run relevant tests. When implementation is complete, create pull requests targeting both master and uat. Handle branch differences as appropriate."
	mosso := createSection(t, h, map[string]string{"name": "Mosso", "repo": "octo/widgets", "model": "claude-opus-5-5", "setup_script": "curl https://mise.run | sh\nmise trust && mise install\n", "instructions": "Run go vet and go test ./... before finishing."})
	perx := createSection(t, h, map[string]string{"name": "Perx API", "repo": "octo/widgets", "base": "main", "model": "claude-sonnet-5-5", "instructions": perxInstructions})
	layer6 := createSection(t, h, map[string]string{"name": "Perx API Layer 6", "repo": "octo/widgets", "base": "dev", "model": "claude-fable-5-2", "instructions": "Layer 6 only: keep changes inside the layer6 module."})
	do(t, h, http.MethodPost, "/settings", `{"default_section_id":`+sid(perx.ID)+`}`)
	for _, w := range []struct {
		section int64
		prompt  string
		end     bool
	}{
		{perx.ID, "Add pagination to the vouchers endpoint", false},
		{perx.ID, "Fix the flaky rewards spec", false},
		{mosso.ID, "Add workspace sections", false},
		{0, "Tidy the Makefile", false},
		{layer6.ID, "Upgrade the layer 6 client", true},
	} {
		ws := createSectionWorkspace(t, h, `{"section_id":`+sid(w.section)+`,"repo":"octo/widgets"}`)
		do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"`+w.prompt+`"}`)
		if w.end {
			do(t, h, http.MethodDelete, "/workspaces/"+ws.Name, "")
		}
	}
}

// fakeImages fills a fake Mosso with a workspace whose chat shows images.
func fakeImages(t *testing.T, srv *Server, fake *fakeVercel) {
	shot := demoPNG(t, 640, 400)
	big := noisePNG(t, 900, 800) // too large to cache locally
	events := screenshotEvents()
	events[1].Images[0] = claude.Image{Type: "image/png", Size: int64(len(shot)), Data: shot, Name: "home.png", Path: "/vercel/sandbox/widgets/screenshots/home.png"}
	events[3].Images[1] = claude.Image{Type: "image/png", Size: int64(len(big)), Data: big, Name: "big.png"}
	events = append(events, claude.Event{Type: "text",
		Text:   "Here is the home page:\n\n![Home page](screenshots/home.png)\n\nNot found: ![old](screenshots/old.png). Remote: ![logo](https://example.com/logo.png).",
		Images: []claude.Image{{Type: "image/png", Size: int64(len(shot)), Data: shot, Name: "home.png", Path: "/vercel/sandbox/widgets/screenshots/home.png", Ref: "screenshots/home.png"}}})
	fake.claude.mu.Lock()
	fake.claude.events = events
	fake.claude.mu.Unlock()
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"Take a screenshot of the home page with Playwright"}`)
	fake.claude.mu.Lock()
	fake.claude.events = nil
	fake.claude.mu.Unlock()
	if os.Getenv("MOSSO_FAKE_STOP") == "1" {
		srv.imageFetches.Wait() // cached first, as while following a running sandbox
		fake.timeout(ws.Name)
	}
	t.Logf("workspace %s", ws.Name)
}

// demoPNG draws a page-like picture: a header bar and some text lines.
func demoPNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{250, 250, 250, 255}
			switch {
			case y < 56:
				c = color.RGBA{37, 99, 235, 255}
			case y > 90 && y%40 < 12 && x > 40 && x < w-40-(y%120):
				c = color.RGBA{200, 205, 215, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// noisePNG is a PNG of random pixels, which compresses badly.
func noisePNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	_, _ = rand.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if b.Len() <= maxCachedImageBytes {
		t.Fatalf("noisePNG is only %d bytes", b.Len())
	}
	return b.Bytes()
}
