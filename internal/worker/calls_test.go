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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// blockingRunner keeps a task running until release is closed.
func blockingRunner(release chan struct{}) Runner {
	return func(ctx context.Context, msg Message, emit func(Event)) (*claude.Turn, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &claude.Turn{SessionID: testSession, Result: "done"}, nil
	}
}

// startTask submits a message to a worker whose runner blocks.
func startTask(t *testing.T, w *Server, base string) chan struct{} {
	t.Helper()
	release := make(chan struct{})
	w.SetRunner(blockingRunner(release))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		w.Wait()
	})
	resp := post(t, base+"/messages", Message{Text: "hi", Prompt: "hi", Dir: t.TempDir(), Token: testToken})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("message status = %d", resp.StatusCode)
	}
	return release
}

func postStatus(t *testing.T, url string, body any) (int, string) {
	t.Helper()
	resp := post(t, url, body)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestToolCallRoundTrip is the relay: Claude's call is recorded and listed
// as pending, the server claims it once and answers, and Claude gets the
// answer.
func TestToolCallRoundTrip(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	startTask(t, w, ts.URL)

	type answer struct {
		ans CallAnswer
		err error
	}
	got := make(chan answer, 1)
	go func() {
		ans, err := CallTool(context.Background(), ts.URL, ToolCreatePR, json.RawMessage(`{"title":"Fix","base":"uat"}`))
		got <- answer{ans, err}
	}()
	var id string
	waitFor(t, func() bool {
		st := getStatus(t, ts.URL)
		if len(st.PendingCalls) == 1 {
			id = st.PendingCalls[0]
		}
		return id != ""
	})

	// The call is in the log, with its task, for a server that follows it.
	events := getEvents(t, ts.URL, 0)
	last := events[len(events)-1]
	if last.Type != EventToolCall || last.Call == nil || last.Call.ID != id || last.Call.Name != ToolCreatePR ||
		string(last.Call.Arguments) != `{"title":"Fix","base":"uat"}` || last.TaskID != events[0].TaskID {
		t.Fatalf("unexpected event: %+v", last)
	}

	// Answering before claiming is refused; claiming works once.
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/result", CallAnswer{Error: "x"}); code != http.StatusConflict {
		t.Fatalf("result before start: %d", code)
	}
	code, body := postStatus(t, ts.URL+"/tools/calls/"+id+"/start", nil)
	var claimed ToolCall
	if code != http.StatusOK || json.Unmarshal([]byte(body), &claimed) != nil || claimed.Name != ToolCreatePR || claimed.ID != id {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/start", nil); code != http.StatusConflict {
		t.Fatalf("second start: %d", code)
	}
	if st := getStatus(t, ts.URL); len(st.PendingCalls) != 0 {
		t.Fatalf("a claimed call is not pending: %v", st.PendingCalls)
	}
	for _, bad := range []CallAnswer{{}, {Result: json.RawMessage(`{}`), Error: "both"}} {
		if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/result", bad); code != http.StatusBadRequest {
			t.Fatalf("result %+v: %d", bad, code)
		}
	}
	if code, body := postStatus(t, ts.URL+"/tools/calls/"+id+"/result", CallAnswer{Result: json.RawMessage(`{"number":12}`)}); code != http.StatusNoContent {
		t.Fatalf("result: %d %s", code, body)
	}
	a := <-got
	if a.err != nil || string(a.ans.Result) != `{"number":12}` || a.ans.Error != "" || a.ans.ID != id {
		t.Fatalf("answer: %+v %v", a.ans, a.err)
	}
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/result", CallAnswer{Error: "again"}); code != http.StatusConflict {
		t.Fatalf("second result: %d", code)
	}
	events = getEvents(t, ts.URL, last.Seq)
	if len(events) != 1 || events[0].Type != EventToolResult || events[0].Call.ID != id || string(events[0].Call.Result) != `{"number":12}` {
		t.Fatalf("result events: %+v", events)
	}
}

func TestToolCallRules(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	// Only during a task.
	if _, err := CallTool(context.Background(), ts.URL, ToolPush, nil); err == nil || !strings.Contains(err.Error(), "while Claude works") {
		t.Fatalf("err = %v", err)
	}
	startTask(t, w, ts.URL)
	if _, err := CallTool(context.Background(), ts.URL, "rm_rf", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("err = %v", err)
	}
	if code, _ := postStatus(t, ts.URL+"/tools/calls/call_nope/start", nil); code != http.StatusConflict {
		t.Fatalf("start unknown: %d", code)
	}
	// A message's calls are bounded.
	w.mu.Lock()
	w.cur.calls = MaxCallsPerTask
	w.mu.Unlock()
	if _, err := CallTool(context.Background(), ts.URL, ToolPush, nil); err == nil || !strings.Contains(err.Error(), "the most Mosso allows") {
		t.Fatalf("err = %v", err)
	}
}

