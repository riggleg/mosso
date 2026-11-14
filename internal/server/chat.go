package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/worker"
)

// streamPing is how often an idle event stream sends a keep-alive comment.
const streamPing = 15 * time.Second

// maxChatBytes bounds the memory one chat log uses. Past it, tool inputs and
// outputs are no longer kept; Claude's progress is still recorded.
const maxChatBytes = 32 << 20

// omitted replaces tool data once a chat log is full.
const omitted = "[mosso: not kept, the chat log is full]"

// chat is a workspace's conversation with Claude Code: a log of prompts,
// Claude's activity and pull request results that every browser tab
// replays. Entries are appended to the workspace's chat.jsonl on disk as
// they are added, so the log survives a Mosso restart; Claude's activity
// arrives from the sandbox worker's event log (see remote.go), so what
// happened while Mosso was away is caught up from there. All fields are
// guarded by Server.mu.
type chat struct {
	// sessionID is the Claude Code session that follow-ups resume.
	sessionID string
	entries   []chatEntry
	// changed is closed and replaced whenever the chat or the workspace's
	// status changes, waking every event stream.
	changed chan struct{}
	running *turnRun // the Claude Code turn in progress, if any
	err     string   // why the last turn or publish failed
	size    int      // approximate bytes held by entries
	full    bool     // size reached maxChatBytes
	// persist, if set, is called with each appended entry.
	persist func(chatEntry)
}

func newChat() *chat { return &chat{changed: make(chan struct{})} }

func (c *chat) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// load replaces the entries with ones read from disk.
func (c *chat) load(entries []chatEntry) {
	c.entries = entries
	c.size, c.full = 0, false
	for _, e := range entries {
		c.size += entrySize(e)
	}
	c.full = c.size >= maxChatBytes
}

func entrySize(e chatEntry) int {
	n := len(e.Text)
	if e.Event != nil {
		n += len(e.Event.Text) + len(e.Event.Input) + len(e.Event.Content)
	}
	if e.Result != nil {
		n += len(e.Result.Text)
	}
	return n
}

func (c *chat) add(e chatEntry) *chatEntry {
	n := entrySize(e)
	if c.size+n > maxChatBytes && e.Event != nil {
		if !c.full {
			c.full = true
			c.append(chatEntry{Type: "notice", Text: "This chat reached Mosso's size limit, so further tool inputs and outputs are not shown. Claude keeps working."})
		}
		ev := *e.Event
		ev.Input = nil
		if ev.Content != "" {
			ev.Content = omitted
		}
		e.Event = &ev
		n = len(ev.Text)
	}
	c.size += n
	return c.append(e)
}

func (c *chat) append(e chatEntry) *chatEntry {
	e.ID = len(c.entries)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	c.entries = append(c.entries, e)
	if c.persist != nil {
		c.persist(e)
	}
	c.notify()
	return &c.entries[len(c.entries)-1]
}

// chatEntry is one item in a chat log. Its ID is its index, used as the
// event stream's event ID so a reconnecting browser resumes where it was.
type chatEntry struct {
	ID   int       `json:"id"`
	Time time.Time `json:"time"`
	// Type is "user" (a prompt), "claude" (an Event: text, tool use or
	// tool result), "result" (a finished turn), "error" (a turn or
	// publish that failed), "notice" or "pull_request".
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Event       *claude.Event     `json:"event,omitempty"`
	Result      *turnResult       `json:"result,omitempty"`
	PullRequest *github.Published `json:"pull_request,omitempty"`
	// Attachments are the files sent with a "user" entry.
	Attachments []attachment `json:"attachments,omitempty"`
	// Seq is the sandbox worker event this entry came from, if any.
	Seq int64 `json:"seq,omitempty"`
}

