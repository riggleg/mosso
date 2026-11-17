package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/vercel"
)

// Idle shutdown stops a workspace's sandbox session once nothing has
// happened in it for the workspace's idle timeout, so an idle sandbox does
// not keep billing memory until its session ends. Stopping keeps the
// workspace: Vercel snapshots the persistent sandbox's filesystem, and the
// next message (or Resume) boots a new session from the snapshot, with the
// checkout, Claude Code's session and the worker's event log.
//
// The Mosso server decides, and the sandbox worker confirms. A workspace
// is idle when no Claude turn runs (the worker owns Claude, its tool calls
// and its subagents, and reports "running" until the turn's terminal event
// is recorded), no tool call waits on the server, and nothing the server
// runs in it (setup, a publish, a resume, a stop) is in progress. The idle
// time counts from the later of when the worker last became idle (it
// reports idle_since, kept across worker restarts through its log) and the
// last thing the server did in the workspace. Browser connections play no
// part: closing the page neither stops a sandbox nor keeps one running.
//
// Before stopping, Mosso marks the workspace busy, so no message or
// publish can start, and asks the worker once more: if Claude is running,
// nothing is stopped. A message that arrives meanwhile waits for the stop
// and then resumes the sandbox.
//
// The worker cannot stop its own sandbox: that needs a Vercel token, and
// the sandbox, where Claude runs unattended, never gets credentials (see
// docs/worker.md). So idle shutdown runs while Mosso runs. While it does
// not (stopped, or the Mac asleep), the session's own length, which Vercel
// enforces, is the limit; when Mosso is back it stops at once a sandbox
// that has been idle long enough, by the worker's clock.

const (
	// defaultIdleTimeout is a new workspace's idle timeout.
	defaultIdleTimeout = 15 * time.Minute
	minIdleTimeout     = time.Minute
	maxIdleTimeout     = 24 * time.Hour
	// idleCheckInterval is how often Mosso looks for idle workspaces.
	idleCheckInterval = 30 * time.Second
	// stopTimeout bounds one attempt to stop a session.
	stopTimeout = 2 * time.Minute
	// maxStopBackoff is the longest wait between failed stop attempts.
	maxStopBackoff = 15 * time.Minute
)

const idleTimeoutRule = "idle_timeout_seconds must be 0 (never stop an idle sandbox) or between 60 and 86400"

// validIdleTimeout reports whether d is an acceptable idle timeout; 0 turns
// idle shutdown off.
func validIdleTimeout(d time.Duration) bool {
	return d == 0 || d >= minIdleTimeout && d <= maxIdleTimeout
}

// errStillWorking means the sandbox was not stopped because something runs
// in it.
var errStillWorking = errors.New("Claude is working in the sandbox")

// idleLoop stops idle workspaces until Shutdown.
func (s *Server) idleLoop() {
	t := time.NewTicker(s.idleCheck)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.checkIdle(time.Now())
		}
	}
}

// checkIdle stops the sandboxes of the workspaces that have been idle for
// their idle timeout at now, and returns when it has tried.
func (s *Server) checkIdle(now time.Time) {
	s.mu.Lock()
	var due []*workspace
	for _, ws := range s.workspaces {
		if at := s.idleStopAtLocked(ws); !at.IsZero() && !now.Before(at) {
			s.beginStopLocked(ws)
			due = append(due, ws)
		}
	}
	s.mu.Unlock()
	done := make(chan struct{}, len(due))
	for _, ws := range due {
		go func() {
			defer func() { done <- struct{}{} }()
			if err := s.stopSandbox(ws, true, now); err != nil && !errors.Is(err, errStillWorking) && !errors.Is(err, errNotIdleYet) {
				log.Printf("workspace %s: idle shutdown: %v", ws.Name, err)
			}
		}()
	}
	for range due {
		<-done
	}
}

// idleStopAtLocked returns when Mosso stops the workspace's sandbox if it
// stays idle, or the zero time if it would not: idle shutdown is off, the
// sandbox is not running, or something runs in it. Call with s.mu held.
func (s *Server) idleStopAtLocked(ws *workspace) time.Time {
	if s.vercel == nil || ws.idleTimeout <= 0 || ws.sandboxState != sandboxRunning ||
		ws.creating || ws.connecting || ws.stopping || ws.resuming ||
		s.busy[ws.Name] || ws.taskRunning || ws.chat.running != nil || len(ws.toolCalls) > 0 {
		return time.Time{}
	}
	since := ws.lastActive
	if ws.workerIdleSince.After(since) {
		since = ws.workerIdleSince
	}
	if since.IsZero() {
		return time.Time{} // not known yet: wait for the worker to say
	}
	at := since.Add(ws.idleTimeout)
	if ws.nextStopTry.After(at) {
		at = ws.nextStopTry
	}
	return at
}

