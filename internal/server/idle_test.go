package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// idleStatus is the part of a workspace snapshot idle shutdown changes.
type idleStatus struct {
	Status             string    `json:"status"`
	SandboxState       string    `json:"sandbox_state"`
	SessionSeconds     int       `json:"session_seconds"`
	IdleTimeoutSeconds int       `json:"idle_timeout_seconds"`
	IdleStopAt         time.Time `json:"idle_stop_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func (h *persistentHarness) idle() idleStatus {
	h.t.Helper()
	var st idleStatus
	rec := do(h.t, h.srv.Handler(), http.MethodGet, "/workspaces/"+h.ws.Name, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		h.t.Fatalf("%v: %s", err, rec.Body)
	}
	return st
}

// finishTask sends a message and waits until Claude's result is recorded
// and the workspace is ready again.
func (h *persistentHarness) finishTask(prompt string) {
	h.t.Helper()
	events := openEvents(h.t, h.ts.URL, h.ws.Name, "")
	if resp := h.chat(prompt); resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("chat status = %d", resp.StatusCode)
	}
	events.until("result")
	h.waitStatus("ready")
}

func (h *persistentHarness) stops() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return len(h.fake.stopped)
}

func (h *persistentHarness) notices(substr string) int {
	n := 0
	for _, e := range h.history() {
		if e.Type == "notice" && strings.Contains(e.Text, substr) {
			n++
		}
	}
	return n
}

// TestIdleShutdownAfterATask: once Claude finishes, the idle timer starts;
// the sandbox is stopped (not deleted) after the idle timeout, the
// workspace stays active, and the next message resumes the same sandbox
// and Claude conversation.
func TestIdleShutdownAfterATask(t *testing.T) {
	h := newPersistentHarness(t)
	h.finishTask("first")
	finished := time.Now()
	st := h.idle()
	if st.IdleTimeoutSeconds != 900 || st.SessionSeconds != 2700 {
		t.Fatalf("defaults: %+v, want a 15-minute idle timeout and 45-minute sessions", st)
	}
	if st.IdleStopAt.IsZero() || st.IdleStopAt.Before(finished.Add(14*time.Minute)) || st.IdleStopAt.After(finished.Add(16*time.Minute)) {
		t.Fatalf("idle_stop_at = %v, want about 15 minutes after the task ended (%v)", st.IdleStopAt, finished)
	}

	h.srv.checkIdle(finished.Add(14 * time.Minute))
	if h.stops() != 0 || h.idle().Status != "ready" {
		t.Fatalf("stopped before the idle timeout: stops = %d, %+v", h.stops(), h.idle())
	}
	oldSession := h.fake.session(h.ws.Name)
	h.srv.checkIdle(finished.Add(16 * time.Minute))
	st = h.idle()
	if st.Status != "stopped" || st.SandboxState != "stopped" || !st.ExpiresAt.IsZero() || !st.IdleStopAt.IsZero() {
		t.Fatalf("after the idle timeout: %+v", st)
	}
	// Stopped, not deleted: the persistent sandbox keeps its snapshot.
	if h.stops() != 1 || len(h.fake.deleted) != 0 || h.fake.state(h.ws.Name) != "stopped" {
		t.Fatalf("stopped = %v, deleted = %v, state = %s", h.fake.stopped, h.fake.deleted, h.fake.state(h.ws.Name))
	}
	if h.notices("without activity") != 1 {
		t.Fatalf("history = %s, want one idle shutdown notice", entryTypes(h.history()))
	}
	if rec := h.record(); rec.SandboxState != "stopped" {
		t.Fatalf("saved state = %s", rec.SandboxState)
	}
	// Still an active workspace, not history.
	rec := do(t, h.srv.Handler(), http.MethodGet, "/workspaces", "")
	if !strings.Contains(rec.Body.String(), `"sandbox_state":"stopped"`) || strings.Contains(rec.Body.String(), `"terminated"`) {
		t.Fatalf("list: %s", rec.Body)
	}

	// The next message resumes it and continues the conversation.
	h.finishTask("again")
	if h.fake.session(h.ws.Name) == oldSession {
		t.Fatal("the sandbox was not resumed into a new session")
	}
	if last := h.fake.claude.last(); last.SessionID != testSessionID || last.Text != "again" {
		t.Fatalf("follow-up = %+v, want the same Claude session", last)
	}
	counts := countTypes(h.history())
	if counts["user"] != 2 || counts["result"] != 2 {
		t.Fatalf("history after resuming = %s, want each message and result once", entryTypes(h.history()))
	}
}

// TestIdleShutdownNeverStopsClaude: a running turn is never stopped, even
// long past the idle timeout, and even if the server lost track of it: the
// worker is asked before stopping.
func TestIdleShutdownNeverStopsClaude(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	h.chat("long task")
	<-started
	h.waitStatus("working")
	if !h.idle().IdleStopAt.IsZero() {
		t.Fatal("idle_stop_at is set while Claude works")
	}
	h.srv.checkIdle(time.Now().Add(3 * time.Hour))
	if h.stops() != 0 || h.idle().Status != "working" {
		t.Fatalf("stopped while Claude works: stops = %d, %+v", h.stops(), h.idle())
	}

	// Even if Mosso believed the workspace idle (say a turn was started
	// from elsewhere), the worker says it is running, so nothing stops.
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	run := ws.chat.running
	ws.chat.running, ws.taskRunning = nil, false
	delete(h.srv.busy, ws.Name)
	ws.lastActive, ws.workerIdleSince = time.Now().Add(-time.Hour), time.Time{}
	h.srv.mu.Unlock()
	h.srv.checkIdle(time.Now().Add(3 * time.Hour))
	if h.stops() != 0 || !h.fake.worker(h.ws.Name).running() {
		t.Fatalf("the worker's running task was stopped: stops = %d", h.stops())
	}
	h.srv.mu.Lock()
	ws.chat.running, ws.taskRunning, h.srv.busy[ws.Name] = run, true, true
	h.srv.mu.Unlock()

	// Once it finishes, the idle timer starts from then.
	close(release)
	events.until("result")
	h.waitStatus("ready")
	finished := time.Now()
	h.srv.checkIdle(finished.Add(10 * time.Minute))
	if h.stops() != 0 {
		t.Fatal("stopped 10 minutes after the task, before the idle timeout")
	}
	h.srv.checkIdle(finished.Add(16 * time.Minute))
	if h.stops() != 1 || h.idle().Status != "stopped" {
		t.Fatalf("not stopped after the task and the idle timeout: %+v", h.idle())
	}
}

// TestIdleShutdownWaitsForToolCalls: a tool call Mosso is running for the
// agent keeps the workspace active.
func TestIdleShutdownWaitsForToolCalls(t *testing.T) {
	h := newPersistentHarness(t)
	h.finishTask("first")
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	ws.toolCalls = map[string]bool{"call_1": true}
	h.srv.mu.Unlock()
	h.srv.checkIdle(time.Now().Add(time.Hour))
	if h.stops() != 0 {
		t.Fatal("stopped while a tool call runs")
	}
	h.srv.mu.Lock()
	delete(ws.toolCalls, "call_1")
	h.srv.mu.Unlock()
	h.srv.checkIdle(time.Now().Add(time.Hour))
	if h.stops() != 1 {
		t.Fatal("not stopped once the tool call ended")
	}
}

// TestMessageDuringIdleShutdown: a message sent while the sandbox is being
// stopped is not refused: it waits, then resumes the sandbox and runs.
func TestMessageDuringIdleShutdown(t *testing.T) {
	h := newPersistentHarness(t)
	h.finishTask("first")
	h.srv.mu.Lock()
	ws := h.srv.workspaces[h.ws.Name]
	h.srv.beginStopLocked(ws)
	h.srv.mu.Unlock()
	if st := h.idle(); st.Status != "stopping" {
		t.Fatalf("status = %s, want stopping", st.Status)
	}
	events := openEvents(t, h.ts.URL, h.ws.Name, "")
	answered := make(chan int, 1)
	go func() { answered <- h.chat("just in time").StatusCode }()
	select {
	case code := <-answered:
		t.Fatalf("the message was answered (%d) before the stop ended", code)
	case <-time.After(200 * time.Millisecond):
	}
	if err := h.srv.stopSandbox(ws, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := <-answered; code != http.StatusAccepted {
		t.Fatalf("message status = %d, want 202", code)
	}
	events.until("result") // the first message's, replayed
	if e := events.until("result"); e[len(e)-1].Result.Text != "you said just in time" {
		t.Fatalf("result = %+v", e[len(e)-1].Result)
	}
	if h.stops() != 1 || h.fake.state(h.ws.Name) != "running" {
		t.Fatalf("stops = %d, state = %s: want stopped once, then resumed", h.stops(), h.fake.state(h.ws.Name))
	}
}

// TestIdleShutdownWhileMossoWasAway: Mosso stops while Claude works, which
// neither stops the sandbox nor Claude. Claude finishes while Mosso is
// away; when Mosso starts again, it counts the idle time from when Claude
// finished, by the worker's clock, and stops the sandbox without waiting
// another full idle timeout.
func TestIdleShutdownWhileMossoWasAway(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	h.chat("long task")
	<-started
	waitFor(t, func() bool { return h.record().TaskRunning })
	h.stop()
	close(release)
	fw := h.fake.worker(h.ws.Name)
	waitFor(t, func() bool {
		fw.mu.Lock()
		defer fw.mu.Unlock()
		return fw.w != nil && fw.w.Status().State == "idle"
	})
	finished := time.Now()
	if h.stops() != 0 {
		t.Fatal("the sandbox was stopped while Mosso was away")
	}

	h.start()
	h.waitStatus("ready")
	h.srv.checkIdle(time.Now())
	if h.stops() != 0 {
		t.Fatal("stopped right after the restart, before the idle timeout")
	}
	h.srv.checkIdle(finished.Add(16 * time.Minute))
	if h.stops() != 1 || h.idle().Status != "stopped" {
		t.Fatalf("not stopped 16 minutes after Claude finished: %+v", h.idle())
	}
	if got := countTypes(h.history()); got["result"] != 1 {
		t.Fatalf("history = %s", entryTypes(h.history()))
	}
}

// TestIdleShutdownDisabled: with the idle timeout set to never, only the
// session length applies.
func TestIdleShutdownDisabled(t *testing.T) {
	h := newPersistentHarness(t)
	rec := do(t, h.srv.Handler(), http.MethodPatch, "/workspaces/"+h.ws.Name, `{"idle_timeout_seconds":0}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"idle_timeout_seconds":0`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	h.finishTask("first")
	if st := h.idle(); !st.IdleStopAt.IsZero() || st.ExpiresAt.IsZero() {
		t.Fatalf("%+v: want no idle stop, and the session's own end", st)
	}
	h.srv.checkIdle(time.Now().Add(48 * time.Hour))
	if h.stops() != 0 {
		t.Fatal("stopped with idle shutdown off")
	}
	if rec := h.record(); rec.IdleTimeoutSeconds == nil || *rec.IdleTimeoutSeconds != 0 {
		t.Fatalf("saved idle timeout = %v, want 0", rec.IdleTimeoutSeconds)
	}
	// Turned on again, it counts from the change.
	do(t, h.srv.Handler(), http.MethodPatch, "/workspaces/"+h.ws.Name, `{"idle_timeout_seconds":300}`)
	h.srv.checkIdle(time.Now().Add(4 * time.Minute))
	if h.stops() != 0 {
		t.Fatal("stopped before the new timeout")
	}
	h.srv.checkIdle(time.Now().Add(6 * time.Minute))
	if h.stops() != 1 {
		t.Fatal("not stopped after the new 5-minute timeout")
	}

	for _, body := range []string{`{"idle_timeout_seconds":30}`, `{"idle_timeout_seconds":90000}`, `{"idle_timeout_seconds":-1}`, `{}`} {
		if rec := do(t, h.srv.Handler(), http.MethodPatch, "/workspaces/"+h.ws.Name, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestIdleShutdownStopFails: a failed stop leaves the workspace running and
// is tried again later, waiting longer each time; the user is told once.
func TestIdleShutdownStopFails(t *testing.T) {
	h := newPersistentHarness(t)
	h.finishTask("first")
	h.fake.mu.Lock()
	h.fake.failStop = true
	h.fake.mu.Unlock()
	now := time.Now().Add(16 * time.Minute)
	h.srv.checkIdle(now)
	if st := h.idle(); st.Status != "ready" || st.SandboxState != "running" {
		t.Fatalf("after a failed stop: %+v", st)
	}
	if h.notices("could not stop") != 1 {
		t.Fatalf("history = %s, want one notice", entryTypes(h.history()))
	}
	// Not tried again at once.
	h.srv.checkIdle(now.Add(30 * time.Second))
	h.srv.checkIdle(now.Add(50 * time.Second))
	if h.notices("could not stop") != 1 {
		t.Fatal("the failure was reported again")
	}
	h.fake.mu.Lock()
	h.fake.failStop = false
	h.fake.mu.Unlock()
	h.srv.checkIdle(now.Add(30 * time.Second))
	if h.stops() != 0 {
		t.Fatal("tried again before the backoff")
	}
	h.srv.checkIdle(now.Add(5 * time.Minute))
	if h.stops() != 1 || h.idle().Status != "stopped" || len(h.fake.deleted) != 0 {
		t.Fatalf("after the retry: stops = %d, %+v, deleted = %v", h.stops(), h.idle(), h.fake.deleted)
	}
}

// TestStopAndResumeSurviveARestart: a workspace stopped by idle shutdown
// is still stopped, active and configured as before after a restart, and
// resumes on demand.
func TestStopAndResumeSurviveARestart(t *testing.T) {
	h := newPersistentHarness(t)
	do(t, h.srv.Handler(), http.MethodPatch, "/workspaces/"+h.ws.Name, `{"idle_timeout_seconds":1800}`)
	h.finishTask("first")
	h.srv.checkIdle(time.Now().Add(31 * time.Minute))
	h.waitStatus("stopped")
	h.stop()
	h.start()
	h.waitStatus("stopped")
	if st := h.idle(); st.IdleTimeoutSeconds != 1800 || st.SessionSeconds != 2700 || st.SandboxState != "stopped" {
		t.Fatalf("after a restart: %+v", st)
	}
	if h.fake.state(h.ws.Name) != "stopped" {
		t.Fatal("the restart resumed an idle sandbox")
	}
	// Resume explicitly; the fresh session gets the full idle timeout.
	if rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces/"+h.ws.Name+"/connect", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	h.waitStatus("ready")
	h.srv.checkIdle(time.Now().Add(20 * time.Minute))
	if h.stops() != 1 {
		t.Fatal("a just-resumed sandbox was stopped before its idle timeout")
	}
	if got := countTypes(h.history()); got["user"] != 1 || got["result"] != 1 {
		t.Fatalf("history = %s", entryTypes(h.history()))
	}
}

// TestManualStop: Stop stops the sandbox at once, keeping everything;
// it is refused while Claude works.
func TestManualStop(t *testing.T) {
	h := newPersistentHarness(t)
	started, release := h.fake.claude.blockRuns()
	h.chat("long task")
	<-started
	h.waitStatus("working")
	if rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces/"+h.ws.Name+"/sandbox/stop", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("stop while working: %d %s", rec.Code, rec.Body)
	}
	close(release)
	h.waitStatus("ready")
	rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces/"+h.ws.Name+"/sandbox/stop", `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"stopped"`) {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	if h.stops() != 1 || len(h.fake.deleted) != 0 {
		t.Fatalf("stopped = %v, deleted = %v", h.fake.stopped, h.fake.deleted)
	}
	// Stopping a stopped sandbox is a no-op.
	if rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces/"+h.ws.Name+"/sandbox/stop", `{}`); rec.Code != http.StatusOK || h.stops() != 1 {
		t.Fatalf("second stop: %d, stops = %d", rec.Code, h.stops())
	}
}

// TestWorkspacesFromBeforeIdleShutdown get 45-minute sessions and the
// default idle timeout.
func TestWorkspacesFromBeforeIdleShutdown(t *testing.T) {
	h := newPersistentHarness(t)
	h.stop()
	// Rewrite the record as an earlier Mosso saved it, without the fields.
	st, err := openStore(h.dataDir + "/mosso.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE workspaces SET state = json_remove(state, '$.session_seconds', '$.idle_timeout_seconds')`); err != nil {
		t.Fatal(err)
	}
	st.close()
	h.start()
	h.waitStatus("ready")
	if got := h.idle(); got.SessionSeconds != 2700 || got.IdleTimeoutSeconds != 900 {
		t.Fatalf("an earlier workspace: %+v", got)
	}
}

