package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	ID, Event, Data string
}

// eventStream reads a workspace's event stream from a real HTTP server.
type eventStream struct {
	t      *testing.T
	events chan sseEvent
	body   io.Closer
}

func openEvents(t *testing.T, base, name, lastID string) *eventStream {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/workspaces/"+name+"/events", nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &eventStream{t: t, events: make(chan sseEvent, 1000), body: resp.Body}
	t.Cleanup(func() { resp.Body.Close() })
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Event != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.Data = line[6:]
			}
		}
	}()
	return s
}

// next returns the next event, failing the test if none arrives in time.
func (s *eventStream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatal("event stream ended")
		}
		return ev
	case <-time.After(10 * time.Second):
		s.t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

// entry returns the next chat entry, skipping workspace status events.
func (s *eventStream) entry() chatEntry {
	s.t.Helper()
	for {
		ev := s.next()
		if ev.Event != "entry" {
			continue
		}
		var e chatEntry
		if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
			s.t.Fatal(err)
		}
		return e
	}
}

// until returns entries up to and including the first of type typ.
func (s *eventStream) until(typ string) []chatEntry {
	s.t.Helper()
	var out []chatEntry
	for {
		e := s.entry()
		out = append(out, e)
		if e.Type == typ {
			return out
		}
	}
}

func entryTypes(entries []chatEntry) string {
	var parts []string
	for _, e := range entries {
		if e.Event != nil {
			parts = append(parts, e.Type+":"+e.Event.Type)
		} else {
			parts = append(parts, e.Type)
		}
	}
	return strings.Join(parts, " ")
}

// newChatServer returns a server with a ready workspace for octo/widgets,
// served over real HTTP so event streams work.
func newChatServer(t *testing.T) (*Server, *fakeVercel, *httptest.Server, testWorkspace) {
	t.Helper()
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	noChanges := false
	fake.stdoutFor = fakeRepoSandbox(&noChanges)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.CloseStreams)
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	return srv, fake, ts, ws
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestWorkspaceChatStreamsAndContinues(t *testing.T) {
	_, fake, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")
	if ev := events.next(); ev.Event != "workspace" || !strings.Contains(ev.Data, `"status":"ready"`) {
		t.Fatalf("first event = %+v, want the ready workspace", ev)
	}

	resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"improve the readme"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	// The workspace was set up with Claude Code installed, so the turn
	// streams straight away.
	first := events.until("result")
	if got := entryTypes(first); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("entries = %s", got)
	}
	if first[0].Text != "improve the readme" || first[1].Event.Name != "Bash" || first[3].Result.Text != "you said improve the readme" {
		t.Fatalf("unexpected entries: %+v", first)
	}

	// A follow-up resumes the same Claude Code session.
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"and the tests"}`)
	second := events.until("result")
	if second[0].ID != first[len(first)-1].ID+1 {
		t.Fatalf("entry IDs are not consecutive: %d after %d", second[0].ID, first[len(first)-1].ID)
	}
	fake.mu.Lock()
	last, _ := json.Marshal(fake.cmds[len(fake.cmds)-1])
	fake.mu.Unlock()
	if !strings.Contains(string(last), `"--resume","`+testSessionID+`"`) {
		t.Fatalf("follow-up did not resume the session: %s", last)
	}

	// Reconnecting with Last-Event-ID replays only what was missed; a new
	// browser tab replays everything.
	again := openEvents(t, ts.URL, ws.Name, strconv.Itoa(first[len(first)-1].ID))
	if e := again.entry(); e.ID != second[0].ID || e.Text != "and the tests" {
		t.Fatalf("resumed stream starts at %+v", e)
	}
	fresh := openEvents(t, ts.URL, ws.Name, "")
	if e := fresh.entry(); e.ID != 0 {
		t.Fatalf("new stream starts at entry %d", e.ID)
	}
}

func TestWorkspaceChatRejectsConcurrentAndStops(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fake.mu.Lock()
	inner := fake.stdoutFor
	fake.stdoutFor = func(cmd map[string]any) string {
		once.Do(func() { close(started) })
		<-release
		return inner(cmd)
	}
	fake.mu.Unlock()
	events := openEvents(t, ts.URL, ws.Name, "")

	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"long task"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	<-started
	for _, path := range []string{"/chat", "/claude/messages", "/pull-request"} {
		if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+path, `{"prompt":"again"}`); resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s while busy: status = %d, want 409", path, resp.StatusCode)
		}
	}
	var snap testWorkspaceStatus
	_ = json.Unmarshal(do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.Bytes(), &snap)
	if snap.Status != "working" {
		t.Fatalf("status = %q, want working", snap.Status)
	}

	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat/stop", `{}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
	close(release) // let the fake answer the stopped request and the kill
	entries := events.until("notice")
	if last := entries[len(entries)-1]; !strings.HasPrefix(last.Text, "Stopped") {
		t.Fatalf("entries = %+v, want a Stopped notice", entries)
	}
	waitFor(t, func() bool {
		var snap testWorkspaceStatus
		_ = json.Unmarshal(do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.Bytes(), &snap)
		return snap.Status == "ready"
	})
	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat/stop", `{}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stop when idle: status = %d, want 409", resp.StatusCode)
	}
}

