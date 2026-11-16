package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/riggleg/mosso/internal/claude"
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
	// sandboxGone: the sandbox was deleted outside Mosso or can no longer
	// resume. The workspace stays, read-only, until it is terminated.
	sandboxGone = "gone"
	// sandboxTerminated: the user terminated the workspace and Mosso
	// deleted its sandbox. The workspace and its chat are kept as history.
	sandboxTerminated = "terminated"
)

const (
	goneMessage       = "The sandbox no longer exists: it was deleted, or its snapshot expired. The conversation is kept; terminate the workspace to move it to the history."
	terminatedMessage = "This workspace was terminated and its sandbox deleted. The conversation is kept as history."
)

// noSandbox says why a workspace in the given sandbox state has no sandbox
// to work in any more, or "" if it has one.
func noSandbox(state string) string {
	switch state {
	case sandboxGone:
		return goneMessage
	case sandboxTerminated:
		return terminatedMessage
	}
	return ""
}

// resumeTimeout bounds resuming a sandbox and starting its worker.
const resumeTimeout = 3 * time.Minute

// Restore opens Mosso's database at dbPath (creating it if needed) and
// does two separate things with the workspaces saved there:
//
//   - It loads them all, with their state as last saved, so they are listed
//     and their conversations can be read. This needs nothing but the
//     database: it works without Vercel credentials.
//   - It reconnects the ones that have a sandbox, in the background (see
//     reconnect). This needs Vercel and is skipped without it; those
//     workspaces then stay as they were last saved, read-only, until Mosso
//     is started with credentials.
//
// New workspaces are saved in the database too.
//
// legacyDir, if not empty, is the "workspaces" directory in which a Mosso
// from before the database saved workspaces as files; any found there are
// imported first (see importFiles).
func (s *Server) Restore(ctx context.Context, dbPath, legacyDir string) error {
	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	if legacyDir != "" {
		if n, err := st.importFiles(legacyDir); err != nil {
			log.Printf("import the workspaces an earlier Mosso saved in %s: %v (imported %d; the rest is tried again on the next start)", legacyDir, err, n)
		} else if n > 0 {
			log.Printf("imported %d workspaces an earlier Mosso saved in %s into the database", n, legacyDir)
		}
	}
	records, err := st.load()
	if err != nil {
		st.close()
		return fmt.Errorf("load workspaces: %w", err)
	}
	model, err := st.setting(settingDefaultModel)
	if err != nil {
		st.close()
		return fmt.Errorf("load settings: %w", err)
	}
	s.mu.Lock()
	s.store = st
	switch {
	case claude.KnownModel(model):
		s.defaultModel = model
	case model != "":
		log.Printf("the saved default model %q is no longer offered; using %s until another is chosen", model, s.defaultModel)
	}
	s.mu.Unlock()

	// Load: every workspace, from the database alone.
	type loaded struct {
		ws  *workspace
		rec workspaceRecord
	}
	var active []loaded // the ones with a sandbox to reconnect to
	for _, rec := range records {
		ws, err := s.loadWorkspace(rec)
		if err != nil {
			log.Printf("workspace %s: not loaded: %v", rec.Name, err)
			continue
		}
		if rec.SandboxState != sandboxTerminated {
			active = append(active, loaded{ws, rec})
		}
	}
	log.Printf("loaded %d workspaces from %s (%d with a sandbox, %d in the history)", len(records), dbPath, len(active), len(records)-len(active))

	// Reconnect: only with a sandbox provider.
	if s.vercel == nil {
		if len(active) > 0 {
			log.Printf("%d workspaces with a sandbox are shown as last saved and are not reconnected: Vercel is not configured", len(active))
		}
		return nil
	}
	for _, l := range active {
		go s.reconnect(l.ws, l.rec.SandboxState, l.rec.TaskRunning)
	}
	return nil
}

