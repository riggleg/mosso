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

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/worker"
)

// persistentHarness is a server over a fake Vercel whose state (sandboxes,
// worker logs) and whose data directory outlive the server, so a second
// server can be started over them like Mosso after a restart.
type persistentHarness struct {
	t       *testing.T
	fake    *fakeVercel
	api     *httptest.Server
	dataDir string
	srv     *Server
	ts      *httptest.Server
	ws      testWorkspace
}

func newPersistentHarness(t *testing.T) *persistentHarness {
	t.Helper()
	fake, api := newFakeVercel(t)
	fake.stdoutFor = fakeRepoSandbox()
	h := &persistentHarness{t: t, fake: fake, api: api, dataDir: t.TempDir()}
	h.start()
	rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &h.ws)
	return h
}

// start runs a new Mosso server over the saved state, like a restart.
func (h *persistentHarness) start() {
	h.t.Helper()
	h.srv = newServerOver(h.t, h.fake, h.api, h.dataDir)
	withToken(h.srv, testClaudeToken, nil)
	withGitHub(h.t, h.srv, &fakeGitHubAPI{})
	h.ts = httptest.NewServer(h.srv.Handler())
	h.t.Cleanup(h.ts.Close)
	h.t.Cleanup(h.srv.CloseStreams)
}

// stop shuts the server down the way `mosso` does on Ctrl-C.
func (h *persistentHarness) stop() {
	h.t.Helper()
	h.srv.CloseStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		h.t.Fatal(err)
	}
	h.ts.Close()
}

func (h *persistentHarness) status() testWorkspaceStatus {
	h.t.Helper()
	var snap testWorkspaceStatus
	rec := do(h.t, h.srv.Handler(), http.MethodGet, "/workspaces/"+h.ws.Name, "")
	if rec.Code != http.StatusOK {
		return testWorkspaceStatus{Status: "http " + rec.Result().Status}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	return snap
}

func (h *persistentHarness) waitStatus(want string) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := h.status()
		if st.Status == want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("status = %+v, want %s", st, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// history replays the whole chat from the server.
func (h *persistentHarness) history() []chatEntry {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	ws := h.srv.workspaces[h.ws.Name]
	if ws == nil {
		return nil
	}
	return append([]chatEntry(nil), ws.chat.entries...)
}

func (h *persistentHarness) chat(prompt string) *http.Response {
	h.t.Helper()
	return postJSON(h.t, h.ts.URL+"/workspaces/"+h.ws.Name+"/chat", `{"prompt":"`+prompt+`"}`)
}

// record reads the saved workspace record from disk.
func (h *persistentHarness) record() workspaceRecord {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dataDir, "workspaces", h.ws.Name, "workspace.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var rec workspaceRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		h.t.Fatal(err)
	}
	return rec
}

// countTypes counts entries by type (claude entries by event type).
func countTypes(entries []chatEntry) map[string]int {
	out := map[string]int{}
	for _, e := range entries {
		k := e.Type
		if e.Event != nil {
			k += ":" + e.Event.Type
		}
		out[k]++
	}
	return out
}

// TestClaudeContinuesAfterMossoStopsAndEventsAreRecovered is the milestone
// scenario: a task is running, Mosso shuts down, Claude finishes in the
// sandbox meanwhile, Mosso restarts, reconnects and shows everything that
// happened, once, and a follow-up continues the conversation.
func TestClaudeContinuesAfterMossoStopsAndEventsAreRecovered(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	if resp := h.chat("long task"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	<-started
	// Claude's first tool call is already visible.
	if got := entryTypes(events.until("claude")); got != "user claude:tool_use" {
		t.Fatalf("entries before shutdown = %s", got)
	}
	waitFor(t, func() bool { return h.record().TaskRunning && h.record().LastSeq >= 1 })

	h.stop()
	if len(h.fake.deleted) != 0 || len(h.fake.stopped) != 0 {
		t.Fatalf("shutdown touched the workspace sandbox: deleted=%v stopped=%v", h.fake.deleted, h.fake.stopped)
	}
	fw := h.fake.worker(h.ws.Name)
	if !fw.running() {
		t.Fatal("the worker stopped with Mosso")
	}
	// Claude finishes while Mosso is away.
	close(release)
	waitFor(t, func() bool {
		fw.mu.Lock()
		defer fw.mu.Unlock()
		return fw.w != nil && fw.w.Status().State == "idle" && fw.w.Status().Seq >= 5
	})

	// Mosso restarts: the workspace is listed at once, from disk, and the
	// missed events are fetched from the worker.
	h.start()
	rec := do(t, h.srv.Handler(), http.MethodGet, "/workspaces", "")
	if !strings.Contains(rec.Body.String(), h.ws.Name) || !strings.Contains(rec.Body.String(), `"repo":"octo/widgets"`) {
		t.Fatalf("workspaces after restart: %s", rec.Body)
	}
	h.waitStatus("ready")
	hist := h.history()
	if got := entryTypes(hist); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("history after restart = %s", got)
	}
	// (The user entry was saved before the worker numbered it, so only its
	// record carries the sequence number; the later entries carry theirs.)
	if hist[3].Result.Text != "you said long task" || hist[1].Seq != 3 || hist[3].Seq != 5 {
		t.Fatalf("history = %+v", hist)
	}
	if rec := h.record(); rec.TaskRunning || rec.LastSeq != 5 || rec.SessionID != testSessionID {
		t.Fatalf("record = %+v", rec)
	}
	// Browsers replay the same, from the server's log.
	replay := openEvents(t, h.ts.URL, h.ws.Name, "")
	if got := entryTypes(replay.until("result")); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("replay = %s", got)
	}

	// A follow-up continues the same Claude session.
	if resp := h.chat("and again"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("follow-up status = %d", resp.StatusCode)
	}
	more := replay.until("result")
	if got := entryTypes(more); got != "user claude:tool_use claude:tool_result result" || more[3].Result.Text != "you said and again" {
		t.Fatalf("follow-up entries = %s: %+v", got, more[3].Result)
	}
	if last := h.fake.claude.last(); last.SessionID != testSessionID || last.Text != "and again" {
		t.Fatalf("follow-up task = %+v", last)
	}
	if fw.starts != 1 {
		t.Fatalf("the worker was started %d times, want once (it never stopped)", fw.starts)
	}
}

