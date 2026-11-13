package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/riggleg/mosso/internal/vercel"
)

const (
	testToken   = "sk-ant-oat01-test-token-value"
	testSession = "5e3b5aa0-bc5b-4e7c-9744-0953cbbcd962"
)

func TestParseStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := ParseStream(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if turn.SessionID != testSession || turn.Model != "claude-sonnet-5-5" || turn.Result != "There are two files." ||
		turn.IsError || turn.Subtype != "success" || turn.NumTurns != 3 || turn.DurationMS != 4211 || turn.TotalCostUSD != 0.0123 {
		t.Fatalf("unexpected turn: %+v", turn)
	}
	want := []Event{
		{Type: "text", Text: "Let me check."},
		{Type: "tool_use", ToolUseID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls","description":"List files"}`)},
		{Type: "tool_result", ToolUseID: "toolu_1", Content: "README.md\ngo.mod"},
		{Type: "tool_use", ToolUseID: "toolu_2", Name: "Read", Input: json.RawMessage(`{"file_path":"/vercel/sandbox/missing"}`), ParentToolUseID: "toolu_task"},
		{Type: "tool_result", ToolUseID: "toolu_2", Content: "File does not exist.\n[image]", IsError: true, ParentToolUseID: "toolu_task"},
		{Type: "text", Text: "There are two files."},
	}
	got, _ := json.Marshal(turn.Events)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("events:\n got %s\nwant %s", got, exp)
	}
}

func TestParseStreamWithoutResult(t *testing.T) {
	_, err := ParseStream(`{"type":"system","subtype":"init","session_id":"x"}` + "\n")
	if err == nil {
		t.Fatal("expected an error for a stream without a result")
	}
}

func TestParseStreamTruncatesLargeToolResults(t *testing.T) {
	big := strings.Repeat("é", maxToolResultBytes)
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t", "content": big},
	}}})
	turn, err := ParseStream(string(line) + "\n" + `{"type":"result","result":"ok"}`)
	if err != nil {
		t.Fatal(err)
	}
	c := turn.Events[0].Content
	if len(c) > maxToolResultBytes+64 || !strings.HasSuffix(c, "[mosso: truncated]") || !json.Valid([]byte(fmt.Sprintf("%q", c))) {
		t.Fatalf("tool result not truncated cleanly: %d bytes", len(c))
	}
}

func TestValidSessionID(t *testing.T) {
	for id, want := range map[string]bool{
		testSession:                      true,
		"":                               false,
		"--dangerously-skip-permissions": false,
		testSession + "x":                false,
		"../../etc/passwd":               false,
	} {
		if got := ValidSessionID(id); got != want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", id, got, want)
		}
	}
}

// fakeSandbox is a minimal Vercel Sandbox API that runs one scripted command.
type fakeSandbox struct {
	mu       sync.Mutex
	bodies   []map[string]any
	exitCode int
	stdout   string
	stderr   string
	// chunks, if set, replaces stdout with several log entries.
	chunks []string
}

func (f *fakeSandbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cmd"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"command":{"id":"cmd_1","exitCode":%d}}`, f.exitCode)
	case strings.HasSuffix(r.URL.Path, "/cmd/cmd_1/logs"):
		enc := json.NewEncoder(w)
		for _, c := range f.chunks {
			_ = enc.Encode(map[string]string{"stream": "stdout", "data": c})
		}
		_ = enc.Encode(map[string]string{"stream": "stdout", "data": f.stdout})
		_ = enc.Encode(map[string]string{"stream": "stderr", "data": f.stderr})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeSandbox(t *testing.T, f *fakeSandbox) *vercel.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return vercel.NewClient(vercel.Config{Token: "vercel-tok", TeamID: "team", ProjectID: "prj", BaseURL: srv.URL})
}