// TestUnclaimedToolCallIsWithdrawn: while Mosso is away, Claude is told so
// instead of waiting forever, and Mosso cannot run the call later.
func TestUnclaimedToolCallIsWithdrawn(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	w.SetCallTimeouts(100*time.Millisecond, 5*time.Second)
	startTask(t, w, ts.URL)
	ans, err := CallTool(context.Background(), ts.URL, ToolPush, nil)
	if err != nil || !strings.Contains(ans.Error, "did not pick up") || !strings.Contains(ans.Error, "nothing was done") {
		t.Fatalf("answer: %+v %v", ans, err)
	}
	events := getEvents(t, ts.URL, 0)
	last := events[len(events)-1]
	if last.Type != EventToolResult || last.Call.ID != ans.ID {
		t.Fatalf("last event: %+v", last)
	}
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+ans.ID+"/start", nil); code != http.StatusConflict {
		t.Fatalf("start after withdrawal: %d", code)
	}
}

// TestClaimedToolCallIsNotWithdrawn: once Mosso runs a call, the pickup
// timeout no longer applies, and the call times out with an unknown
// outcome only at the overall limit.
func TestClaimedToolCallIsNotWithdrawn(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	w.SetCallTimeouts(100*time.Millisecond, 600*time.Millisecond)
	startTask(t, w, ts.URL)
	done := make(chan error, 1)
	go func() {
		_, err := CallTool(context.Background(), ts.URL, ToolPush, nil)
		done <- err
	}()
	var id string
	waitFor(t, func() bool {
		if st := getStatus(t, ts.URL); len(st.PendingCalls) == 1 {
			id = st.PendingCalls[0]
		}
		return id != ""
	})
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/start", nil); code != http.StatusOK {
		t.Fatalf("start: %d", code)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("err = %v", err)
	}
	// Mosso's late answer is still recorded.
	if code, _ := postStatus(t, ts.URL+"/tools/calls/"+id+"/result", CallAnswer{Result: json.RawMessage(`{}`)}); code != http.StatusNoContent {
		t.Fatalf("late result: %d", code)
	}
}

func TestToolCallWithdrawnWhenCallerGoesAway(t *testing.T) {
	w, ts, _, _ := newTestWorker(t)
	startTask(t, w, ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = CallTool(ctx, ts.URL, ToolPush, nil) }()
	waitFor(t, func() bool { return len(getStatus(t, ts.URL).PendingCalls) == 1 })
	cancel()
	waitFor(t, func() bool {
		events := getEvents(t, ts.URL, 0)
		last := events[len(events)-1]
		return last.Type == EventToolResult && strings.Contains(last.Call.Error, "withdrawn")
	})
	if st := getStatus(t, ts.URL); len(st.PendingCalls) != 0 {
		t.Fatalf("pending = %v", st.PendingCalls)
	}
}

func TestRestartEndsOpenToolCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Append(Event{Type: EventUser, TaskID: "task_1", Text: "hi"})
	_, _ = l.Append(Event{Type: EventToolCall, TaskID: "task_1", Call: &ToolCall{ID: "call_done", Name: ToolPush}})
	_, _ = l.Append(Event{Type: EventToolResult, TaskID: "task_1", Call: &ToolCall{ID: "call_done", Result: json.RawMessage(`{}`)}})
	_, _ = l.Append(Event{Type: EventToolCall, TaskID: "task_1", Call: &ToolCall{ID: "call_open", Name: ToolCreatePR}})
	l.Close()

	l, err = OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	New(l, "/nonexistent")
	events := l.After(4)
	if len(events) != 2 || events[0].Type != EventToolResult || events[0].Call.ID != "call_open" ||
		!strings.Contains(events[0].Call.Error, "interrupted") || events[0].TaskID != "task_1" || events[1].Type != EventInterrupted {
		t.Fatalf("events after restart: %+v", events)
	}
	if open := l.OpenCalls(); len(open) != 0 {
		t.Fatalf("open calls = %v", open)
	}
}

// TestLateToolResultDoesNotReopenItsTask: Mosso may answer a call after
// Claude's task ended; a restart must not then report the task interrupted.
func TestLateToolResultDoesNotReopenItsTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Append(Event{Type: EventUser, TaskID: "task_1", Text: "hi"})
	_, _ = l.Append(Event{Type: EventToolCall, TaskID: "task_1", Call: &ToolCall{ID: "call_1", Name: ToolPush}})
	_, _ = l.Append(Event{Type: EventResult, TaskID: "task_1", Result: &Result{Text: "done"}})
	_, _ = l.Append(Event{Type: EventToolResult, TaskID: "task_1", Call: &ToolCall{ID: "call_1", Result: json.RawMessage(`{}`)}})
	l.Close()
	if l, err = OpenLog(path); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	New(l, "/nonexistent")
	if l.Seq() != 4 || l.OpenTask() != "" {
		t.Fatalf("seq = %d, open task = %q: %+v", l.Seq(), l.OpenTask(), l.After(4))
	}
}