// TestReconnectAfterSandboxSessionStoppedResumesIt: the sandbox session
// ended (timeout) while Mosso was away. The history is shown from disk, the
// sandbox stays stopped until needed, and the next message resumes it and
// restarts the worker over its recovered log.
func TestReconnectAfterSandboxSessionStoppedResumesIt(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("first")
	events.until("result")
	att := uploaded(t, postUpload(t, h.ts.URL, h.ws.Name, upFile{"a.png", onePixelPNG}))[0]
	h.stop()
	h.fake.timeout(h.ws.Name)
	oldSession := h.fake.session(h.ws.Name)

	h.start()
	h.waitStatus("stopped")
	hist := h.history()
	if got := entryTypes(hist); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("history = %s", got)
	}
	if h.fake.session(h.ws.Name) != oldSession {
		t.Fatal("an idle stopped sandbox was resumed just by restarting Mosso")
	}
	// Attachments are not fetched from a stopped sandbox (that would boot
	// it for a thumbnail), but the record survived.
	if resp, _ := get(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusGone {
		t.Fatalf("attachment from a stopped sandbox: status = %d", resp.StatusCode)
	}

	// The next message resumes the sandbox: a new session, the same files,
	// a restarted worker, and the conversation continues.
	fresh := openEvents(t, h.ts.URL, h.ws.Name, "")
	fresh.until("result")
	if resp := h.chat("second"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	more := fresh.until("result")
	if got := entryTypes(more); got != "user notice claude:tool_use claude:tool_result result" || !strings.HasPrefix(more[1].Text, "Resuming the sandbox") {
		t.Fatalf("entries = %s: %+v", got, more[1])
	}
	newSession := h.fake.session(h.ws.Name)
	if newSession == oldSession || !strings.HasSuffix(newSession, "_2") {
		t.Fatalf("session = %s, want a resumed one", newSession)
	}
	if fw := h.fake.worker(h.ws.Name); fw.starts != 2 {
		t.Fatalf("worker starts = %d, want 2", fw.starts)
	}
	if last := h.fake.claude.last(); last.SessionID != testSessionID {
		t.Fatalf("the follow-up did not resume the Claude session: %+v", last)
	}
	h.waitStatus("ready")
	if rec := h.record(); rec.Sandbox.SessionID != newSession || rec.SandboxState != sandboxRunning {
		t.Fatalf("record = %+v", rec)
	}
	// The attachment is readable again.
	if resp, body := get(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusOK || string(body) != string(onePixelPNG) {
		t.Fatalf("attachment after resume: %d", resp.StatusCode)
	}
	// Publishing after a restart knows nothing was pushed yet and works.
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/pull-request", `{"title":"t"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish status = %d", resp.StatusCode)
	}
}