func TestSendResumesAndPassesTokenOnlyInEnv(t *testing.T) {
	raw, _ := os.ReadFile("testdata/stream.jsonl")
	fake := &fakeSandbox{stdout: string(raw)}
	vc := newFakeSandbox(t, fake)

	turn, err := Send(context.Background(), vc, "sbx_1", Token(testToken), Request{Prompt: "--help; $(rm -rf /)", SessionID: testSession})
	if err != nil {
		t.Fatal(err)
	}
	if turn.SessionID != testSession || turn.Result != "There are two files." {
		t.Fatalf("unexpected turn: %+v", turn)
	}

	body := fake.bodies[0]
	env, _ := body["env"].(map[string]any)
	if env[EnvToken] != testToken {
		t.Fatalf("token not passed in env: %v", env)
	}
	if env["MOSSO_PROMPT"] != "--help; $(rm -rf /)" {
		t.Fatalf("prompt not passed verbatim in env: %v", env["MOSSO_PROMPT"])
	}
	if body["cwd"] != WorkDir || body["command"] != "bash" {
		t.Fatalf("unexpected command: %v", body)
	}
	args, _ := json.Marshal(body["args"])
	if strings.Contains(string(args), testToken) || strings.Contains(string(args), "rm -rf") {
		t.Fatalf("token or prompt leaked into args: %s", args)
	}
	if !strings.HasSuffix(string(args), `"claude","--resume","`+testSession+`"]`) {
		t.Fatalf("args do not resume the session: %s", args)
	}
	for _, flag := range []string{"-p", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(string(args), flag) {
			t.Errorf("args missing %q: %s", flag, args)
		}
	}
}

