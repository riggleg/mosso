package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

const (
	testToken   = "sk-ant-oat01-worker-test-token"
	testSession = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

// fakeClaude is a stand-in for the claude binary. It prints stream-json:
// the session ID (the --resume one if given), a tool call, then, after a
// pause when the prompt says "slow", the result echoing the prompt. It
// appends its arguments to $MOSSO_TEST_ARGS and leaks the token into a text
// block so redaction can be checked.
const fakeClaude = `#!/bin/bash
prompt=$(cat)
printf '%s\n' "$MOSSO_TEST_ARGV_MARK $*" >/dev/null
sid="` + testSession + `"
while [ $# -gt 0 ]; do
  if [ "$1" = --resume ]; then sid=$2; fi
  shift
done
echo "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$sid\",\"model\":\"claude-test\"}"
echo "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"token is $CLAUDE_CODE_OAUTH_TOKEN in $PWD\"}]}}"
echo "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Bash\",\"input\":{\"command\":\"ls\"}}]}}"
echo "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_1\",\"content\":\"README.md\"}]}}"
case "$prompt" in *slow*) sleep 30;; esac
case "$prompt" in *crash*) echo "boom" >&2; exit 3;; esac
case "$prompt" in *noisy*) head -c 300000 /dev/zero | tr '\0' 'e' >&2; echo >&2; echo " the end" >&2; exit 4;; esac
printf '{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"you said %s","session_id":"%s"}\n' "$prompt" "$sid"
`

// writeFakeClaude installs the fake binary, recording each run's arguments
// in a file.
func writeFakeClaude(t *testing.T) (path, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := strings.Replace(fakeClaude, `printf '%s\n' "$MOSSO_TEST_ARGV_MARK $*" >/dev/null`, `printf '%s\n' "$*" >> "`+argsFile+`"`, 1)
	path = filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

func newTestWorker(t *testing.T) (*Server, *httptest.Server, string, string) {
	t.Helper()
	claudePath, argsFile := writeFakeClaude(t)
	state := t.TempDir()
	l, err := OpenLog(filepath.Join(state, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	w := New(l, claudePath)
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(ts.Close)
	return w, ts, state, argsFile
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func getStatus(t *testing.T, base string) Status {
	t.Helper()
	resp, err := http.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func getEvents(t *testing.T, base string, after int64) []Event {
	t.Helper()
	resp, err := http.Get(base + "/events?after=" + itoa(after))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []Event
	dec := json.NewDecoder(resp.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func types(events []Event) string {
	var parts []string
	for _, e := range events {
		if e.Event != nil {
			parts = append(parts, e.Type+":"+e.Event.Type)
		} else {
			parts = append(parts, e.Type)
		}
	}
	return strings.Join(parts, " ")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMessageRunsAsynchronouslyAndRecordsIncrementally(t *testing.T) {
	w, ts, state, argsFile := newTestWorker(t)
	dir := t.TempDir()
	msg := Message{Text: "do it slow", Prompt: "do it slow (full prompt)", Dir: dir, Token: testToken, SystemPrompt: "be brief"}
	start := time.Now()
	resp := post(t, ts.URL+"/messages", msg)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("POST /messages waited for Claude instead of returning at once")
	}
	var acc Accepted
	_ = json.NewDecoder(resp.Body).Decode(&acc)
	if acc.Seq != 1 || !strings.HasPrefix(acc.TaskID, "task_") {
		t.Fatalf("accepted = %+v", acc)
	}

	// While Claude is still running (the fake sleeps), the events so far
	// are already on disk and served.
	waitFor(t, func() bool { return len(getEvents(t, ts.URL, 0)) >= 5 })
	if st := getStatus(t, ts.URL); st.State != "running" || st.TaskID != acc.TaskID || st.SessionID != testSession {
		t.Fatalf("status while running = %+v", st)
	}
	events := getEvents(t, ts.URL, 0)
	if got := types(events); got != "user session claude:text claude:tool_use claude:tool_result" {
		t.Fatalf("events = %s", got)
	}
	if events[0].Text != "do it slow" || events[0].TaskID != acc.TaskID || events[1].SessionID != testSession {
		t.Fatalf("events = %+v", events[:2])
	}
	if text := events[2].Event.Text; strings.Contains(text, testToken) || !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, dir) {
		t.Fatalf("text event = %q: token not redacted or wrong cwd", text)
	}
	raw, _ := os.ReadFile(filepath.Join(state, "events.jsonl"))
	if strings.Count(string(raw), "\n") != 5 || strings.Contains(string(raw), testToken) {
		t.Fatalf("log file has %d lines, token leaked=%t", strings.Count(string(raw), "\n"), strings.Contains(string(raw), testToken))
	}
	// A second message is refused while one runs.
	if resp := post(t, ts.URL+"/messages", msg); resp.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent status = %d", resp.StatusCode)
	}
	// Stopping ends the task with a "stopped" event.
	if resp := post(t, ts.URL+"/stop", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
	w.Wait()
	events = getEvents(t, ts.URL, 5)
	if len(events) != 1 || events[0].Type != EventStopped || events[0].Seq != 6 {
		t.Fatalf("after stop: %+v", events)
	}
	if st := getStatus(t, ts.URL); st.State != "idle" || st.Seq != 6 {
		t.Fatalf("status after stop = %+v", st)
	}
	if resp := post(t, ts.URL+"/stop", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stop when idle = %d", resp.StatusCode)
	}

	// A follow-up resumes the session and finishes with a result.
	msg = Message{Text: "and again", Prompt: "and again", Dir: dir, Token: testToken, SessionID: testSession}
	if resp := post(t, ts.URL+"/messages", msg); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("follow-up status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return getStatus(t, ts.URL).State == "idle" })
	events = getEvents(t, ts.URL, 6)
	if got := types(events); got != "user session claude:text claude:tool_use claude:tool_result result" {
		t.Fatalf("follow-up events = %s", got)
	}
	last := events[len(events)-1]
	if last.Result == nil || last.Result.Text != "you said and again" || last.SessionID != testSession || last.Result.NumTurns != 2 {
		t.Fatalf("result = %+v", last)
	}
	args, _ := os.ReadFile(argsFile)
	runs := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(runs) != 2 || strings.Contains(runs[0], "--resume") || !strings.Contains(runs[0], "--append-system-prompt be brief") ||
		!strings.Contains(runs[1], "--resume "+testSession) || !strings.Contains(runs[1], "--permission-mode bypassPermissions") {
		t.Fatalf("claude runs = %q", runs)
	}
	for _, r := range runs {
		if strings.Contains(r, testToken) || strings.Contains(r, "and again") {
			t.Fatalf("token or prompt on the command line: %q", r)
		}
	}
}

func TestClaudeFailureIsRecordedAsError(t *testing.T) {
	_, ts, _, _ := newTestWorker(t)
	post(t, ts.URL+"/messages", Message{Text: "crash", Prompt: "crash please", Dir: t.TempDir(), Token: testToken})
	waitFor(t, func() bool { return getStatus(t, ts.URL).State == "idle" })
	events := getEvents(t, ts.URL, 0)
	last := events[len(events)-1]
	if last.Type != EventError || !strings.Contains(last.Text, "code 3") || !strings.Contains(last.Text, "boom") {
		t.Fatalf("last = %+v", last)
	}
}

// TestLargeStderrDoesNotBreakClaude: a verbose Claude Code (more stderr
// than the worker keeps) must not get a broken pipe; the error keeps the
// end of stderr.
func TestLargeStderrDoesNotBreakClaude(t *testing.T) {
	_, ts, _, _ := newTestWorker(t)
	post(t, ts.URL+"/messages", Message{Text: "noisy", Prompt: "be noisy", Dir: t.TempDir(), Token: testToken})
	waitFor(t, func() bool { return getStatus(t, ts.URL).State == "idle" })
	events := getEvents(t, ts.URL, 0)
	last := events[len(events)-1]
	if last.Type != EventError || !strings.Contains(last.Text, "code 4") || !strings.Contains(last.Text, "the end") {
		t.Fatalf("last = %+v", last)
	}
	if strings.Contains(last.Text, "broken pipe") || strings.Contains(last.Text, "short write") {
		t.Fatalf("stderr limit broke the process: %s", last.Text)
	}
}

// TestStatusIsIdleOnlyAfterTheTerminalEvent: once /status says idle, the
// log already holds the task's terminal event, so a next message cannot
// interleave with it.
func TestStatusIsIdleOnlyAfterTheTerminalEvent(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	for i := 0; i < 5; i++ {
		post(t, ts.URL+"/messages", Message{Text: "hi", Prompt: "hi", Dir: t.TempDir(), Token: testToken})
		for {
			st := getStatus(t, ts.URL)
			if st.State == "idle" {
				if w.log.OpenTask() != "" {
					t.Fatal("idle with an open task")
				}
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	events := getEvents(t, ts.URL, 0)
	open := ""
	for _, e := range events {
		switch {
		case e.Type == EventUser && open != "":
			t.Fatalf("task %s started before %s ended", e.TaskID, open)
		case e.Type == EventUser:
			open = e.TaskID
		case e.Terminal():
			open = ""
		}
	}
}

func TestMessageValidation(t *testing.T) {
	_, ts, _, _ := newTestWorker(t)
	for name, m := range map[string]Message{
		"no prompt":    {Text: "x", Dir: "/tmp", Token: testToken},
		"relative dir": {Prompt: "x", Dir: "tmp", Token: testToken},
		"bad session":  {Prompt: "x", Dir: "/tmp", Token: testToken, SessionID: "--model"},
		"no token":     {Prompt: "x", Dir: "/tmp"},
	} {
		if resp := post(t, ts.URL+"/messages", m); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d", name, resp.StatusCode)
		}
	}
	if n := getStatus(t, ts.URL).Seq; n != 0 {
		t.Fatalf("invalid messages recorded %d events", n)
	}
}

func TestFollowStreamsNewEvents(t *testing.T) {
	_, ts, _, _ := newTestWorker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?after=0&follow=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan Event, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				lines <- e
			}
		}
		close(lines)
	}()
	post(t, ts.URL+"/messages", Message{Text: "hi", Prompt: "hi", Dir: t.TempDir(), Token: testToken})
	var got []Event
	for e := range lines {
		got = append(got, e)
		if e.Type == EventResult {
			break
		}
	}
	if got[0].Seq != 1 || got[len(got)-1].Seq != int64(len(got)) {
		t.Fatalf("sequence numbers are not consecutive: %s", types(got))
	}
	cancel()
	for range lines {
	}
}

func TestRestartMarksRunningTaskInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Append(Event{Type: EventUser, TaskID: "task_a", Text: "first"})
	_, _ = l.Append(Event{Type: EventSession, TaskID: "task_a", SessionID: testSession})
	_, _ = l.Append(Event{Type: EventResult, TaskID: "task_a", Result: &Result{Text: "done"}})
	_, _ = l.Append(Event{Type: EventUser, TaskID: "task_b", Text: "second"})
	_, _ = l.Append(Event{Type: EventClaude, TaskID: "task_b", Event: &claude.Event{Type: "text", Text: "working"}})
	l.Close()

	// The worker restarts (the sandbox resumed): the open task is closed.
	l, err = OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	w := New(l, "/nonexistent/claude")
	if last, _ := l.Last(); last.Type != EventInterrupted || last.TaskID != "task_b" || last.Seq != 6 || !strings.HasPrefix(last.Text, "Interrupted") {
		t.Fatalf("last event = %+v", last)
	}
	if st := w.Status(); st.State != "idle" || st.Seq != 6 || st.SessionID != testSession {
		t.Fatalf("status = %+v", st)
	}
	// Restarting again does not add another one.
	l.Close()
	l, _ = OpenLog(path)
	New(l, "")
	if l.Seq() != 6 {
		t.Fatalf("seq after second restart = %d", l.Seq())
	}
}

func TestLogRecoversAndDiscardsPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l.Append(Event{Type: EventNotice, Text: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	// A crash mid-write leaves a partial line.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":4,"type":"noti`)
	f.Close()

	l, err = OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Seq() != 3 {
		t.Fatalf("seq = %d, want 3", l.Seq())
	}
	e, _ := l.Append(Event{Type: EventNotice, Text: "after"})
	if e.Seq != 4 {
		t.Fatalf("next seq = %d", e.Seq)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "\n") != 4 || strings.Contains(string(raw), `"noti`+"\n") {
		t.Fatalf("file:\n%s", raw)
	}
	if got := l.After(2); len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("After(2) = %+v", got)
	}
}

func TestLogBoundsSize(t *testing.T) {
	l, err := OpenLog(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	huge := json.RawMessage(`"` + strings.Repeat("x", maxInputBytes+10) + `"`)
	e, _ := l.Append(Event{Type: EventClaude, Event: &claude.Event{Type: "tool_use", Name: "Write", Input: huge}})
	if !strings.Contains(string(e.Event.Input), "omitted") {
		t.Fatalf("oversized input kept: %d bytes", len(e.Event.Input))
	}
	big := strings.Repeat("y", 1<<20)
	for i := 0; i < maxLogBytes/len(big)+3; i++ {
		if _, err := l.Append(Event{Type: EventClaude, Event: &claude.Event{Type: "tool_result", ToolUseID: "t", Content: big}}); err != nil {
			t.Fatal(err)
		}
	}
	last, _ := l.Last()
	if last.Event.Content != Omitted || l.bytes > maxLogBytes+2<<20 {
		t.Fatalf("log not bounded: content=%d bytes, log=%d bytes", len(last.Event.Content), l.bytes)
	}
	notices := 0
	for _, e := range l.After(0) {
		if e.Type == EventNotice {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("notices = %d, want 1", notices)
	}
	// Sequence numbers stay consecutive around the notice.
	for i, e := range l.After(0) {
		if e.Seq != int64(i)+1 {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func TestListenAndServeRefusesNonLoopback(t *testing.T) {
	l, _ := OpenLog(filepath.Join(t.TempDir(), "events.jsonl"))
	defer l.Close()
	if err := New(l, "").ListenAndServe(context.Background(), "0.0.0.0:7777"); err == nil {
		t.Fatal("expected an error")
	}
}