// TestConnectEndpointResumesAStoppedSandbox resumes without a message.
func TestConnectEndpointResumesAStoppedSandbox(t *testing.T) {
	h := newPersistentHarness(t)
	h.fake.timeout(h.ws.Name)
	// The follower notices the session ending.
	h.waitStatus("stopped")
	resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/connect", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d", resp.StatusCode)
	}
	var snap testWorkspaceStatus
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	if snap.Status != "ready" {
		t.Fatalf("status after connect = %+v", snap)
	}
	if !strings.HasSuffix(h.fake.session(h.ws.Name), "_2") {
		t.Fatal("the sandbox was not resumed")
	}
}

// TestSessionEndsWhileClaudeWorks: the sandbox times out mid-task while
// Mosso is watching. The turn is marked interrupted, the saved events stay,
// and a follow-up resumes the sandbox and continues; the worker's own
// interrupted marker is not shown twice.
func TestSessionEndsWhileClaudeWorks(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	defer close(release)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("long task")
	<-started
	events.until("claude")
	h.fake.timeout(h.ws.Name)
	notice := events.until("notice")
	if last := notice[len(notice)-1]; !strings.HasPrefix(last.Text, "Interrupted") {
		t.Fatalf("entries = %s", entryTypes(notice))
	}
	h.waitStatus("stopped")

	h.fake.claude.mu.Lock()
	h.fake.claude.block = nil
	h.fake.claude.mu.Unlock()
	h.chat("continue")
	more := events.until("result")
	got := entryTypes(more)
	if got != "user notice claude:tool_use claude:tool_result result" || !strings.HasPrefix(more[1].Text, "Resuming") {
		t.Fatalf("entries after resume = %s", got)
	}
	// Only Mosso's interruption notice is shown, not the worker's as well.
	n := 0
	for _, e := range h.history() {
		if e.Type == "notice" && strings.HasPrefix(e.Text, "Interrupted") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("interrupted notices = %d, want 1", n)
	}
}

// TestRestartAfterSessionEndedMidTaskCatchesUp: Mosso was down when the
// session timed out mid-task. On restart it resumes the sandbox (a task
// was in flight) and the worker's log marks the task interrupted.
func TestRestartAfterSessionEndedMidTaskCatchesUp(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	defer close(release)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("long task")
	<-started
	events.until("claude")
	waitFor(t, func() bool { return h.record().TaskRunning })
	h.stop()
	h.fake.timeout(h.ws.Name)

	h.fake.claude.mu.Lock()
	h.fake.claude.block = nil
	h.fake.claude.mu.Unlock()
	h.start()
	h.waitStatus("ready")
	hist := h.history()
	got := entryTypes(hist)
	if got != "user claude:tool_use claude:tool_result notice notice" {
		t.Fatalf("history = %s", got)
	}
	if !strings.HasPrefix(hist[3].Text, "Resuming") || !strings.HasPrefix(hist[4].Text, "Interrupted") {
		t.Fatalf("notices = %q, %q", hist[3].Text, hist[4].Text)
	}
	if !strings.HasSuffix(h.fake.session(h.ws.Name), "_2") {
		t.Fatal("the sandbox was not resumed to catch up")
	}
	// Work continues.
	h.chat("continue")
	events = openEvents(t, h.ts.URL, h.ws.Name, "")
	all := events.until("result")
	if got := entryTypes(all); !strings.HasSuffix(got, "user claude:tool_use claude:tool_result result") {
		t.Fatalf("entries = %s", got)
	}
}

