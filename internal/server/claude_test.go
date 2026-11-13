package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
)

const (
	testClaudeToken = "sk-ant-oat01-server-test-token"
	testSessionID   = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

func withToken(srv *Server, tok string, err error) {
	srv.claudeToken = func(context.Context) (claude.Token, string, error) {
		return claude.Token(tok), claude.SourceKeychain, err
	}
}

// fakeClaude scripts the sandbox: the install script prints a version and
// each claude run echoes the prompt back as its result.
func fakeClaude(cmd map[string]any) string {
	args, _ := json.Marshal(cmd["args"])
	if strings.Contains(string(args), "install.sh") {
		return "2.1.311 (Claude Code)\n"
	}
	env, _ := cmd["env"].(map[string]any)
	result, _ := json.Marshal(fmt.Sprintf("you said %v", env["MOSSO_PROMPT"]))
	return `{"type":"system","subtype":"init","session_id":"` + testSessionID + `","model":"claude-test"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"README.md"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":` + string(result) + `,"session_id":"` + testSessionID + `"}` + "\n"
}

func createSandbox(t *testing.T, h http.Handler) vercel.Sandbox {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/sandboxes", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var sb vercel.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestClaudeConversation(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	fake.stdoutFor = fakeClaude
	h := srv.Handler()
	sb := createSandbox(t, h)
	path := "/sandboxes/" + sb.Name + "/claude/messages"

	rec := do(t, h, http.MethodPost, path, `{"prompt":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), testClaudeToken) {
		t.Fatal("response leaks the token")
	}
	var turn claude.Turn
	if err := json.Unmarshal(rec.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.SessionID != testSessionID || turn.Result != "you said hello" || len(turn.Events) != 2 || turn.Events[0].Name != "Bash" {
		t.Fatalf("unexpected turn: %+v", turn)
	}

	rec = do(t, h, http.MethodPost, path, `{"prompt":"and again","session_id":"`+turn.SessionID+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "you said and again") {
		t.Fatalf("follow-up status = %d: %s", rec.Code, rec.Body)
	}

	// install, first turn, follow-up: Claude Code is installed only once.
	if len(fake.cmds) != 3 {
		t.Fatalf("got %d commands, want 3 (install once, then two turns)", len(fake.cmds))
	}
	followUp, _ := json.Marshal(fake.cmds[2]["args"])
	if !strings.Contains(string(followUp), `"--resume","`+testSessionID+`"`) {
		t.Fatalf("follow-up did not resume the session: %s", followUp)
	}
	for _, c := range fake.cmds {
		args, _ := json.Marshal(c["args"])
		if strings.Contains(string(args), testClaudeToken) {
			t.Fatalf("token leaked into command args: %s", args)
		}
	}
	if env, _ := fake.cmds[0]["env"].(map[string]any); env[claude.EnvToken] != nil {
		t.Fatal("the install step does not need the token")
	}
	if env, _ := fake.cmds[1]["env"].(map[string]any); env[claude.EnvToken] != testClaudeToken {
		t.Fatal("token not passed to claude via env")
	}
}

func TestClaudeMessageValidation(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	fake.stdoutFor = fakeClaude
	h := srv.Handler()
	sb := createSandbox(t, h)
	path := "/sandboxes/" + sb.Name + "/claude/messages"

	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"unknown sandbox": {"/sandboxes/nope/claude/messages", `{"prompt":"hi"}`, http.StatusNotFound},
		"bad json":        {path, `{`, http.StatusBadRequest},
		"no prompt":       {path, `{}`, http.StatusBadRequest},
		"blank prompt":    {path, `{"prompt":" \n"}`, http.StatusBadRequest},
		"long prompt":     {path, `{"prompt":"` + strings.Repeat("a", claude.MaxPromptBytes+1) + `"}`, http.StatusBadRequest},
		"bad session":     {path, `{"prompt":"hi","session_id":"--model opus"}`, http.StatusBadRequest},
	} {
		if rec := do(t, h, http.MethodPost, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rec.Code, tc.want, rec.Body)
		}
	}
	if len(fake.cmds) != 0 {
		t.Fatalf("invalid requests ran %d commands", len(fake.cmds))
	}
}

func TestClaudeMessageWithoutToken(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, "", claude.ErrNoToken)
	h := srv.Handler()
	sb := createSandbox(t, h)

	rec := do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/claude/messages", `{"prompt":"hi"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "mosso auth claude") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(fake.cmds) != 0 {
		t.Fatal("nothing should run in the sandbox without a token")
	}
}

func TestClaudeMessageRejectsConcurrentTurns(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	h := srv.Handler()
	sb := createSandbox(t, h)
	path := "/sandboxes/" + sb.Name + "/claude/messages"

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fake.stdoutFor = func(cmd map[string]any) string {
		once.Do(func() { close(started) })
		<-release
		return fakeClaude(cmd)
	}
	// The fake serializes requests and is now blocked inside the first
	// turn, so the second request must be rejected before reaching it.
	done := make(chan int)
	go func() { done <- do(t, h, http.MethodPost, path, `{"prompt":"first"}`).Code }()
	<-started

	srv.mu.Lock()
	busy := srv.busy[sb.Name]
	srv.mu.Unlock()
	if !busy {
		t.Fatal("sandbox should be marked busy during a turn")
	}
	if rec := do(t, h, http.MethodPost, path, `{"prompt":"second"}`); rec.Code != http.StatusConflict {
		t.Fatalf("concurrent status = %d, want 409: %s", rec.Code, rec.Body)
	}
	close(release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("first status = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first turn did not finish")
	}
}

func TestClaudeAuthStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/claude/auth", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), testClaudeToken) ||
		!strings.Contains(rec.Body.String(), `"configured":true`) || !strings.Contains(rec.Body.String(), `"source":"keychain"`) {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	withToken(srv, "", claude.ErrNoToken)
	rec = do(t, srv.Handler(), http.MethodGet, "/claude/auth", "")
	if !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("unexpected body: %s", rec.Body)
	}
}
