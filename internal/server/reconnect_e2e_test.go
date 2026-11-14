package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// TestReconnectEndToEnd is the acceptance test for the remote worker and
// persistence milestone, against real accounts:
//
//  1. Create a workspace and start a Claude task that takes a while and
//     uses tools.
//  2. Shut the Mosso server down while Claude is still working.
//  3. Check, through the Vercel API, that the worker in the sandbox keeps
//     going and finishes.
//  4. Start a new Mosso server over the same data directory: it reconnects,
//     fetches the missed events, and the whole conversation is there once.
//  5. Send a follow-up, which continues the Claude session.
//  6. Stop the sandbox session (as a timeout would), restart Mosso again,
//     resume the sandbox and check the history and a follow-up again.
//
// It uses real credentials, creates one real Vercel Sandbox (persistent,
// destroyed at the end with its snapshots), clones a repository and uses
// your Claude subscription, so it only runs when asked:
//
//	MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/disposable-repo \
//	  go test ./internal/server -run TestReconnectEndToEnd -v -timeout 40m
//
// It needs VERCEL_TOKEN, VERCEL_TEAM_ID, VERCEL_PROJECT_ID, a Claude Code
// token (`mosso auth claude` or CLAUDE_CODE_OAUTH_TOKEN), a GitHub token
// that can read the repository, and Go (to build the worker). Nothing is
// pushed to GitHub.
func TestReconnectEndToEnd(t *testing.T) {
	if os.Getenv("MOSSO_E2E") != "1" {
		t.Skip("set MOSSO_E2E=1 to run against real Vercel, Claude and GitHub accounts")
	}
	repo := os.Getenv("MOSSO_E2E_GITHUB_REPO")
	if !github.ValidRepo(repo) {
		t.Skip("set MOSSO_E2E_GITHUB_REPO=owner/repo to a repository the GitHub token can read")
	}
	cfg, err := vercel.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := claude.LoadToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := github.LoadToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	vc := vercel.NewClient(cfg)
	dataDir := t.TempDir()

	var wsName string
	t.Cleanup(func() {
		if wsName == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sb, err := vc.Get(ctx, wsName, false)
		if vercel.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Errorf("look up sandbox %s for cleanup: %v (delete it in the Vercel dashboard)", wsName, err)
			return
		}
		if err := vc.Destroy(ctx, *sb); err != nil {
			t.Errorf("destroy sandbox %s: %v (delete it in the Vercel dashboard)", wsName, err)
		}
	})

	start := func() (*Server, *httptest.Server) {
		srv := New(vc)
		if err := srv.Restore(context.Background(), dataDir); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv.Handler())
		return srv, ts
	}
	stop := func(srv *Server, ts *httptest.Server) {
		srv.CloseStreams()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		ts.Close()
	}
	status := func(srv *Server) workspace {
		var ws workspace
		rec := do(t, srv.Handler(), http.MethodGet, "/workspaces/"+wsName, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("get workspace: %d %s", rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &ws)
		return ws
	}
	waitStatus := func(srv *Server, want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			ws := status(srv)
			if ws.Status == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("status = %s (%s), want %s", ws.Status, ws.Error, want)
			}
			time.Sleep(2 * time.Second)
		}
	}
	history := func(srv *Server) []chatEntry {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return append([]chatEntry(nil), srv.workspaces[wsName].chat.entries...)
	}
	count := func(entries []chatEntry, typ string) int {
		n := 0
		for _, e := range entries {
			if e.Type == typ {
				n++
			}
		}
		return n
	}

	// 1. A workspace and a slow task with tool activity.
	srv, ts := start()
	body, _ := json.Marshal(map[string]any{"repo": repo, "base": os.Getenv("MOSSO_E2E_GITHUB_BASE"), "timeout_seconds": 1500})
	began := time.Now()
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rec.Code, rec.Body)
	}
	var created testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	wsName = created.Name
	t.Logf("workspace %s created in %s", wsName, time.Since(began).Round(time.Second))

	events := openEvents(t, ts.URL, wsName, "")
	prompt := "Remember the codeword PINEAPPLE. Then use the Bash tool to run exactly: sleep 45 && echo mosso-done. " +
		"After it finishes, reply with only the command's output. Do not change any files."
	b, _ := json.Marshal(map[string]string{"prompt": prompt})
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/chat", string(b)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	// Wait for Claude's first tool call, then pull the plug.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		e := events.entry()
		if e.Type == "claude" && e.Event.Type == "tool_use" {
			t.Logf("Claude called %s; shutting Mosso down", e.Event.Name)
			break
		}
		if e.Type == "error" || e.Type == "result" || time.Now().After(deadline) {
			t.Fatalf("no tool call before the turn ended: %+v", e)
		}
	}
	session := status(srv).Sandbox.SessionID

	// 2. Mosso stops while Claude works.
	stop(srv, ts)

	// 3. The worker keeps going without Mosso.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	probe := New(vc) // only for its worker transport; never serves
	var st worker.Status
	for {
		st, err = probe.workerStatus(ctx, session)
		if err != nil {
			t.Fatalf("worker status after shutdown: %v", err)
		}
		t.Logf("worker without Mosso: %s, %d events", st.State, st.Seq)
		if st.State == "idle" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Claude did not finish in time")
		case <-time.After(5 * time.Second):
		}
	}
	res, err := probe.workerCall(ctx, session, http.MethodGet, fmt.Sprintf("/events?after=%d", st.Seq-1), nil)
	if err != nil || !strings.Contains(string(res.Body), `"type":"result"`) {
		t.Fatalf("the worker did not record a result: %v %s", err, res.Body)
	}

	// 4. Mosso restarts and catches up.
	srv, ts = start()
	waitStatus(srv, "ready", 3*time.Minute)
	hist := history(srv)
	t.Logf("history after restart: %s", entryTypes(hist))
	if count(hist, "user") != 1 || count(hist, "result") != 1 {
		t.Fatalf("history has %d user and %d result entries, want one each", count(hist, "user"), count(hist, "result"))
	}
	last := hist[len(hist)-1]
	if last.Type != "result" || !strings.Contains(last.Result.Text, "mosso-done") {
		t.Fatalf("last entry = %+v, want the result with mosso-done", last)
	}
	var sawBash bool
	for _, e := range hist {
		sawBash = sawBash || (e.Event != nil && e.Event.Type == "tool_use" && e.Event.Name == "Bash")
	}
	if !sawBash {
		t.Fatal("the Bash tool call made while Mosso was down is missing")
	}

	// 5. A follow-up continues the conversation.
	events = openEvents(t, ts.URL, wsName, "")
	for range hist {
		events.entry()
	}
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/chat", `{"prompt":"What codeword did I ask you to remember? Reply with only the codeword."}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("follow-up: %d", resp.StatusCode)
	}
	more := events.until("result")
	if r := more[len(more)-1].Result; !strings.Contains(strings.ToUpper(r.Text), "PINEAPPLE") {
		t.Fatalf("follow-up result = %q, want PINEAPPLE", r.Text)
	}
	t.Log("follow-up remembered the codeword")

	// 6. The session stops (like a timeout); Mosso restarts, resumes the
	// sandbox and the conversation.
	stop(srv, ts)
	if err := vc.Stop(ctx, session); err != nil {
		t.Fatal(err)
	}
	t.Log("sandbox session stopped")
	srv, ts = start()
	waitStatus(srv, "stopped", 2*time.Minute)
	if n := len(history(srv)); n != len(hist)+len(more) {
		t.Fatalf("history after the second restart has %d entries, want %d", n, len(hist)+len(more))
	}
	began = time.Now()
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/connect", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	t.Logf("sandbox resumed in %s", time.Since(began).Round(time.Second))
	ws := status(srv)
	if ws.Sandbox.SessionID == session || ws.Status != "ready" {
		t.Fatalf("after resume: session %s, status %s", ws.Sandbox.SessionID, ws.Status)
	}
	events = openEvents(t, ts.URL, wsName, "")
	for range history(srv) {
		events.entry()
	}
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/chat", `{"prompt":"Say the codeword again, and nothing else."}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("follow-up after resume: %d", resp.StatusCode)
	}
	again := events.until("result")
	if r := again[len(again)-1].Result; !strings.Contains(strings.ToUpper(r.Text), "PINEAPPLE") {
		t.Fatalf("result after resume = %q, want PINEAPPLE", r.Text)
	}
	t.Log("the conversation continued after the sandbox resumed")

	// Terminate: the sandbox, its snapshots and the saved state go away.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/workspaces/"+wsName, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %v %v", resp, err)
	}
	stop(srv, ts)
	if _, err := vc.Get(ctx, wsName, false); !vercel.IsNotFound(err) {
		t.Fatalf("sandbox still exists after terminate: %v", err)
	}
	wsName = ""
}
