package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"testing"
	"time"
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
// MOSSO_FAKE_SCENARIO=idle creates a workspace with one finished turn that
// idle shutdown stopped, and a running one with a 5-minute idle timeout,
// to look at idle shutdown. MOSSO_FAKE_PLAN (hobby, pro, enterprise or any
// other name) is the Vercel plan the fake reports; unset, it is unknown.
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
	fake.setPlan(os.Getenv("MOSSO_FAKE_PLAN"))
	if os.Getenv("MOSSO_FAKE_SCENARIO") == "idle" {
		rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
		var ws testWorkspace
		_ = json.Unmarshal(rec.Body.Bytes(), &ws)
		do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", `{"prompt":"Add a README badge"}`)
		srv.checkIdle(time.Now().Add(16 * time.Minute))
		t.Logf("workspace %s: stopped by idle shutdown", ws.Name)
		rec = do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets","idle_timeout_seconds":300}`)
		_ = json.Unmarshal(rec.Body.Bytes(), &ws)
		t.Logf("workspace %s: running, stops after 5 idle minutes", ws.Name)
	}
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