// TestTerminateRemovesSandboxSnapshotsAndSavedState: Terminate is the only
// thing that deletes a workspace.
func TestTerminateRemovesSandboxSnapshotsAndSavedState(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("hi")
	events.until("result")
	if got := readDir(t, filepath.Join(h.dataDir, "workspaces")); len(got) != 1 {
		t.Fatalf("saved workspaces = %v", got)
	}
	req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/workspaces/"+h.ws.Name, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %v %v", resp, err)
	}
	if len(h.fake.deleted) != 1 || !h.fake.orphanSnapshots[0] || len(h.fake.stopped) != 1 {
		t.Fatalf("deleted=%v orphan=%v stopped=%v", h.fake.deleted, h.fake.orphanSnapshots, h.fake.stopped)
	}
	if got := readDir(t, filepath.Join(h.dataDir, "workspaces")); len(got) != 0 {
		t.Fatalf("saved state left behind: %v", got)
	}
	if h.fake.worker(h.ws.Name).running() {
		t.Fatal("the worker outlived its sandbox")
	}
	h.stop()
	h.start()
	if rec := do(t, h.srv.Handler(), http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), `"workspaces":[]`) {
		t.Fatalf("terminated workspace came back: %s", rec.Body)
	}
}

// TestGoneSandboxKeepsHistoryUntilTerminated: the sandbox disappeared
// (deleted elsewhere, or its snapshot expired). The chat stays readable, new
// messages are refused with an explanation, and Terminate cleans up.
func TestGoneSandboxKeepsHistoryUntilTerminated(t *testing.T) {
	h := newPersistentHarness(t)
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("hi")
	events.until("result")
	h.stop()
	h.fake.lose(h.ws.Name)

	h.start()
	h.waitStatus("error")
	if st := h.status(); !strings.Contains(st.Error, "no longer exists") {
		t.Fatalf("status = %+v", st)
	}
	replay := openEvents(t, h.ts.URL, h.ws.Name, "")
	if got := entryTypes(replay.until("result")); got != "user claude:tool_use claude:tool_result result" {
		t.Fatalf("replay = %s", got)
	}
	if resp := h.chat("again"); resp.StatusCode != http.StatusGone {
		t.Fatalf("chat on a gone sandbox: status = %d", resp.StatusCode)
	}
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/connect", `{}`); resp.StatusCode != http.StatusGone {
		t.Fatalf("connect on a gone sandbox: status = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/workspaces/"+h.ws.Name, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %v %v", resp, err)
	}
	if got := readDir(t, filepath.Join(h.dataDir, "workspaces")); len(got) != 0 {
		t.Fatalf("saved state left behind: %v", got)
	}
}

// TestRestoredWorkspaceRemembersPublishedHead: after a restart, a rebased
// branch can still be force-pushed, because the last pushed head is saved.
func TestRestoredWorkspaceRemembersPublishedHead(t *testing.T) {
	h := newPersistentHarness(t)
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/pull-request", `{"title":"t"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish status = %d", resp.StatusCode)
	}
	if rec := h.record(); rec.Pushed != testCommitSHA || rec.PullRequest == nil || rec.PullRequest.Number != 7 {
		t.Fatalf("record = %+v", rec)
	}
	h.stop()
	h.start()
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	opts := ws.git.Options()
	pr := ws.PullRequest
	h.srv.mu.Unlock()
	if opts.Pushed != testCommitSHA || pr == nil || pr.Number != 7 {
		t.Fatalf("restored mirror options = %+v, pr = %+v", opts, pr)
	}
}

