package server

import (
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
// state and the Resume button.
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