func TestSendNewConversationHasNoResume(t *testing.T) {
	fake := &fakeSandbox{stdout: `{"type":"result","result":"hi","session_id":"` + testSession + `"}`}
	vc := newFakeSandbox(t, fake)
	if _, err := Send(context.Background(), vc, "sbx_1", Token(testToken), Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if args, _ := json.Marshal(fake.bodies[0]["args"]); strings.Contains(string(args), "--resume") {
		t.Fatalf("new conversation should not resume: %s", args)
	}
}

func TestSendRedactsToken(t *testing.T) {
	fake := &fakeSandbox{
		exitCode: 1,
		stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"token is ` + testToken + `"}]}}` + "\n" +
			`{"type":"result","is_error":true,"result":"leaked ` + testToken + `"}`,
	}
	vc := newFakeSandbox(t, fake)
	turn, err := Send(context.Background(), vc, "sbx_1", Token(testToken), Request{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(turn)
	if strings.Contains(string(out), testToken) || !strings.Contains(string(out), "[REDACTED]") || !turn.IsError {
		t.Fatalf("token not redacted: %s", out)
	}

	// Errors carry stderr; that must be redacted too.
	fake.stdout, fake.stderr = "", "auth failed for "+testToken
	_, err = Send(context.Background(), vc, "sbx_1", Token(testToken), Request{Prompt: "hi"})
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendStreamsEvents(t *testing.T) {
	raw, _ := os.ReadFile("testdata/stream.jsonl")
	secret := `{"type":"assistant","message":{"content":[{"type":"text","text":"see ` + testToken + `"}]}}` + "\n"
	all := secret + string(raw)
	// Split the output into chunks that cut lines (and the token) in half.
	var chunks []string
	for i := 0; i < len(all); i += 37 {
		chunks = append(chunks, all[i:min(i+37, len(all))])
	}
	vc := newFakeSandbox(t, &fakeSandbox{chunks: chunks})

	var sessions []string
	var events []Event
	turn, err := Send(context.Background(), vc, "sbx_1", Token(testToken), Request{
		Prompt:    "hi",
		OnSession: func(id string) { sessions = append(sessions, id) },
		OnEvent:   func(e Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0] != testSession {
		t.Fatalf("OnSession calls = %v", sessions)
	}
	got, _ := json.Marshal(events)
	want, _ := json.Marshal(turn.Events)
	if string(got) != string(want) || len(events) != 7 {
		t.Fatalf("streamed events %s\nwant %s", got, want)
	}
	if strings.Contains(string(got), testToken) || events[0].Text != "see [REDACTED]" {
		t.Fatalf("streamed events are not redacted: %s", got)
	}
}

func TestSendValidatesInput(t *testing.T) {
	vc := newFakeSandbox(t, &fakeSandbox{})
	for name, req := range map[string]Request{
		"empty prompt":   {Prompt: "  "},
		"huge prompt":    {Prompt: strings.Repeat("a", MaxPromptBytes+1)},
		"bad session id": {Prompt: "hi", SessionID: "--model=opus"},
	} {
		if _, err := Send(context.Background(), vc, "sbx_1", Token(testToken), req); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInstall(t *testing.T) {
	fake := &fakeSandbox{stdout: "2.1.311 (Claude Code)\n"}
	vc := newFakeSandbox(t, fake)
	v, err := Install(context.Background(), vc, "sbx_1")
	if err != nil || v != "2.1.311 (Claude Code)" {
		t.Fatalf("Install = %q, %v", v, err)
	}
	if args, _ := json.Marshal(fake.bodies[0]["args"]); !strings.Contains(string(args), "https://claude.ai/install.sh") {
		t.Fatalf("install does not use the official installer: %s", args)
	}

	fake.exitCode, fake.stderr = 22, "curl: (22) 403"
	if _, err := Install(context.Background(), vc, "sbx_1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenNeverFormats(t *testing.T) {
	tok := Token(testToken)
	js, _ := json.Marshal(map[string]any{"t": tok})
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %s %q %#v %+v", tok, tok, tok, tok, tok), string(js)} {
		if strings.Contains(s, testToken) {
			t.Fatalf("token leaked through formatting: %s", s)
		}
	}
	if tok.Reveal() != testToken {
		t.Fatal("Reveal must return the raw token")
	}
}

func stubKeychain(t *testing.T, value string, err error) {
	t.Helper()
	orig := readKeychain
	readKeychain = func(context.Context) (string, error) { return value, err }
	t.Cleanup(func() { readKeychain = orig })
}

func TestLoadToken(t *testing.T) {
	ctx := context.Background()

	stubKeychain(t, testToken, nil)
	t.Setenv(EnvToken, "sk-ant-oat01-from-env")
	if tok, src, err := LoadToken(ctx); err != nil || tok.Reveal() != testToken || src != SourceKeychain {
		t.Fatalf("keychain should win: src=%q err=%v", src, err)
	}

	stubKeychain(t, "", nil)
	if tok, src, err := LoadToken(ctx); err != nil || tok.Reveal() != "sk-ant-oat01-from-env" || src != SourceEnv {
		t.Fatalf("env fallback: src=%q err=%v", src, err)
	}

	t.Setenv(EnvToken, "")
	if _, _, err := LoadToken(ctx); err != ErrNoToken {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}

	t.Setenv(EnvToken, "sk-ant-api03-secret-api-key")
	_, _, err := LoadToken(ctx)
	if err == nil || strings.Contains(err.Error(), "secret-api-key") || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("API keys must be rejected without echoing them: %v", err)
	}

	t.Setenv(EnvToken, "sk-ant-oat01-abc def")
	if _, _, err := LoadToken(ctx); err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("malformed token must be rejected without echoing it: %v", err)
	}

	stubKeychain(t, "", io.ErrUnexpectedEOF)
	if _, _, err := LoadToken(ctx); err == nil {
		t.Fatal("keychain errors must be reported, not silently skipped")
	}
}

func TestParseStreamBoundsKeptEvents(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + strings.Repeat("x", 1<<20) + `"}]}}`
	n := maxEventBytes/len(line) + 3
	out := strings.Repeat(line+"\n", n) + `{"type":"result","result":"done","session_id":"` + testSession + `"}`
	var streamed int
	p := &parser{turn: Turn{Events: []Event{}}, onEvent: func(Event) { streamed++ }}
	for _, l := range strings.Split(out, "\n") {
		p.line(l)
	}
	turn, err := p.finish()
	if err != nil {
		t.Fatal(err)
	}
	if streamed != n || len(turn.Events) >= n || len(turn.Events) == 0 || turn.Result != "done" {
		t.Fatalf("streamed %d, kept %d of %d events, result %q", streamed, len(turn.Events), n, turn.Result)
	}
}