// loadWorkspace rebuilds a workspace from its record, as last saved, and
// lists it. It does not contact the sandbox; reconnect does.
func (s *Server) loadWorkspace(rec workspaceRecord) (*workspace, error) {
	ws := &workspace{
		Name: rec.Name, Repo: rec.Repo, Base: rec.Base, BaseSHA: rec.BaseSHA, Branch: rec.Branch, Dir: rec.Dir, Model: rec.Model,
		Sandbox: rec.Sandbox, ExpiresAt: rec.ExpiresAt, PullRequest: rec.PullRequest, CreatedAt: rec.CreatedAt,
		ArchivedAt:   rec.ArchivedAt,
		chat:         newChat(),
		attachments:  map[string]attachment{},
		lastSeq:      rec.LastSeq,
		sandboxState: rec.SandboxState,
		pushed:       rec.Pushed,
		queuedPrompt: rec.QueuedPrompt,
	}
	if ws.sandboxState == "" {
		ws.sandboxState = sandboxStopped
	}
	for _, a := range rec.Attachments {
		ws.attachments[a.ID] = a
		ws.attachBytes += a.Size
	}
	ws.chat.sessionID = rec.SessionID
	ws.chat.persist = s.persister(ws)
	if ws.sandboxState == sandboxTerminated {
		// History: nothing more happens in it, so its chat stays in the
		// database until someone opens it (see loadChatLocked).
		ws.chat.unloaded = true
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.workspaces[rec.Name]; exists {
			return nil, errors.New("already loaded")
		}
		s.workspaces[rec.Name] = ws
		return ws, nil
	}
	entries, err := s.store.loadEntries(rec.Name)
	if err != nil {
		return nil, err
	}
	ws.chat.load(entries)
	for _, e := range entries {
		if e.Seq > ws.lastSeq {
			ws.lastSeq = e.Seq
		}
	}
	// Whether a Claude task was in flight is kept as saved, so that it is
	// still known the next time Mosso can reach the sandbox.
	ws.taskRunning = rec.TaskRunning && ws.sandboxState != sandboxGone
	// The rest prepares for reconnecting, which needs Vercel.
	reconnecting := s.vercel != nil
	ws.connecting = reconnecting
	if rec.BaseSHA != "" && reconnecting {
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
	if ws.taskRunning && reconnecting {
		// Claude may still be working, or have finished while Mosso was
		// away; the follower finds out.
		s.busy[rec.Name] = true
	}
	return ws, nil
}

// reconnect is Restore's background step for one workspace that has a
// sandbox: it asks Vercel what became of the sandbox and squares the
// workspace with it.
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
	if ws.sandboxState == sandboxTerminated {
		s.mu.Unlock()
		return // terminated while Mosso was checking
	}
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
	queued := ws.queuedPrompt
	s.mu.Unlock()
	// A queued first message is what the user is waiting for: resume a
	// stopped sandbox for it too.
	if sb.Running() || taskRunning || queued {
		if err := s.connect(ctx, ws); err != nil {
			if ctx.Err() == nil {
				log.Printf("workspace %s: reconnect: %v", ws.Name, err)
			}
			return
		}
	}
	if queued {
		s.sendQueuedPrompt(ctx, ws)
	}
}

// sendQueuedPrompt recovers a first message that Mosso saved but stopped
// before the worker confirmed it. The worker runs nothing before the first
// message, so its event log tells: empty, the message never reached it and
// is sent now; otherwise the worker has it, and the follower matches the
// worker's copy to the saved entry (see applyEvent) so it runs only once.
func (s *Server) sendQueuedPrompt(ctx context.Context, ws *workspace) {
	s.mu.Lock()
	session := ws.Sandbox.SessionID
	s.mu.Unlock()
	st, err := s.workerStatus(ctx, session)
	if err != nil {
		log.Printf("workspace %s: check the worker for the first message: %v (tried again at the next start)", ws.Name, err)
		return
	}
	s.mu.Lock()
	idx := queuedEntry(ws.chat)
	if !ws.queuedPrompt || st.Seq > 0 || idx < 0 || ws.chat.running != nil {
		s.mu.Unlock()
		return
	}
	prompt := ws.chat.entries[idx].Text
	// Saved as running if Mosso stopped while submitting; nothing runs.
	s.finishTurnLocked(ws, nil)
	s.mu.Unlock()
	token, _, err := s.claudeToken(ctx)
	if err == nil {
		log.Printf("workspace %s: sending the first message, which had not reached the worker before Mosso stopped", ws.Name)
		if _, ok := s.startTurn(ws, token, prompt, nil, nil, idx); ok {
			return
		}
		err = errors.New("another message is running")
	}
	s.mu.Lock()
	ws.queuedPrompt = false
	ws.chat.add(chatEntry{Type: "error", Text: "Your first message could not be sent after Mosso restarted (" + err.Error() + "). Send it again."})
	s.persistLocked(ws)
	s.mu.Unlock()
}

