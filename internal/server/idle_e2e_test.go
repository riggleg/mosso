package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

// TestIdleShutdownEndToEnd checks plan detection and idle shutdown against
// real accounts:
//
//  1. Mosso reads the Vercel plan and its session limit.
//  2. A workspace with a 1-minute idle timeout runs a Claude task that
//     takes longer than that; idle checks during the task stop nothing.
//  3. Once the task ends, an idle check stops the session: Vercel reports
//     the sandbox stopped, not deleted.
//  4. A follow-up resumes the sandbox and continues the same Claude
//     conversation.
//
// It creates one persistent sandbox (destroyed at the end with its
// snapshots) and uses your Claude subscription, so it only runs when asked:
//
//	MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/disposable-repo \
//	  go test ./internal/server -run TestIdleShutdownEndToEnd -v -timeout 30m
//
// It needs the same credentials as TestReconnectEndToEnd.
func TestIdleShutdownEndToEnd(t *testing.T) {
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
	vc := vercel.NewClient(cfg)
	srv := New(vc)
	if err := srv.Restore(context.Background(), filepath.Join(t.TempDir(), "mosso.db"), ""); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.CloseStreams)

	// 1. The plan.
	plan := srv.vercelPlan(context.Background(), true)
	if plan.PlanError != "" {
		t.Fatalf("plan detection: %s", plan.PlanError)
	}
	t.Logf("Vercel plan %q: sessions up to %s", plan.Plan, fmtDuration(plan.maxSession()))

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
		if err == nil {
			err = vc.Destroy(ctx, *sb)
		}
		if err != nil {
			t.Errorf("destroy sandbox %s: %v (delete it in the Vercel dashboard)", wsName, err)
		}
	})

	// 2. A task longer than the idle timeout.
	body, _ := json.Marshal(map[string]any{"repo": repo, "base": os.Getenv("MOSSO_E2E_GITHUB_BASE"), "idle_timeout_seconds": 60})
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rec.Code, rec.Body)
	}
	var created testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	wsName = created.Name
	// results counts Claude's finished turns; idle reports whether nothing
	// runs in the workspace.
	results := func() (n int, last string) {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		for _, e := range srv.workspaces[wsName].chat.entries {
			if e.Type == "result" && e.Result != nil {
				n, last = n+1, e.Result.Text
			}
		}
		return n, last
	}
	idle := func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return !srv.busy[wsName]
	}
	b, _ := json.Marshal(map[string]string{"prompt": "Remember the codeword PINEAPPLE. Use the Bash tool to run exactly: sleep 100 && echo mosso-done. Then reply with only its output. Do not change any files."})
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/chat", string(b)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	running := func() bool {
		sb, err := vc.Get(context.Background(), wsName, false)
		if err != nil {
			t.Fatal(err)
		}
		return sb.Running()
	}
	deadline := time.Now().Add(10 * time.Minute)
	checks := 0
	for {
		time.Sleep(20 * time.Second)
		if n, _ := results(); n > 0 && idle() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Claude did not finish within 10 minutes")
		}
		srv.checkIdle(time.Now().Add(10 * time.Minute))
		checks++
		if !running() {
			t.Fatal("idle shutdown stopped the sandbox while Claude was working")
		}
	}
	if checks < 3 {
		t.Fatalf("the task ended after %d idle checks; it should outlast the idle timeout", checks)
	}
	t.Logf("Claude finished; %d idle checks during the task stopped nothing", checks)

	// 3. Idle: stopped, not deleted.
	srv.checkIdle(time.Now().Add(2 * time.Minute))
	sb, err := vc.Get(context.Background(), wsName, false)
	if err != nil {
		t.Fatalf("the sandbox after idle shutdown: %v", err)
	}
	if sb.Running() {
		t.Fatalf("the sandbox is %s after idle shutdown, want stopped", sb.Status)
	}
	t.Logf("idle shutdown stopped the sandbox (status %s)", sb.Status)

	// 4. A follow-up resumes it and continues the conversation.
	b, _ = json.Marshal(map[string]string{"prompt": "What was the codeword? Reply with only the word."})
	if resp := postJSON(t, ts.URL+"/workspaces/"+wsName+"/chat", string(b)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("follow-up: %d", resp.StatusCode)
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		time.Sleep(5 * time.Second)
		if n, last := results(); n >= 2 {
			if !strings.Contains(strings.ToUpper(last), "PINEAPPLE") {
				t.Fatalf("follow-up after resuming = %q, want the codeword", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer to the follow-up within 5 minutes")
		}
	}
	if !running() {
		t.Fatal("the follow-up did not resume the sandbox")
	}
}