// turnResult is how a Claude Code turn ended.
type turnResult struct {
	Text         string  `json:"text"`
	IsError      bool    `json:"is_error"`
	Subtype      string  `json:"subtype,omitempty"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func (s *Server) addEntry(c *chat, e chatEntry) {
	s.mu.Lock()
	c.add(e)
	s.mu.Unlock()
}

// turnRun is a Claude Code turn in progress: submitted to the sandbox
// worker (or about to be), and finished when the worker reports its end.
type turnRun struct {
	done chan struct{} // closed when the turn has ended and been recorded
	// taskID is the worker's ID for the turn, once submitted.
	taskID string
	// userIdx is the index of the turn's user entry in the chat.
	userIdx int
	stopped bool // the user asked to stop it; guarded by Server.mu
	// turn and err are how it ended, for the synchronous endpoint.
	turn *claude.Turn
	err  error
}

// startTurn records prompt in the workspace's chat and submits a Claude
// Code turn to the sandbox worker in the background. The turn belongs to
// the worker, not to a request or to this process: it keeps running if the
// browser disconnects or Mosso stops, until Claude finishes, the user stops
// it, or the sandbox session ends. A nil sessionID continues the chat's
// conversation. It reports false if something else is running in the
// workspace.
func (s *Server) startTurn(ws *workspace, token claude.Token, prompt string, atts []attachment, sessionID *string) (*turnRun, bool) {
	if !s.acquire(ws.Name) {
		return nil, false
	}
	run := &turnRun{done: make(chan struct{}), userIdx: -1}
	c := ws.chat
	s.mu.Lock()
	sid := c.sessionID
	if sessionID != nil {
		sid = *sessionID
	}
	c.running, c.err = run, ""
	ws.taskRunning = true
	run.userIdx = c.add(chatEntry{Type: "user", Text: prompt, Attachments: atts}).ID
	s.persistLocked(ws)
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, setupTimeout)
		defer cancel()
		if err := s.submitTurn(ctx, ws, run, token, prompt, atts, sid); err != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			if run.stopped {
				c.add(chatEntry{Type: "notice", Text: "Stopped. Send a message to continue the conversation."})
			} else {
				// Prompt text is deliberately not logged.
				log.Printf("claude in workspace %s: %v", ws.Name, err)
				c.err = err.Error()
				c.add(chatEntry{Type: "error", Text: err.Error()})
			}
			run.err = err
			s.finishTurnLocked(ws, run)
		}
	}()
	return run, true
}

// submitTurn gets the sandbox and worker ready and hands them the message.
// From then on the worker runs Claude and the follower records the events.
func (s *Server) submitTurn(ctx context.Context, ws *workspace, run *turnRun, token claude.Token, prompt string, atts []attachment, sid string) error {
	if err := s.connect(ctx, ws); err != nil {
		return err
	}
	s.mu.Lock()
	session := ws.Sandbox.SessionID
	stopped := run.stopped
	s.mu.Unlock()
	if stopped {
		return errors.New("stopped before it started")
	}
	// Claude Code was installed when the workspace was created and the
	// install persists with the sandbox; this is a quick check unless the
	// install failed then.
	if err := s.ensureClaude(ctx, ws.Name, session); err != nil {
		return err
	}
	s.syncBeforeTurn(ctx, ws)
	s.mu.Lock()
	stopped = run.stopped
	s.mu.Unlock()
	if stopped {
		return errors.New("stopped before it started")
	}
	attsJSON, _ := json.Marshal(atts)
	if len(atts) == 0 {
		attsJSON = nil
	}
	acc, err := s.workerSubmit(ctx, session, worker.Message{
		Text:         prompt,
		Attachments:  attsJSON,
		Prompt:       claudePrompt(prompt, atts),
		SessionID:    sid,
		Dir:          ws.Dir,
		SystemPrompt: workspacePrompt(ws),
		Token:        token.Reveal(),
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run.taskID = acc.TaskID
	// The follower may already have delivered the worker's user event.
	if acc.Seq > ws.lastSeq {
		ws.lastSeq = acc.Seq
		if run.userIdx >= 0 && run.userIdx < len(ws.chat.entries) {
			ws.chat.entries[run.userIdx].Seq = acc.Seq
		}
	}
	s.persistLocked(ws)
	if run.stopped {
		// Stop arrived while the message was on its way.
		go s.stopTask(ws.Sandbox.SessionID)
	}
	return nil
}

// stopTask asks the worker to interrupt its task, in the background.
func (s *Server) stopTask(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.workerStop(ctx, sessionID); err != nil && !errors.Is(err, errWorkerDown) {
		log.Printf("stop claude: %v", err)
	}
}

// applyEvent records one worker event in the chat. Events already applied
// (by sequence number) are skipped, so replays after a reconnect or a
// restart never duplicate messages.
func (s *Server) applyEvent(ws *workspace, e worker.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Seq <= ws.lastSeq {
		return
	}
	ws.lastSeq = e.Seq
	c := ws.chat
	// run is the turn in progress, if the event belongs to it. Events of an
	// earlier task (for example the worker's "interrupted" marker for a
	// task that ended while the sandbox was stopped) are recorded in the
	// chat but must not end a turn that is being submitted now. A turn from
	// an earlier Mosso process has no run and is finished by any terminal
	// event, since the worker runs one task at a time.
	run := c.running
	if run != nil && (run.taskID == "" || run.taskID != e.TaskID) && e.Type != worker.EventUser {
		run = nil
	}
	switch e.Type {
	case worker.EventUser:
		if run != nil && (run.taskID == "" || run.taskID == e.TaskID) && run.userIdx >= 0 && run.userIdx < len(c.entries) && c.entries[run.userIdx].Seq == 0 {
			c.entries[run.userIdx].Seq = e.Seq // the entry Mosso added when submitting
			if run.taskID == "" {
				run.taskID = e.TaskID
			}
			break
		}
		var atts []attachment
		_ = json.Unmarshal(e.Attachments, &atts)
		c.add(chatEntry{Type: "user", Text: e.Text, Attachments: atts, Seq: e.Seq, Time: e.Time})
	case worker.EventSession:
		if e.SessionID != "" {
			c.sessionID = e.SessionID
		}
	case worker.EventClaude:
		if e.Event != nil {
			c.add(chatEntry{Type: "claude", Event: e.Event, Seq: e.Seq, Time: e.Time})
			if run != nil {
				if run.turn == nil {
					run.turn = &claude.Turn{Events: []claude.Event{}}
				}
				run.turn.Events = append(run.turn.Events, *e.Event)
			}
		}
	case worker.EventResult:
		r := e.Result
		if r == nil {
			r = &worker.Result{}
		}
		if e.SessionID != "" {
			c.sessionID = e.SessionID
		}
		log.Printf("claude in workspace %s: session %s, is_error=%t", ws.Name, c.sessionID, r.IsError)
		if r.IsError {
			c.err = "Claude Code reported an error"
			if r.Text != "" {
				c.err = r.Text
			}
		}
		c.add(chatEntry{Type: "result", Seq: e.Seq, Time: e.Time, Result: &turnResult{
			Text: r.Text, IsError: r.IsError, Subtype: r.Subtype,
			NumTurns: r.NumTurns, DurationMS: r.DurationMS, TotalCostUSD: r.TotalCostUSD,
		}})
		if run != nil {
			if run.turn == nil {
				run.turn = &claude.Turn{Events: []claude.Event{}}
			}
			t := run.turn
			t.SessionID, t.Model, t.Result, t.IsError, t.Subtype = c.sessionID, r.Model, r.Text, r.IsError, r.Subtype
			t.NumTurns, t.DurationMS, t.TotalCostUSD = r.NumTurns, r.DurationMS, r.TotalCostUSD
		}
		s.finishTurnLocked(ws, run)
	case worker.EventError:
		log.Printf("claude in workspace %s: %s", ws.Name, e.Text)
		c.err = e.Text
		c.add(chatEntry{Type: "error", Text: e.Text, Seq: e.Seq, Time: e.Time})
		if run != nil {
			run.err = errors.New(e.Text)
		}
		s.finishTurnLocked(ws, run)
	case worker.EventStopped:
		log.Printf("claude in workspace %s: stopped by the user", ws.Name)
		c.add(chatEntry{Type: "notice", Text: "Stopped. Send a message to continue the conversation.", Seq: e.Seq, Time: e.Time})
		if run != nil {
			run.err = errors.New("stopped by the user")
		}
		s.finishTurnLocked(ws, run)
	case worker.EventInterrupted:
		if ws.interruptedTask != e.TaskID {
			c.add(chatEntry{Type: "notice", Text: e.Text, Seq: e.Seq, Time: e.Time})
		}
		if run != nil {
			run.err = errors.New("interrupted")
		}
		s.finishTurnLocked(ws, run)
	case worker.EventNotice:
		c.add(chatEntry{Type: "notice", Text: e.Text, Seq: e.Seq, Time: e.Time})
	}
	if e.Terminal() || e.Type == worker.EventSession || e.Type == worker.EventUser {
		s.persistLocked(ws)
	}
}

// finishTurnLocked frees the workspace once its turn has ended. run is nil
// when the turn was started by an earlier Mosso process; if another turn is
// in progress then, the event was about an earlier task and nothing ends.
func (s *Server) finishTurnLocked(ws *workspace, run *turnRun) {
	if run == nil && ws.chat.running != nil {
		return
	}
	delete(s.busy, ws.Name)
	ws.taskRunning = false
	ws.chat.running = nil
	if run != nil {
		select {
		case <-run.done:
		default:
			close(run.done)
		}
	}
	ws.chat.notify()
	s.persistLocked(ws)
}

// claudePrompt is what Claude receives: the user's text and, if there are
// attachments, a note saying where they are.
func claudePrompt(prompt string, atts []attachment) string {
	if strings.TrimSpace(prompt) == "" {
		prompt = "(The user sent no text, only attachments.)"
	}
	return prompt + attachmentNote(atts)
}

// startTurnWithAttachments resolves a message's attachments and starts its
// turn, or writes an error.
func (s *Server) startTurnWithAttachments(w http.ResponseWriter, name string, token claude.Token, req messageRequest, sessionID *string) (*turnRun, bool) {
	live, ok := s.workspaceByName(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return nil, false
	}
	atts, err := s.takeAttachments(live, req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	s.mu.Lock()
	gone := live.sandboxState == sandboxGone
	creating := live.creating
	s.mu.Unlock()
	if gone {
		writeError(w, http.StatusGone, goneMessage)
		return nil, false
	}
	if creating {
		writeError(w, http.StatusConflict, "the workspace is still being set up")
		return nil, false
	}
	run, ok := s.startTurn(live, token, req.Prompt, atts, sessionID)
	if !ok {
		writeBusy(w)
	}
	return run, ok
}

// syncTimeout bounds the fetch before a turn.
const syncTimeout = 2 * time.Minute

// syncBeforeTurn fetches the base and working branches from GitHub into the
// sandbox's origin/* refs, so Claude can rebase or merge without
// credentials. A failure is reported in the chat but does not stop the turn.
func (s *Server) syncBeforeTurn(ctx context.Context, ws *workspace) {
	if ws.git == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	gh, _, err := s.githubClient(ctx)
	var synced github.Synced
	if err == nil {
		synced, err = s.gitSync(ws.git, ctx, gh.Token())
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("workspace %s: %v", ws.Name, err)
			s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Could not fetch the latest changes from GitHub (" + err.Error() + "). Claude works with what the sandbox already has."})
		}
		return
	}
	var moved []string
	if b, a := synced.Before.Base, synced.After.Base; b != a {
		moved = append(moved, fmt.Sprintf("origin/%s moved %s → %s", ws.Base, short(b), short(a)))
	}
	if b, a := synced.Before.Branch, synced.After.Branch; b != a && a != "" {
		moved = append(moved, fmt.Sprintf("origin/%s moved %s → %s", ws.Branch, short(b), short(a)))
	}
	if len(moved) > 0 {
		s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Fetched from GitHub: " + strings.Join(moved, "; ") + "."})
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// workspacePrompt tells Claude how Git works in a workspace: no
// credentials, Mosso fetches and pushes.
func workspacePrompt(ws *workspace) string {
	return fmt.Sprintf(`You are working in a Mosso workspace: a checkout of the GitHub repository %[1]s in %[2]s, on the working branch %[3]s, whose pull request targets %[4]s.
Git in this sandbox has no GitHub credentials, on purpose. Do not try to push, authenticate, or use the gh CLI, and do not ask for a token.
- Before each of your turns, Mosso fetches from GitHub for you: origin/%[4]s is the latest base branch, and origin/%[3]s is the working branch as last published (once it has been). git fetch and git pull may fail here; use those refs instead, for example "git rebase origin/%[4]s".
- Commit your work on the current branch. When the user publishes from Mosso, Mosso pushes the current HEAD to %[3]s and opens or updates the pull request. A rebased branch is pushed with --force-with-lease.
- If a rebase or merge stops on conflicts, resolve them and finish it before ending your turn: Mosso will not publish while one is in progress.`,
		ws.Repo, ws.Dir, ws.Branch, ws.Base)
}

// workspaceChat starts a Claude Code turn that continues the workspace's
// conversation and returns immediately; follow its progress with
// GET /workspaces/{name}/events.
func (s *Server) workspaceChat(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.lookupWorkspace(w, r)
	if !ok {
		return
	}
	req, ok := decodeMessage(w, r)
	if !ok {
		return
	}
	token, ok := s.loadClaudeToken(w, r)
	if !ok {
		return
	}
	if _, ok := s.startTurnWithAttachments(w, ws.Name, token, req, nil); !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "working"})
}

// workspaceStop stops the running Claude Code turn. The conversation so far
// is kept, so the next message continues it.
func (s *Server) workspaceStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ws, ok := s.workspaces[r.PathValue("name")]
	var run *turnRun
	var session string
	busy := false
	if ok {
		run, session, busy = ws.chat.running, ws.Sandbox.SessionID, s.busy[ws.Name] && ws.taskRunning
		if run != nil {
			run.stopped = true
		}
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "unknown workspace")
	case run == nil && !busy:
		writeError(w, http.StatusConflict, "Claude is not running in this workspace")
	default:
		// A turn that is still being submitted is stopped by submitTurn;
		// one the worker already has is stopped there.
		if run == nil || run.taskID != "" {
			go s.stopTask(session)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// workspaceEvents streams a workspace's chat as server-sent events. It
// first replays the log (after Last-Event-ID, when the browser reconnects),
// then sends entries as they happen:
//
//	id: <n>, event: entry      a chatEntry
//	event: workspace           the workspace, whenever its status changes
//	event: closed              the workspace was destroyed; the stream ends
//
// Disconnecting does not affect a running turn.
func (s *Server) workspaceEvents(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	ws, ok := s.workspaces[name]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	next := 0
	if id, err := strconv.Atoi(r.Header.Get("Last-Event-ID")); err == nil && id >= 0 {
		next = id + 1
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	ping := time.NewTicker(streamPing)
	defer ping.Stop()

	var lastSnapshot []byte
	for {
		s.mu.Lock()
		cur, live := s.workspaces[name]
		live = live && cur == ws
		var entries []chatEntry
		if next < len(ws.chat.entries) {
			entries = slices.Clone(ws.chat.entries[next:])
		}
		snapshot, _ := json.Marshal(s.snapshotLocked(ws))
		changed := ws.chat.changed
		s.mu.Unlock()

		for _, e := range entries {
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: entry\ndata: %s\n\n", e.ID, b); err != nil {
				return
			}
			next = e.ID + 1
		}
		if !live {
			_, _ = fmt.Fprint(w, "event: closed\ndata: {}\n\n")
			_ = rc.Flush()
			return
		}
		if string(snapshot) != string(lastSnapshot) {
			if _, err := fmt.Fprintf(w, "event: workspace\ndata: %s\n\n", snapshot); err != nil {
				return
			}
			lastSnapshot = snapshot
		}
		if err := rc.Flush(); err != nil {
			return
		}

		select {
		case <-changed:
		case <-ping.C:
			// Also re-checks the status, which changes when the sandbox expires.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.streamsDone:
			return
		}
	}
}

// CloseStreams ends every open event stream so an HTTP server shutdown does
// not wait for them. Browsers reconnect on their own.
func (s *Server) CloseStreams() { s.closeStreams.Do(func() { close(s.streamsDone) }) }
