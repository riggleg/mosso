package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"testing"
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
func TestServeFakeForBrowser(t *testing.T) {
	addr := os.Getenv("MOSSO_FAKE_ADDR")
	if addr == "" {
		t.Skip("set MOSSO_FAKE_ADDR to serve the UI over a fake sandbox")
	}
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = fakeRepoSandbox()
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