// touchLocked records that something the worker does not know about just
// happened in the workspace, so its idle time starts again. Call with s.mu
// held.
func (s *Server) touchLocked(ws *workspace) {
	ws.lastActive = time.Now()
}

// beginStopLocked marks the workspace as stopping. It holds the workspace
// busy, so no turn or publish can start until endStopLocked. Call with
// s.mu held.
func (s *Server) beginStopLocked(ws *workspace) {
	ws.stopping = true
	ws.stopDone = make(chan struct{})
	s.busy[ws.Name] = true
	ws.chat.notify()
}

func (s *Server) endStopLocked(ws *workspace) {
	ws.stopping = false
	delete(s.busy, ws.Name)
	close(ws.stopDone)
	ws.chat.notify()
}

// waitForStop waits until a stop in progress, if any, is over.
func (s *Server) waitForStop(ctx context.Context, ws *workspace) error {
	s.mu.Lock()
	stopping, done := ws.stopping, ws.stopDone
	s.mu.Unlock()
	if !stopping {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquireWaiting is acquire for a workspace, except that it waits for a
// stop in progress to end instead of failing: a message sent just as the
// sandbox is being stopped then resumes it.
func (s *Server) acquireWaiting(ws *workspace) bool {
	for range 3 {
		if s.acquire(ws.Name) {
			return true
		}
		ctx, cancel := context.WithTimeout(s.ctx, stopTimeout)
		s.mu.Lock()
		stopping := ws.stopping
		s.mu.Unlock()
		err := s.waitForStop(ctx, ws)
		cancel()
		if !stopping || err != nil {
			return false
		}
	}
	return false
}

// errNotIdleYet means the worker became idle more recently than Mosso
// knew, so the idle timeout has not passed after all.
var errNotIdleYet = errors.New("not idle long enough")

// stopSandbox stops the workspace's sandbox session, which beginStopLocked
// marked as stopping. The worker is asked first: if Claude is running (or
// a tool call is waiting), nothing is stopped and errStillWorking is
// returned. For an idle shutdown, the worker's idle time must also have
// reached the idle timeout. Stopping keeps the sandbox and its snapshot;
// it never deletes anything. A failed stop is tried again later, waiting
// longer after each failure. now is the current time (tests move it on).
func (s *Server) stopSandbox(ws *workspace, idle bool, now time.Time) error {
	defer func() {
		s.mu.Lock()
		s.endStopLocked(ws)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(s.ctx, stopTimeout)
	defer cancel()
	s.mu.Lock()
	session, lastSeq, timeout := ws.Sandbox.SessionID, ws.lastSeq, ws.idleTimeout
	s.mu.Unlock()

	// The worker knows for sure whether Claude is working.
	st, err := s.workerStatus(ctx, session)
	switch {
	case vercel.SessionOver(err):
		s.sessionEnded(ws, session) // it ended by itself meanwhile
		return nil
	case errors.Is(err, errWorkerDown):
		// No worker, so no Claude: the worker runs it, and a worker that
		// starts again marks an unfinished turn interrupted.
	case err != nil:
		s.stopFailed(ws, err, now)
		return fmt.Errorf("ask the sandbox worker before stopping: %w", err)
	case st.State != "idle" || len(st.PendingCalls) > 0:
		s.mu.Lock()
		ws.workerIdleSince = time.Time{}
		s.touchLocked(ws)
		s.mu.Unlock()
		return errStillWorking
	case st.IdleSince != nil:
		s.mu.Lock()
		if st.IdleSince.After(ws.workerIdleSince) {
			ws.workerIdleSince = *st.IdleSince
		}
		s.mu.Unlock()
		if idle && now.Sub(*st.IdleSince) < timeout {
			return errNotIdleYet
		}
	case idle && st.Seq > lastSeq:
		// A worker that does not report when it became idle, with events
		// Mosso has not seen: look again once they are applied.
		return errNotIdleYet
	}

	// Stop following first, so the follower does not take the end of the
	// session for a timeout.
	s.mu.Lock()
	if ws.follower != nil && ws.Sandbox.SessionID == session {
		ws.follower()
		ws.follower = nil
	}
	s.mu.Unlock()
	if err := s.vercel.Stop(ctx, session); err != nil {
		s.stopFailed(ws, err, now)
		// Still running: follow it again (never resuming anything).
		s.mu.Lock()
		if ws.follower == nil && ws.Sandbox.SessionID == session && ws.sandboxState == sandboxRunning {
			fctx, cancel := context.WithCancel(s.ctx)
			ws.follower = cancel
			go s.follow(fctx, ws, session)
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ws.stopFailures, ws.nextStopTry = 0, time.Time{}
	if ws.Sandbox.SessionID != session || ws.sandboxState != sandboxRunning {
		return nil // terminated, or already stopped, meanwhile
	}
	ws.sandboxState = sandboxStopped
	ws.Sandbox.Status = "stopped"
	ws.ExpiresAt = time.Time{}
	ws.workerIdleSince = time.Time{}
	text := "Sandbox stopped. Its files and Claude's conversation are kept; your next message, or Resume, starts it again."
	if idle {
		log.Printf("workspace %s: stopped the sandbox after %s idle", ws.Name, fmtDuration(timeout))
		text = fmt.Sprintf("Sandbox stopped after %s without activity, to save cost. Its files and Claude's conversation are kept; your next message, or Resume, starts it again.", fmtDuration(timeout))
	} else {
		log.Printf("workspace %s: stopped the sandbox", ws.Name)
	}
	ws.chat.add(chatEntry{Type: "notice", Text: text})
	s.persistLocked(ws)
	return nil
}

// stopFailed schedules the next attempt to stop the workspace's sandbox,
// waiting longer after each failure (1, 2, 4, … up to 15 minutes). The
// first failure in a row is shown in the chat.
func (s *Server) stopFailed(ws *workspace, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws.stopFailures++
	wait := min(time.Minute<<min(ws.stopFailures-1, 10), maxStopBackoff)
	ws.nextStopTry = now.Add(wait)
	log.Printf("workspace %s: stop the sandbox: %v (trying again in %s)", ws.Name, err, wait)
	if ws.stopFailures == 1 {
		ws.chat.add(chatEntry{Type: "notice", Text: "Mosso could not stop the idle sandbox (" + err.Error() + "). It keeps running, and Mosso tries again."})
	}
}

// workspaceSandboxStop stops the workspace's sandbox session now (Stop in
// the web UI): compute stops and costs nothing more, and the workspace,
// its snapshot and its conversation are kept, to be resumed by the next
// message. It is refused while Claude or a publish is running: stop Claude
// first.
func (s *Server) workspaceSandboxStop(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByName(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	s.mu.Lock()
	gone := noSandbox(ws.sandboxState)
	switch {
	case gone != "":
		s.mu.Unlock()
		writeError(w, http.StatusGone, gone)
		return
	case ws.creating || ws.connecting || ws.resuming:
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "the sandbox is being set up or resumed; try again in a moment")
		return
	case ws.stopping:
		s.mu.Unlock()
		_ = s.waitForStop(r.Context(), ws)
	case ws.sandboxState == sandboxStopped:
		s.mu.Unlock()
	case s.busy[ws.Name] || ws.taskRunning:
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "Claude or a publish is running in this workspace; stop Claude first, or wait for it to finish")
		return
	default:
		s.beginStopLocked(ws)
		s.mu.Unlock()
		if err := s.stopSandbox(ws, false, time.Now()); err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errStillWorking) {
				status = http.StatusConflict
			}
			writeError(w, status, "the sandbox was not stopped: "+err.Error())
			return
		}
	}
	s.mu.Lock()
	snapshot := s.snapshotLocked(ws)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

// updateWorkspace changes a workspace's settings: for now its idle timeout
// (idle_timeout_seconds, 0 to never stop an idle sandbox). The new timeout
// counts from now. It only touches the database, so it works without
// Vercel; it fails if the change cannot be saved.
func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdleTimeoutSeconds *int `json:"idle_timeout_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.IdleTimeoutSeconds == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: give idle_timeout_seconds")
		return
	}
	idle := time.Duration(*req.IdleTimeoutSeconds) * time.Second
	if !validIdleTimeout(idle) {
		writeError(w, http.StatusBadRequest, idleTimeoutRule)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[r.PathValue("name")]
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	case ws.sandboxState == sandboxTerminated:
		writeError(w, http.StatusConflict, terminatedMessage)
		return
	case ws.creating:
		writeError(w, http.StatusConflict, "the workspace is still being set up")
		return
	}
	before, unsaved := ws.idleTimeout, ws.unsaved
	ws.idleTimeout = idle
	if err := s.saveLocked(ws); err != nil {
		ws.idleTimeout, ws.unsaved = before, unsaved
		log.Printf("workspace %s: save: %v", ws.Name, err)
		writeError(w, http.StatusInternalServerError, "the change could not be saved: "+err.Error())
		return
	}
	s.touchLocked(ws)
	ws.chat.notify()
	writeJSON(w, http.StatusOK, s.snapshotLocked(ws))
}

// fmtDuration writes a duration in hours and minutes, like "45 minutes"
// or "1 hour 30 minutes".
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	var parts []string
	if h > 0 {
		parts = append(parts, unit(h, "hour"))
	}
	if m > 0 || h == 0 {
		parts = append(parts, unit(m, "minute"))
	}
	return strings.Join(parts, " ")
}