// mcpSession runs ServeMCP over pipes against a fake worker API.
type mcpSession struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
}

func newMCPSession(t *testing.T, workerURL string) *mcpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeMCP(context.Background(), inR, outW, workerURL)
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 1<<20)
	return &mcpSession{t: t, in: inW, out: sc}
}

func (m *mcpSession) send(msg string) {
	m.t.Helper()
	if _, err := io.WriteString(m.in, msg+"\n"); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mcpSession) recv() map[string]any {
	m.t.Helper()
	if !m.out.Scan() {
		m.t.Fatalf("no response: %v", m.out.Err())
	}
	var v map[string]any
	if err := json.Unmarshal(m.out.Bytes(), &v); err != nil {
		m.t.Fatalf("%v: %s", err, m.out.Bytes())
	}
	return v
}

func TestMCPServer(t *testing.T) {
	var calls []CallRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req)
		if req.Name == ToolGetPR {
			writeJSON(w, http.StatusOK, CallAnswer{ID: "call_2", Error: "pull request #9 is not one of this workspace's pull requests"})
			return
		}
		writeJSON(w, http.StatusOK, CallAnswer{ID: "call_1", Result: json.RawMessage(`{"number":12,"url":"https://github.com/o/r/pull/12"}`)})
	}))
	defer api.Close()
	m := newMCPSession(t, api.URL)

	m.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"2"}}}`)
	init := m.recv()
	res := init["result"].(map[string]any)
	if init["id"] != float64(1) || res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "mosso" {
		t.Fatalf("initialize: %v", init)
	}
	m.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	m.send(`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`)
	list := m.recv()
	tools := list["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" || tm["description"] == "" {
			t.Fatalf("tool %v", tm)
		}
	}
	want := []string{ToolPush, ToolCreatePR, ToolListPRs, ToolGetPR, ToolUpdatePR, ToolChecks, ToolFetch}
	if list["id"] != "two" || !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}

	m.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"github_create_pr","arguments":{"title":"Fix","base":"uat"}}}`)
	call := m.recv()
	res = call["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if call["id"] != float64(3) || res["isError"] != false || !strings.Contains(text, `"number": 12`) {
		t.Fatalf("call: %v", call)
	}
	if len(calls) != 1 || calls[0].Name != ToolCreatePR || string(calls[0].Arguments) != `{"title":"Fix","base":"uat"}` {
		t.Fatalf("relayed: %+v", calls)
	}
	m.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"github_get_pr","arguments":{"number":9}}}`)
	res = m.recv()["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "not one of this workspace's") {
		t.Fatalf("error result: %v", res)
	}
	m.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	if e := m.recv()["error"].(map[string]any); e["code"] != float64(-32602) {
		t.Fatalf("unknown tool: %v", e)
	}
	m.send(`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	if e := m.recv()["error"].(map[string]any); e["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", e)
	}
	m.send(`not json`)
	if e := m.recv()["error"].(map[string]any); e["code"] != float64(-32700) {
		t.Fatalf("parse error: %v", e)
	}
	m.send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if p := m.recv(); p["id"] != float64(7) || p["error"] != nil {
		t.Fatalf("ping: %v", p)
	}
}

// TestMCPServerReportsAnUnreachableWorker: Claude gets an error it can act
// on, not a hang.
func TestMCPServerReportsAnUnreachableWorker(t *testing.T) {
	m := newMCPSession(t, "http://127.0.0.1:1")
	m.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_push","arguments":{}}}`)
	res := m.recv()["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "did not answer") {
		t.Fatalf("result: %v", res)
	}
}

func TestClaudeGetsTheMCPServer(t *testing.T) {
	w, ts, _, argsFile := newTestWorker(t)
	w.SetMCP("/vercel/sandbox/.mosso/bin/mosso-worker-abc", "mcp", "-worker", "http://127.0.0.1:7777")
	post(t, ts.URL+"/messages", Message{Text: "hi", Prompt: "hi", Dir: t.TempDir(), Token: testToken})
	waitFor(t, func() bool { return getStatus(t, ts.URL).State == "idle" && getStatus(t, ts.URL).Seq > 1 })
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if !strings.Contains(args, `--mcp-config {"mcpServers":{"mosso":{"args":["mcp","-worker","http://127.0.0.1:7777"],"command":"/vercel/sandbox/.mosso/bin/mosso-worker-abc","type":"stdio"}}}`) {
		t.Fatalf("claude args: %s", args)
	}
}
