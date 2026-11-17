package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// queueStatus is the queue part of a workspace snapshot.
type queueStatus struct {
	Status      string          `json:"status"`
	Queue       []queuedMessage `json:"queue"`
	QueuePaused string          `json:"queue_paused"`
}

func (h *persistentHarness) queue() queueStatus {
	h.t.Helper()
	var st queueStatus
	rec := do(h.t, h.srv.Handler(), http.MethodGet, "/workspaces/"+h.ws.Name, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	return st
}

// send posts a message and returns how it was taken: "working" or
// "queued", and the queued message's ID.
func (h *persistentHarness) send(prompt string) (status, id string) {
	h.t.Helper()
	resp := h.chat(prompt)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("send %q: status = %d", prompt, resp.StatusCode)
	}
	var body struct{ Status, ID string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Status, body.ID
}

func (h *persistentHarness) cancelQueued(id string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/workspaces/"+h.ws.Name+"/queue/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// ran lists the messages Claude ran, in order.
func (fc *fakeClaudeRunner) ran() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []string
	for _, m := range fc.tasks {
		out = append(out, m.Text)
	}
	return out
}

func userTexts(entries []chatEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Type == "user" {
			out = append(out, e.Text)
		}
	}
	return out
}

// waitSubmitted waits until the worker has the turn in progress.
func (h *persistentHarness) waitSubmitted() {
	h.t.Helper()
	waitFor(h.t, func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		run := h.srv.workspaces[h.ws.Name].chat.running
		return run != nil && run.taskID != ""
	})
}