// TestStoppedWorkspacesDoNotCountTowardsTheSandboxLimit.
func TestStoppedWorkspacesDoNotCountTowardsTheSandboxLimit(t *testing.T) {
	h := newPersistentHarness(t)
	h.fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	for i := 0; i < maxSandboxes; i++ {
		if rec := do(t, h.srv.Handler(), http.MethodPost, "/sandboxes", ""); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: status = %d (a stopped workspace should not count)", i, rec.Code)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := workspaceRecord{Name: "mosso-0123456789ab", Repo: "octo/widgets", Base: "main", Branch: "mosso/x", Dir: "/vercel/sandbox/widgets",
		SandboxState: sandboxStopped, LastSeq: 7, SessionID: testSessionID, PullRequest: &github.PullRequest{Number: 3, URL: "https://github.com/octo/widgets/pull/3"},
		Attachments: []attachment{{ID: "a1", Name: "x.png", Size: 3, Type: "image/png", Image: true}}, CreatedAt: time.Now().UTC()}
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../etc", "mosso-x", "", "other"} {
		if err := st.save(workspaceRecord{Name: bad}); err == nil {
			t.Errorf("saved record with name %q", bad)
		}
		if err := st.appendEntry(bad, chatEntry{}); err == nil {
			t.Errorf("appended for name %q", bad)
		}
		if err := st.remove(bad); err == nil {
			t.Errorf("removed name %q", bad)
		}
	}
	_ = st.appendEntry(rec.Name, chatEntry{Type: "user", Text: "hi", Seq: 1})
	_ = st.appendEntry(rec.Name, chatEntry{Type: "notice", Text: "n"})
	// A truncated last line is skipped.
	f, _ := os.OpenFile(filepath.Join(dir, "workspaces", rec.Name, "chat.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"type":"us`)
	f.Close()

	records, err := st.load()
	if err != nil || len(records) != 1 {
		t.Fatalf("load = %+v, %v", records, err)
	}
	got := records[0]
	if got.Name != rec.Name || got.LastSeq != 7 || got.SessionID != testSessionID || got.PullRequest.Number != 3 || len(got.Attachments) != 1 || got.UpdatedAt.IsZero() {
		t.Fatalf("record = %+v", got)
	}
	entries, err := st.loadEntries(rec.Name)
	if err != nil || len(entries) != 2 || entries[0].Seq != 1 || entries[1].ID != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if err := st.remove(rec.Name); err != nil {
		t.Fatal(err)
	}
	if records, _ := st.load(); len(records) != 0 {
		t.Fatalf("records after remove = %+v", records)
	}
	// Unknown directories and files are ignored.
	_ = os.MkdirAll(filepath.Join(dir, "workspaces", "junk"), 0o700)
	if records, err := st.load(); err != nil || len(records) != 0 {
		t.Fatalf("load with junk = %+v, %v", records, err)
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv(EnvDataDir, "/tmp/mosso-test-data")
	if d, err := DataDir(); err != nil || d != "/tmp/mosso-test-data" {
		t.Fatalf("DataDir = %q, %v", d, err)
	}
	t.Setenv(EnvDataDir, "")
	d, err := DataDir()
	if err != nil || !strings.HasSuffix(d, "mosso") {
		t.Fatalf("DataDir = %q, %v", d, err)
	}
}

// TestWorkerEventsAreNotDuplicatedOnReplay: the follower may deliver the
// worker's own copy of the user event before or after Mosso records its
// submission; either way the chat has it once.
func TestWorkerEventsAreNotDuplicatedOnReplay(t *testing.T) {
	srv, _ := newTestServer(t)
	ws := &workspace{Name: "mosso-0123456789ab", chat: newChat(), attachments: map[string]attachment{}, sandboxState: sandboxRunning}
	srv.mu.Lock()
	srv.workspaces[ws.Name] = ws
	srv.busy[ws.Name] = true
	run := &turnRun{done: make(chan struct{})}
	ws.chat.running = run
	ws.taskRunning = true
	run.userIdx = ws.chat.add(chatEntry{Type: "user", Text: "hi"}).ID
	srv.mu.Unlock()

	srv.applyEvent(ws, worker.Event{Seq: 1, Type: worker.EventUser, TaskID: "task_1", Text: "hi"})
	srv.applyEvent(ws, worker.Event{Seq: 2, Type: worker.EventSession, TaskID: "task_1", SessionID: testSessionID})
	srv.applyEvent(ws, worker.Event{Seq: 1, Type: worker.EventUser, TaskID: "task_1", Text: "hi"}) // replayed
	srv.applyEvent(ws, worker.Event{Seq: 3, Type: worker.EventResult, TaskID: "task_1", SessionID: testSessionID, Result: &worker.Result{Text: "done"}})
	srv.applyEvent(ws, worker.Event{Seq: 3, Type: worker.EventResult, TaskID: "task_1", Result: &worker.Result{Text: "done"}}) // replayed
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := entryTypes(ws.chat.entries); got != "user result" || ws.chat.entries[0].Seq != 1 || run.taskID != "task_1" {
		t.Fatalf("entries = %s (%+v), taskID = %q", got, ws.chat.entries, run.taskID)
	}
	if srv.busy[ws.Name] || ws.chat.running != nil || ws.chat.sessionID != testSessionID || ws.lastSeq != 3 {
		t.Fatalf("turn not finished: busy=%t running=%v session=%q lastSeq=%d", srv.busy[ws.Name], ws.chat.running, ws.chat.sessionID, ws.lastSeq)
	}
	select {
	case <-run.done:
	default:
		t.Fatal("run.done not closed")
	}
}