func TestSnapshotStatuses(t *testing.T) {
	srv, _ := newTestServer(t)
	ws := &workspace{Name: "mosso-0123456789ab", chat: newChat(), sandboxState: sandboxRunning, idleTimeout: defaultIdleTimeout}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, tc := range []struct {
		set  func()
		want string
	}{
		{func() {}, "ready"},
		{func() { ws.resuming = true }, "resuming"},
		{func() { ws.resuming, ws.stopping = false, true }, "stopping"},
		{func() { ws.stopping, ws.sandboxState = false, sandboxStopped }, "stopped"},
	} {
		tc.set()
		if got := srv.snapshotLocked(ws).Status; got != tc.want {
			t.Errorf("status = %s, want %s", got, tc.want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Minute:                "45 minutes",
		time.Hour:                       "1 hour",
		24 * time.Hour:                  "24 hours",
		90 * time.Minute:                "1 hour 30 minutes",
		time.Minute:                     "1 minute",
		2*time.Hour + 30*time.Second:    "2 hours 1 minute",
		0:                               "0 minutes",
		12*time.Hour + 59*time.Minute:   "12 hours 59 minutes",
		5*time.Minute + 29*time.Second:  "5 minutes",
		15*time.Minute + 31*time.Second: "16 minutes",
	} {
		if got := fmtDuration(d); got != want {
			t.Errorf("fmtDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