// queuedEntry returns the index of the queued first message: the first
// user entry, while no worker event has been matched to it; -1 if none.
func queuedEntry(c *chat) int {
	for i, e := range c.entries {
		if e.Type == "user" {
			if e.Seq == 0 {
				return i
			}
			return -1
		}
	}
	return -1
}

// connect makes the workspace's sandbox usable: it resumes the sandbox if
// its session has ended, makes sure the worker runs, and follows the
// worker's events from where Mosso left off. It is safe to call often.
func (s *Server) connect(ctx context.Context, ws *workspace) error {
	s.mu.Lock()
	state := ws.sandboxState
	expired := !ws.ExpiresAt.IsZero() && time.Now().After(ws.ExpiresAt)
	s.mu.Unlock()
	if msg := noSandbox(state); msg != "" {
		return errors.New(msg)
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
	if ws.sandboxState == sandboxTerminated {
		// Terminated while resuming: the sandbox is being deleted.
		return errors.New(terminatedMessage)
	}
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
	// A queued first message the worker never got is not interrupted: the
	// reconnect sends it (sendQueuedPrompt).
	if ws.queuedPrompt && st.Seq == 0 {
		submitted = false
	}
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
	if ws.Sandbox.SessionID != session || ws.sandboxState == sandboxTerminated {
		return // already resumed into a new session, or terminated
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
	if noSandbox(ws.sandboxState) != "" {
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

// persister returns the chat's persist callback for a workspace: it saves
// the chat entries not saved yet. Nothing is saved of a workspace that is
// still being set up, so a failed setup leaves nothing behind. A failure
// is logged; the entries go out with the next save.
func (s *Server) persister(ws *workspace) func() {
	return func() {
		if s.store == nil || ws.creating {
			return
		}
		c := ws.chat
		if err := s.store.appendEntries(ws.Name, c.entries[c.saved:]); err != nil {
			log.Printf("workspace %s: save the chat: %v", ws.Name, err)
			ws.unsaved = true
			return
		}
		c.saved = len(c.entries)
	}
}

// saveLocked saves the workspace in the database: its record and the chat
// entries not saved yet, all or nothing. Requests that create, terminate
// or archive a workspace use it and fail if it does. While a save has
// failed and none has succeeded since, the workspace is marked unsaved.
// Call with s.mu held.
func (s *Server) saveLocked(ws *workspace) error {
	if s.store == nil {
		return nil // no database: workspaces live in memory only
	}
	rec := workspaceRecord{
		Name: ws.Name, Repo: ws.Repo, Base: ws.Base, Branch: ws.Branch, Model: ws.Model, SandboxState: ws.sandboxState,
		CreatedAt: ws.CreatedAt, ArchivedAt: ws.ArchivedAt,
		workspaceState: workspaceState{
			BaseSHA: ws.BaseSHA, Dir: ws.Dir, Sandbox: ws.Sandbox, ExpiresAt: ws.ExpiresAt, PullRequest: ws.PullRequest,
			Pushed: ws.pushed, SessionID: ws.chat.sessionID, LastSeq: ws.lastSeq, TaskRunning: ws.taskRunning,
			QueuedPrompt: ws.queuedPrompt,
		},
	}
	for _, a := range ws.attachments {
		rec.Attachments = append(rec.Attachments, a)
	}
	c := ws.chat
	if err := s.store.save(rec, c.entries[c.saved:]...); err != nil {
		ws.unsaved = true
		return err
	}
	c.saved = len(c.entries)
	ws.unsaved = false
	return nil
}

// persistLocked saves the workspace after a change that no request is
// waiting on (a worker event, a session ending). A failure is logged and
// the next save writes the current state again. Call with s.mu held.
func (s *Server) persistLocked(ws *workspace) {
	if ws.creating {
		return
	}
	if err := s.saveLocked(ws); err != nil {
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
	gone, creating := noSandbox(ws.sandboxState), ws.creating
	s.mu.Unlock()
	switch {
	case gone != "":
		writeError(w, http.StatusGone, gone)
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