type testWorkspaceStatus struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkspaceChatErrorsAreRecorded(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")

	// Claude Code runs but reports an error, e.g. an expired token.
	fake.mu.Lock()
	fake.stdoutFor = func(map[string]any) string {
		return `{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login","session_id":"` + testSessionID + `"}` + "\n"
	}
	fake.mu.Unlock()
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"hi"}`)
	if r := events.until("result"); !r[len(r)-1].Result.IsError {
		t.Fatalf("result = %+v, want is_error", r[len(r)-1].Result)
	}
	var snap testWorkspaceStatus
	_ = json.Unmarshal(do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.Bytes(), &snap)
	if snap.Status != "error" || !strings.Contains(snap.Error, "Invalid API key") {
		t.Fatalf("workspace = %+v, want the error", snap)
	}

	// The sandbox fails (for example, it was stopped).
	fake.mu.Lock()
	fake.failCmd = true
	fake.mu.Unlock()
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"hi"}`)
	if e := events.until("error"); !strings.Contains(e[len(e)-1].Text, "cmd failed") {
		t.Fatalf("error entry = %+v", e[len(e)-1])
	}

	// Without a token nothing runs and the request says why.
	withToken(srv, "", claude.ErrNoToken)
	resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"hi"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status without a token = %d", resp.StatusCode)
	}
}

func TestWorkspaceEventsCloseWhenDestroyed(t *testing.T) {
	_, fake, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")
	events.next() // workspace
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/workspaces/"+ws.Name, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %v %v", resp, err)
	}
	for {
		if ev := events.next(); ev.Event == "closed" {
			break
		}
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted = %v", fake.deleted)
	}
	resp, _ := http.Get(ts.URL + "/workspaces/" + ws.Name + "/events")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("events after delete: status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWorkspacePullRequestIsRecorded(t *testing.T) {
	_, _, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")
	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/pull-request", `{"title":"Improve README"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	entries := events.until("pull_request")
	pr := entries[len(entries)-1].PullRequest
	if !pr.Created || pr.Number != 7 || entries[0].Type != "notice" {
		t.Fatalf("entries = %s, pull request = %+v", entryTypes(entries), pr)
	}
	postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/pull-request", `{"title":"More"}`)
	entries = events.until("pull_request")
	if entries[len(entries)-1].PullRequest.Created {
		t.Fatal("second publish should update the pull request")
	}
	for {
		ev := events.next()
		if ev.Event == "workspace" && strings.Contains(ev.Data, `"status":"ready"`) {
			if !strings.Contains(ev.Data, `"pull_request":{"number":7`) {
				t.Fatalf("workspace does not show the pull request: %s", ev.Data)
			}
			break
		}
	}
}

func TestWorkspaceShowsCreatingWhileCloning(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	noChanges := false
	inner := fakeRepoSandbox(&noChanges)
	cloning := make(chan struct{})
	release := make(chan struct{})
	fake.stdoutFor = func(cmd map[string]any) string {
		if args, _ := json.Marshal(cmd["args"]); strings.Contains(string(args), "clone --quiet") {
			close(cloning)
			<-release
		}
		return inner(cmd)
	}
	h := srv.Handler()
	done := make(chan int)
	go func() { done <- do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`).Code }()
	<-cloning
	rec := do(t, h, http.MethodGet, "/workspaces", "")
	if !strings.Contains(rec.Body.String(), `"status":"creating"`) {
		t.Fatalf("list during clone: %s", rec.Body)
	}
	close(release)
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}
	if rec := do(t, h, http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Fatalf("list after create: %s", rec.Body)
	}
}

func TestWorkspaceExpiredStatus(t *testing.T) {
	srv, _, _, ws := newChatServer(t)
	srv.mu.Lock()
	srv.workspaces[ws.Name].ExpiresAt = time.Now().Add(-time.Second)
	srv.mu.Unlock()
	var snap testWorkspaceStatus
	_ = json.Unmarshal(do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.Bytes(), &snap)
	if snap.Status != "error" || !strings.Contains(snap.Error, "expired") {
		t.Fatalf("workspace = %+v, want expired", snap)
	}
}

func TestWebUI(t *testing.T) {
	h := NewHandler()
	for path, want := range map[string]string{
		"/":                   "<title>Mosso</title>",
		"/assets/app.js":      "EventSource",
		"/assets/markdown.js": "renderMarkdown",
		"/assets/style.css":   "#sidebar",
	} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: CSP = %q", path, csp)
		}
	}
	if rec := do(t, h, http.MethodGet, "/assets/missing.js", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset: status %d", rec.Code)
	}
}

func TestGitHubRepoAndBranchLists(t *testing.T) {
	srv, _ := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	h := srv.Handler()
	rec := do(t, h, http.MethodGet, "/github/repos", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"full_name":"octo/widgets"`) {
		t.Fatalf("repos: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/github/repos/octo/widgets/branches", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"branches":["main","dev"]}`+"\n" {
		t.Fatalf("branches: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/github/repos/octo/widgets.git/branches", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo: status %d", rec.Code)
	}
}

func TestWorkspaceMessageStopsWhenClientDisconnects(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	fake.mu.Lock()
	inner := fake.stdoutFor
	fake.stdoutFor = func(cmd map[string]any) string {
		once.Do(func() { close(started) })
		<-release
		return inner(cmd)
	}
	fake.mu.Unlock()
	events := openEvents(t, ts.URL, ws.Name, "")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/workspaces/"+ws.Name+"/claude/messages", strings.NewReader(`{"prompt":"hi"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	cancel()
	close(release)
	<-done
	if e := events.until("notice"); !strings.HasPrefix(e[len(e)-1].Text, "Stopped") {
		t.Fatalf("entries = %s, want the turn stopped", entryTypes(e))
	}
}
