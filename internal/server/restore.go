package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// Sandbox states of a workspace, as last known to this server.
const (
	// sandboxRunning: the current session is running (or starting).
	sandboxRunning = "running"
	// sandboxStopped: the session ended (timeout or stop); the persistent
	// sandbox resumes from its snapshot on the next message or connect.
	sandboxStopped = "stopped"
	// sandboxGone: the sandbox was deleted or can no longer resume. Its
	// chat history is still kept here until the workspace is terminated.
	sandboxGone = "gone"
)

const goneMessage = "The sandbox no longer exists: it was deleted, or its snapshot expired. The conversation is kept here; terminate this workspace to remove it."

// resumeTimeout bounds resuming a sandbox and starting its worker.
const resumeTimeout = 3 * time.Minute

// Restore opens Mosso's data directory, loads the workspaces saved there
// and reconnects to their sandboxes in the background: a running sandbox
// is followed again from the last event Mosso saw; a stopped one whose
// Claude task was still running when Mosso last saw it is resumed to fetch
// what happened meanwhile; other stopped sandboxes are left stopped (and
// unbilled) until the next message or connect. New workspaces are saved
// there too.
func (s *Server) Restore(ctx context.Context, dataDir string) error {
	st, err := openStore(dataDir)
	if err != nil {
		return err
	}
	records, err := st.load()
	if err != nil {
		return fmt.Errorf("load workspaces: %w", err)
	}
	s.mu.Lock()
	s.store = st
	s.mu.Unlock()
	for _, rec := range records {
		ws, err := s.restoreWorkspace(rec)
		if err != nil {
			log.Printf("workspace %s: not restored: %v", rec.Name, err)
			continue
		}
		log.Printf("workspace %s: restored (%s, %d chat entries, sandbox %s)", ws.Name, ws.Repo, len(ws.chat.entries), rec.SandboxState)
		go s.reconnect(ws, rec.SandboxState, rec.TaskRunning)
	}
	return nil
}

