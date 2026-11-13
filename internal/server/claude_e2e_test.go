package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
)

// TestClaudeEndToEnd is the milestone acceptance test. It uses real
// credentials, creates a real Vercel Sandbox and uses your Claude
// subscription, so it only runs when asked:
//
//	MOSSO_E2E=1 go test ./internal/server -run TestClaudeEndToEnd -v -timeout 20m
//
// It needs VERCEL_TOKEN, VERCEL_TEAM_ID, VERCEL_PROJECT_ID and a Claude Code
// token in the macOS Keychain (`mosso auth claude`) or CLAUDE_CODE_OAUTH_TOKEN.
func TestClaudeEndToEnd(t *testing.T) {
	if os.Getenv("MOSSO_E2E") != "1" {
		t.Skip("set MOSSO_E2E=1 to run against real Vercel and Claude accounts")
	}
	cfg, err := vercel.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, source, err := claude.LoadToken(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("Claude token source: %s", source)
	}
	srv := New(vercel.NewClient(cfg))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("destroy sandbox: %v (check the Vercel dashboard for mosso-* sandboxes)", err)
		}
	})
	h := srv.Handler()

	rec := do(t, h, http.MethodPost, "/sandboxes", `{"timeout_seconds":900}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sandbox: %d %s", rec.Code, rec.Body)
	}
	var sb vercel.Sandbox
	_ = json.Unmarshal(rec.Body.Bytes(), &sb)
	t.Logf("sandbox %s", sb.Name)
	path := "/sandboxes/" + sb.Name + "/claude/messages"

	send := func(body string) claude.Turn {
		t.Helper()
		start := time.Now()
		rec := do(t, h, http.MethodPost, path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("message: %d %s", rec.Code, rec.Body)
		}
		var turn claude.Turn
		if err := json.Unmarshal(rec.Body.Bytes(), &turn); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s in %s: session=%s model=%s events=%d result=%q",
			body, time.Since(start).Round(time.Second), turn.SessionID, turn.Model, len(turn.Events), turn.Result)
		if turn.IsError {
			t.Fatalf("claude reported an error: %s", turn.Result)
		}
		return turn
	}

	first := send(`{"prompt":"Use the Bash tool to run: echo mosso-$((6*7)). Then remember the codeword PINEAPPLE. Reply with only the command's output."}`)
	if !strings.Contains(first.Result, "mosso-42") {
		t.Errorf("first result = %q, want it to contain mosso-42", first.Result)
	}
	var usedBash bool
	for _, e := range first.Events {
		usedBash = usedBash || (e.Type == "tool_use" && e.Name == "Bash")
	}
	if !usedBash {
		t.Error("expected a Bash tool_use event")
	}

	second := send(`{"prompt":"What codeword did I ask you to remember? Reply with only the codeword.","session_id":"` + first.SessionID + `"}`)
	if second.SessionID != first.SessionID {
		t.Errorf("follow-up session = %s, want %s", second.SessionID, first.SessionID)
	}
	if !strings.Contains(strings.ToUpper(second.Result), "PINEAPPLE") {
		t.Errorf("follow-up result = %q, want PINEAPPLE", second.Result)
	}
}