// waitRan waits until Claude has run exactly want, and the workspace is
// idle with an empty queue.
func (h *persistentHarness) waitRan(want ...string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		q := h.queue()
		got := h.fake.claude.ran()
		if slices.Equal(got, want) && q.Status == "ready" && len(q.Queue) == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("Claude ran %q (status %s, queue %+v), want %q", got, q.Status, q.Queue, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQueuedMessagesRunInOrder is the acceptance scenario: three messages
// sent while Claude works are queued and saved, one is cancelled, and the
// others run by themselves, in order, in the same Claude session.
func TestQueuedMessagesRunInOrder(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	if st, _ := h.send("long task"); st != "working" {
		t.Fatalf("idle send = %s, want working", st)
	}
	<-started
	var ids []string
	for _, p := range []string{"one", "two", "three"} {
		st, id := h.send(p)
		if st != "queued" || id == "" {
			t.Fatalf("send %q while working = %s %q, want queued", p, st, id)
		}
		ids = append(ids, id)
	}
	q := h.queue()
	if q.Status != "working" || len(q.Queue) != 3 || q.Queue[0].Text != "one" || q.Queue[2].Text != "three" || q.Queue[0].ID != ids[0] {
		t.Fatalf("queue = %+v", q)
	}
	if rec := h.record(); len(rec.Queue) != 3 {
		t.Fatalf("saved queue = %+v", rec.Queue)
	}
	// Queued messages are not in the chat yet.
	if got := userTexts(h.history()); !slices.Equal(got, []string{"long task"}) {
		t.Fatalf("chat users = %q", got)
	}

	if code := h.cancelQueued(ids[1]); code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	if code := h.cancelQueued(ids[1]); code != http.StatusNotFound {
		t.Fatalf("cancel again = %d, want 404", code)
	}
	if rec := h.record(); len(rec.Queue) != 2 || rec.Queue[1].Text != "three" {
		t.Fatalf("saved queue after cancel = %+v", rec.Queue)
	}

	close(release)
	h.waitRan("long task", "one", "three")
	h.fake.claude.mu.Lock()
	tasks := slices.Clone(h.fake.claude.tasks)
	h.fake.claude.mu.Unlock()
	for _, m := range tasks[1:] {
		if m.SessionID != testSessionID || m.Key == "" {
			t.Fatalf("queued task = %+v, want the same session and a key", m)
		}
	}
	entries := h.history()
	if got := userTexts(entries); !slices.Equal(got, []string{"long task", "one", "three"}) {
		t.Fatalf("chat users = %q", got)
	}
	if got := countTypes(entries); got["result"] != 3 || got["error"] != 0 {
		t.Fatalf("chat = %s", entryTypes(entries))
	}
	for _, e := range entries {
		if e.Type == "user" && e.Seq == 0 && e.Text != "long task" {
			t.Fatalf("queued message entry not matched to the worker: %+v", e)
		}
	}
	if rec := h.record(); len(rec.Queue) != 0 || rec.QueuePaused != "" || rec.TaskRunning {
		t.Fatalf("record = %+v", rec)
	}
}

// TestIdleSendIsNotQueued: with Claude idle and nothing queued, a message
// is sent at once, as before.
func TestIdleSendIsNotQueued(t *testing.T) {
	h := newPersistentHarness(t)
	if st, _ := h.send("hello"); st != "working" {
		t.Fatalf("send = %s", st)
	}
	h.waitRan("hello")
}

// TestQueueSurvivesRestart: Mosso stops while Claude works with messages
// queued; Claude finishes meanwhile. After the restart the queued messages
// are still there and run, in order, once each.
func TestQueueSurvivesRestart(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	h.send("long task")
	<-started
	h.waitSubmitted()
	h.send("a")
	h.send("b")
	h.stop()
	close(release) // Claude finishes while Mosso is away
	h.start()
	h.waitRan("long task", "a", "b")
	if got := userTexts(h.history()); !slices.Equal(got, []string{"long task", "a", "b"}) {
		t.Fatalf("chat users = %q", got)
	}
}

// TestQueueSurvivesRestartWhileClaudeStillWorks: Mosso restarts while the
// task is still running; the queue waits for it, then goes on.
func TestQueueSurvivesRestartWhileClaudeStillWorks(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	h.send("long task")
	<-started
	h.waitSubmitted()
	h.send("a")
	h.stop()
	h.start()
	h.waitStatus("working")
	time.Sleep(100 * time.Millisecond)
	if q := h.queue(); len(q.Queue) != 1 || h.fake.claude.count() != 1 {
		t.Fatalf("queue = %+v, ran %q", q, h.fake.claude.ran())
	}
	close(release)
	h.waitRan("long task", "a")
}

// rewriteRecord edits the stopped harness's saved workspace, as if Mosso
// had stopped at another moment, and keeps only the first n chat entries.
func (h *persistentHarness) rewriteRecord(n int, edit func(*workspaceRecord)) {
	h.t.Helper()
	st, err := openStore(filepath.Join(h.dataDir, "mosso.db"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer st.close()
	records, err := st.load()
	if err != nil || len(records) != 1 {
		h.t.Fatalf("records = %+v, %v", records, err)
	}
	rec := records[0]
	edit(&rec)
	entries, err := st.loadEntries(rec.Name)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM chat_entries WHERE workspace_id = ?`, rec.Name); err != nil {
		h.t.Fatal(err)
	}
	if err := st.save(rec, entries[:min(n, len(entries))]...); err != nil {
		h.t.Fatal(err)
	}
}

// runQueuedOnce runs "long task" and one queued message, "next", to the
// end, stops Mosso, and returns the queued message and its index in the
// chat.
func (h *persistentHarness) runQueuedOnce() (queuedMessage, int) {
	h.t.Helper()
	started, release := h.fake.claude.blockRuns()
	h.send("long task")
	<-started
	_, id := h.send("next")
	close(release)
	h.waitRan("long task", "next")
	h.stop()
	idx := slices.IndexFunc(h.history(), func(e chatEntry) bool { return e.Type == "user" && e.Text == "next" })
	return queuedMessage{ID: id, Text: "next", Sending: true}, idx
}

// TestQueuedMessageTheWorkerGotIsNotRunAgain: Mosso stopped while sending
// a queued message, after the worker had it but before Mosso recorded
// that. After the restart the worker's copy takes it off the queue: it
// runs once and shows once.
func TestQueuedMessageTheWorkerGotIsNotRunAgain(t *testing.T) {
	h := newPersistentHarness(t)
	q, idx := h.runQueuedOnce()
	user, ok := h.fake.worker(h.ws.Name).log.UserEvent(q.ID)
	if !ok {
		t.Fatal("the worker has no user event with the queued message's key")
	}
	h.rewriteRecord(idx, func(rec *workspaceRecord) {
		rec.Queue, rec.TaskRunning, rec.LastSeq = []queuedMessage{q}, true, user.Seq-1
	})
	h.start()
	h.waitRan("long task", "next")
	time.Sleep(200 * time.Millisecond) // time for a wrong second run to start
	if got := h.fake.claude.ran(); len(got) != 2 {
		t.Fatalf("Claude ran %q", got)
	}
	entries := h.history()
	if got := userTexts(entries); !slices.Equal(got, []string{"long task", "next"}) {
		t.Fatalf("chat users = %q", got)
	}
	if got := countTypes(entries); got["result"] != 2 || got["notice"] != 0 {
		t.Fatalf("chat = %s", entryTypes(entries))
	}
}

// TestQueuedMessageSentAgainIsNotRunTwice: Mosso's saved state says a
// queued message is still being sent although the worker ran it: sending
// it again after the restart finds the worker's copy (same key) instead of
// running it a second time.
func TestQueuedMessageSentAgainIsNotRunTwice(t *testing.T) {
	h := newPersistentHarness(t)
	q, _ := h.runQueuedOnce()
	h.rewriteRecord(1000, func(rec *workspaceRecord) {
		rec.Queue, rec.TaskRunning = []queuedMessage{q}, true
	})
	h.start()
	h.waitRan("long task", "next")
	time.Sleep(200 * time.Millisecond)
	if got := h.fake.claude.ran(); len(got) != 2 {
		t.Fatalf("Claude ran %q", got)
	}
	entries := h.history()
	if got := userTexts(entries); !slices.Equal(got, []string{"long task", "next"}) {
		t.Fatalf("chat users = %q", got)
	}
	for _, e := range entries {
		if e.Type == "notice" && strings.HasPrefix(e.Text, "Interrupted") {
			t.Fatalf("chat = %s: %q", entryTypes(entries), e.Text)
		}
	}
}

// TestQueuedMessageNotSentBeforeRestartIsSent: Mosso stopped while sending
// a queued message the worker never got; after the restart it is sent,
// once, without an interruption notice.
func TestQueuedMessageNotSentBeforeRestartIsSent(t *testing.T) {
	h := newPersistentHarness(t)
	h.send("first")
	h.waitRan("first")
	h.stop()
	h.rewriteRecord(1000, func(rec *workspaceRecord) {
		rec.Queue, rec.TaskRunning = []queuedMessage{{ID: "q_0123456789abcdef", Text: "pending", Sending: true}}, true
	})
	h.start()
	h.waitRan("first", "pending")
	entries := h.history()
	if got := userTexts(entries); !slices.Equal(got, []string{"first", "pending"}) {
		t.Fatalf("chat users = %q", got)
	}
	if got := countTypes(entries); got["notice"] != 0 {
		t.Fatalf("chat = %s", entryTypes(entries))
	}
}

// TestQueuePausesWhenClaudeFails: a failed turn keeps the queue but pauses
// it; a message sent while it is paused and Claude is idle goes at once;
// resuming sends the queued ones.
func TestQueuePausesWhenClaudeFails(t *testing.T) {
	h := newPersistentHarness(t)
	fc := h.fake.claude
	started, release := fc.blockRuns()
	fc.mu.Lock()
	fc.fail = true
	fc.mu.Unlock()
	h.send("long task")
	<-started
	h.send("queued")
	close(release)
	waitFor(t, func() bool { q := h.queue(); return q.QueuePaused == queueFailed && q.Status == "error" })
	fc.mu.Lock()
	fc.fail = false
	fc.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if q := h.queue(); len(q.Queue) != 1 || q.Queue[0].Sending || fc.count() != 1 {
		t.Fatalf("after the failure: queue %+v, ran %q", q, fc.ran())
	}
	if rec := h.record(); rec.QueuePaused != queueFailed || len(rec.Queue) != 1 {
		t.Fatalf("saved = %q %+v", rec.QueuePaused, rec.Queue)
	}

	if st, _ := h.send("direct"); st != "working" {
		t.Fatalf("send while paused and idle = %s, want working", st)
	}
	waitFor(t, func() bool { return h.queue().Status == "ready" && fc.count() == 2 })
	if q := h.queue(); len(q.Queue) != 1 || q.QueuePaused != queueFailed {
		t.Fatalf("queue = %+v", q)
	}

	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/queue/resume", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("resume = %d", resp.StatusCode)
	}
	h.waitRan("long task", "direct", "queued")
	if q := h.queue(); q.QueuePaused != "" {
		t.Fatalf("still paused: %+v", q)
	}
}

// TestStopPausesTheQueue: stopping Claude does not start the next queued
// message; resuming the queue does.
func TestStopPausesTheQueue(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	h.send("long task")
	<-started
	h.send("after")
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/chat/stop", `{}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { q := h.queue(); return q.QueuePaused == queueStopped && q.Status == "ready" })
	time.Sleep(100 * time.Millisecond)
	if n := h.fake.claude.count(); n != 1 {
		t.Fatalf("Claude ran %q after the stop", h.fake.claude.ran())
	}
	close(release)
	postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/queue/resume", `{}`)
	h.waitRan("long task", "after")
}

// TestQueueWaitsForTheSandbox: the sandbox session ends while Claude works
// with a message queued. The message stays queued, not paused; when the
// sandbox is resumed it is sent.
func TestQueueWaitsForTheSandbox(t *testing.T) {
	h := newPersistentHarness(t)
	fc := h.fake.claude
	started, release := fc.blockRuns()
	defer close(release)
	h.send("long task")
	<-started
	h.waitSubmitted()
	h.send("after")
	h.fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	time.Sleep(100 * time.Millisecond)
	if q := h.queue(); len(q.Queue) != 1 || q.QueuePaused != "" || fc.count() != 1 {
		t.Fatalf("after the session ended: queue %+v, ran %q", q, fc.ran())
	}
	// The synchronous API does not jump the queue either.
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/claude/messages", `{"prompt":"sync"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("claude/messages while messages are queued = %d, want 409", resp.StatusCode)
	}
	fc.mu.Lock()
	fc.block = nil
	fc.mu.Unlock()
	if resp := postJSON(t, h.ts.URL+"/workspaces/"+h.ws.Name+"/connect", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("connect = %d", resp.StatusCode)
	}
	h.waitRan("long task", "after")
}

// TestQueuingToAStoppedSandboxResumesIt: a message sent while queued ones
// wait for a stopped sandbox joins the end of the queue and, like any
// message sent to a stopped sandbox, resumes it; then they all run in order.
func TestQueuingToAStoppedSandboxResumesIt(t *testing.T) {
	h := newPersistentHarness(t)
	fc := h.fake.claude
	started, release := fc.blockRuns()
	defer close(release)
	h.send("long task")
	<-started
	h.waitSubmitted()
	h.send("after")
	h.fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	fc.mu.Lock()
	fc.block = nil
	fc.mu.Unlock()
	if st, _ := h.send("later"); st != "queued" {
		t.Fatalf("send while waiting for the sandbox = %s, want queued", st)
	}
	h.waitRan("long task", "after", "later")
}

// TestQueuedMessageBeingSentCannotBeCancelled: once Mosso is sending a
// queued message, cancelling it is refused rather than racing the worker.
func TestQueuedMessageBeingSentCannotBeCancelled(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	defer close(release)
	h.send("long task")
	<-started
	_, id := h.send("next")
	_, other := h.send("other")
	// Play the turn in progress sending "next".
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	run := ws.chat.running
	q := ws.Queue[0]
	run.queued = &q
	h.srv.mu.Unlock()
	defer func() {
		h.srv.mu.Lock()
		run.queued = nil
		h.srv.mu.Unlock()
	}()
	if st := h.queue(); !st.Queue[0].Sending || st.Queue[1].Sending {
		t.Fatalf("queue = %+v, want the first one sending", st.Queue)
	}
	if code := h.cancelQueued(id); code != http.StatusConflict {
		t.Fatalf("cancel while sending = %d, want 409", code)
	}
	if code := h.cancelQueued(other); code != http.StatusOK {
		t.Fatalf("cancel another = %d", code)
	}
	if code := h.cancelQueued("q_unknown"); code != http.StatusNotFound {
		t.Fatalf("cancel unknown = %d, want 404", code)
	}
}