// restoreWorkspace rebuilds a workspace from its record and chat log.
func (s *Server) restoreWorkspace(rec workspaceRecord) (*workspace, error) {
	if s.vercel == nil {
		return nil, errors.New("Vercel is not configured")
	}
	entries, err := s.store.loadEntries(rec.Name)
	if err != nil {
		return nil, err
	}
	ws := &workspace{
		Name: rec.Name, Repo: rec.Repo, Base: rec.Base, BaseSHA: rec.BaseSHA, Branch: rec.Branch, Dir: rec.Dir,
		Sandbox: rec.Sandbox, ExpiresAt: rec.ExpiresAt, PullRequest: rec.PullRequest, CreatedAt: rec.CreatedAt,
		chat:         newChat(),
		attachments:  map[string]attachment{},
		lastSeq:      rec.LastSeq,
		sandboxState: rec.SandboxState,
		connecting:   true,
	}
	if ws.sandboxState == "" {
		ws.sandboxState = sandboxStopped
	}
	for _, a := range rec.Attachments {
		ws.attachments[a.ID] = a
		ws.attachBytes += a.Size
	}
	ws.chat.load(entries)
	ws.chat.sessionID = rec.SessionID
	for _, e := range entries {
		if e.Seq > ws.lastSeq {
			ws.lastSeq = e.Seq
		}
	}
	ws.chat.persist = s.persister(ws.Name)
	if rec.BaseSHA != "" {
		mirror, err := github.NewMirror(s.vercel, rec.Sandbox.SessionID, github.MirrorOptions{
			Repo: rec.Repo, Base: rec.Base, BaseSHA: rec.BaseSHA, Branch: rec.Branch, Dir: rec.Dir, Pushed: rec.Pushed,
		})
		if err != nil {
			return nil, err
		}
		ws.git = mirror
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.workspaces[rec.Name]; exists {
		return nil, errors.New("already loaded")
	}
	s.workspaces[rec.Name] = ws
	s.sandboxes[rec.Name] = rec.Sandbox
	if rec.TaskRunning && ws.sandboxState != sandboxGone {
		// Claude may still be working, or have finished while Mosso was
		// away; the follower finds out.
		s.busy[rec.Name] = true
		ws.taskRunning = true
	}
	return ws, nil
}

// reconnect is Restore's background step for one workspace.
func (s *Server) reconnect(ws *workspace, lastState string, taskRunning bool) {
	ctx, cancel := context.WithTimeout(s.ctx, resumeTimeout)
	defer cancel()
	defer func() {
		s.mu.Lock()
		ws.connecting = false
		ws.chat.notify()
		s.mu.Unlock()
	}()
	if lastState == sandboxGone {
		return
	}
	sb, err := s.vercel.Get(ctx, ws.Name, false)
	switch {
	case ctx.Err() != nil:
		return
	case vercel.IsNotFound(err) || vercel.SnapshotGone(err):
		s.markGone(ws)
		return
	case err != nil:
		log.Printf("workspace %s: check the sandbox: %v (will try again on the next message)", ws.Name, err)
		return
	}
	s.mu.Lock()
	ws.Sandbox = *sb
	if sb.Running() {
		ws.sandboxState = sandboxRunning
		ws.ExpiresAt = sb.ExpiresAt()
	} else {
		ws.sandboxState = sandboxStopped
		ws.ExpiresAt = time.Time{}
	}
	s.sandboxes[ws.Name] = *sb
	s.persistLocked(ws)
	s.mu.Unlock()
	if sb.Running() || taskRunning {
		if err := s.connect(ctx, ws); err != nil && ctx.Err() == nil {
			log.Printf("workspace %s: reconnect: %v", ws.Name, err)
		}
	}
}

// connect makes the workspace's sandbox usable: it resumes the sandbox if
// its session has ended, makes sure the worker runs, and follows the
// worker's events from where Mosso left off. It is safe to call often.
func (s *Server) connect(ctx context.Context, ws *workspace) error {
	s.mu.Lock()
	state := ws.sandboxState
	expired := !ws.ExpiresAt.IsZero() && time.Now().After(ws.ExpiresAt)
	s.mu.Unlock()
	if state == sandboxGone {
		return errors.New(goneMessage)
	}
	if state != sandboxRunning || expired {
		if err := s.resume(ctx, ws); err != nil {
			return err
		}
	}
	s.mu.Lock()
	session := ws.Sandbox.SessionID
	s.mu.Unlock()
	if _, err := s.ensureWorker(ctx, ws.Name, session); err != nil {
		if vercel.SessionOver(err) {
			s.sessionEnded(ws, session)
		}
		return fmt.Errorf("connect to the sandbox worker: %w", err)
	}
	s.mu.Lock()
	if ws.follower == nil && ws.Sandbox.SessionID == session && ws.sandboxState == sandboxRunning {
		fctx, cancel := context.WithCancel(s.ctx)
		ws.follower = cancel
		go s.follow(fctx, ws, session)
	}
	s.mu.Unlock()
	return nil
}

// resume boots a new session of the stopped persistent sandbox. The
// filesystem (checkout, worker log, Claude Code transcripts) comes back
// from the last snapshot; the worker is started again by connect.
func (s *Server) resume(ctx context.Context, ws *workspace) error {
	s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Resuming the sandbox from its last snapshot…"})
	var sb *vercel.Sandbox
	var err error
	for attempt := 0; ; attempt++ {
		sb, err = s.vercel.Get(ctx, ws.Name, true)
		if err == nil || !vercel.InTransition(err) || attempt >= 20 {
			break
		}
		// Stopping or snapshotting: wait for it.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	switch {
	case vercel.IsNotFound(err) || vercel.SnapshotGone(err):
		s.markGone(ws)
		return errors.New(goneMessage)
	case err != nil:
		return fmt.Errorf("resume the sandbox: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resumed := sb.SessionID != ws.Sandbox.SessionID
	ws.Sandbox = *sb
	ws.sandboxState = sandboxRunning
	ws.ExpiresAt = sb.ExpiresAt()
	if ws.ExpiresAt.IsZero() {
		ws.ExpiresAt = time.Now().Add(sb.Timeout)
	}
	ws.attachDirReady = false
	s.sandboxes[ws.Name] = *sb
	delete(s.claudeInstalled, ws.Name) // the install script is idempotent and quick
	if ws.git != nil {
		ws.git.SetSandboxSession(sb.SessionID)
	}
	if resumed {
		log.Printf("workspace %s: sandbox resumed (session %s, expires %s)", ws.Name, sb.SessionID, ws.ExpiresAt.Format(time.RFC3339))
		if ws.follower != nil {
			// The old follower is bound to the old session.
			ws.follower()
			ws.follower = nil
		}
	}
	s.persistLocked(ws)
	return nil
}

// follow reads the worker's events for the session and records them, for
// as long as the session lasts. It restarts the worker if it is not
// running (after a resume), and reconnects after transient errors.
func (s *Server) follow(ctx context.Context, ws *workspace, session string) {
	defer func() {
		s.mu.Lock()
		if ws.Sandbox.SessionID == session && ws.follower != nil {
			ws.follower()
			ws.follower = nil
		}
		s.mu.Unlock()
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		st, err := s.ensureWorker(ctx, ws.Name, session)
		if err == nil {
			s.reconcile(ws, st)
			s.mu.Lock()
			after := ws.lastSeq
			s.mu.Unlock()
			err = s.workerFollow(ctx, session, after, func(e worker.Event) { s.applyEvent(ws, e) })
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		if vercel.SessionOver(err) {
			s.sessionEnded(ws, session)
			return
		}
		log.Printf("workspace %s: follow the worker: %v (retrying in %s)", ws.Name, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// reconcile squares what Mosso believes with the worker's status before
// following its events.
func (s *Server) reconcile(ws *workspace, st worker.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Seq < ws.lastSeq {
		// The sandbox came back from an older snapshot than Mosso had
		// seen; follow from what it has rather than wait forever.
		log.Printf("workspace %s: the worker has %d events but Mosso had seen %d; the sandbox restored an older state", ws.Name, st.Seq, ws.lastSeq)
		ws.chat.add(chatEntry{Type: "notice", Text: "The sandbox resumed from a snapshot older than what Mosso had already recorded; recent events may appear again."})
		ws.lastSeq = st.Seq
	}
	// A turn Mosso is still submitting is not in the worker yet; one the
	// worker has, or one from an earlier Mosso process (run == nil), should
	// be running or have left a terminal event.
	run := ws.chat.running
	submitted := run == nil || run.taskID != ""
	if ws.taskRunning && submitted && st.State == "idle" && st.Seq <= ws.lastSeq {
		// Nothing more will come for the task Mosso was waiting on.
		ws.chat.add(chatEntry{Type: "notice", Text: "Interrupted: Claude is no longer running in the sandbox and left no result for the last message. Send a follow-up to continue."})
		s.finishTurnLocked(ws, ws.chat.running)
	}
	if st.SessionID != "" && ws.chat.sessionID == "" {
		ws.chat.sessionID = st.SessionID
	}
}

// sessionEnded records that the sandbox session is over (it timed out, was
// stopped, or the sandbox is gone). A turn in progress is interrupted here;
// what Claude saved before the end is caught up from the worker's log when
// the sandbox resumes.
func (s *Server) sessionEnded(ws *workspace, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gone := false
	if _, err := s.vercel.Get(ctx, ws.Name, false); vercel.IsNotFound(err) || vercel.SnapshotGone(err) {
		gone = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws.Sandbox.SessionID != session {
		return // already resumed into a new session
	}
	if gone {
		s.markGoneLocked(ws)
		return
	}
	if ws.sandboxState == sandboxRunning {
		log.Printf("workspace %s: the sandbox session ended; it resumes on the next message", ws.Name)
	}
	ws.sandboxState = sandboxStopped
	ws.Sandbox.Status = "stopped"
	ws.ExpiresAt = time.Time{}
	if ws.taskRunning {
		if ws.chat.running != nil {
			ws.interruptedTask = ws.chat.running.taskID
		}
		ws.chat.add(chatEntry{Type: "notice", Text: "Interrupted: the sandbox session ended while Claude was working. What Claude saved before that is in the sandbox and shown here when it resumes; send a follow-up to let Claude continue."})
		s.finishTurnLocked(ws, ws.chat.running)
	}
	ws.chat.notify()
	s.persistLocked(ws)
}

func (s *Server) markGone(ws *workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markGoneLocked(ws)
}

func (s *Server) markGoneLocked(ws *workspace) {
	if ws.sandboxState == sandboxGone {
		return
	}
	log.Printf("workspace %s: the sandbox is gone", ws.Name)
	ws.sandboxState = sandboxGone
	ws.ExpiresAt = time.Time{}
	if ws.follower != nil {
		ws.follower()
		ws.follower = nil
	}
	if ws.taskRunning {
		ws.chat.add(chatEntry{Type: "notice", Text: "Interrupted: the sandbox is gone, so the rest of Claude's work on the last message was lost."})
		s.finishTurnLocked(ws, ws.chat.running)
	}
	ws.chat.add(chatEntry{Type: "notice", Text: goneMessage})
	s.persistLocked(ws)
}

// persister returns the chat's persist callback for a workspace.
func (s *Server) persister(name string) func(chatEntry) {
	return func(e chatEntry) {
		if s.store == nil {
			return
		}
		if err := s.store.appendEntry(name, e); err != nil {
			log.Printf("workspace %s: save chat entry: %v", name, err)
		}
	}
}

// persistLocked saves the workspace's record. Call with s.mu held.
func (s *Server) persistLocked(ws *workspace) {
	if s.store == nil || ws.creating {
		return
	}
	rec := workspaceRecord{
		Name: ws.Name, Repo: ws.Repo, Base: ws.Base, BaseSHA: ws.BaseSHA, Branch: ws.Branch, Dir: ws.Dir,
		Sandbox: ws.Sandbox, SandboxState: ws.sandboxState, ExpiresAt: ws.ExpiresAt, PullRequest: ws.PullRequest,
		Pushed: ws.pushed, SessionID: ws.chat.sessionID, LastSeq: ws.lastSeq, TaskRunning: ws.taskRunning,
		CreatedAt: ws.CreatedAt,
	}
	for _, a := range ws.attachments {
		rec.Attachments = append(rec.Attachments, a)
	}
	if err := s.store.save(rec); err != nil {
		log.Printf("workspace %s: save: %v", ws.Name, err)
	}
}

// workspaceConnect resumes a stopped workspace's sandbox, restarts its
// worker and catches up on events, so attachments can be read and the
// chat continues without waiting for a message to do it.
func (s *Server) workspaceConnect(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceByName(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	s.mu.Lock()
	gone, creating := ws.sandboxState == sandboxGone, ws.creating
	s.mu.Unlock()
	switch {
	case gone:
		writeError(w, http.StatusGone, goneMessage)
		return
	case creating:
		writeError(w, http.StatusConflict, "the workspace is still being set up")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resumeTimeout)
	defer cancel()
	if err := s.connect(ctx, ws); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	snapshot := s.snapshotLocked(ws)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}
