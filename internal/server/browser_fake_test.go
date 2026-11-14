package server

import (
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
func TestServeFakeForBrowser(t *testing.T) {
	addr := os.Getenv("MOSSO_FAKE_ADDR")
	if addr == "" {
		t.Skip("set MOSSO_FAKE_ADDR to serve the UI over a fake sandbox")
	}
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = fakeRepoSandbox()
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
