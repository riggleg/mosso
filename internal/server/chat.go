package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
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
// replays. Entries are saved in Mosso's database as they are added, so the
// log survives a Mosso restart and the workspace's sandbox; Claude's activity
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
	// persist, if set, is called after an entry is appended, to save it.
	persist func()
	// saved is how many of the entries are in the database. Saving writes
	// the rest, so an entry whose save failed goes out with the next one.
	saved int
	// unloaded says entries has not been read from the database yet: the
	// chat of a terminated workspace is only loaded when it is opened.
	unloaded bool
}

func newChat() *chat { return &chat{changed: make(chan struct{})} }

func (c *chat) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// load replaces the entries with ones read from the database.
func (c *chat) load(entries []chatEntry) {
	c.entries = entries
	c.saved = len(entries)
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
		c.persist()
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
	// publish that failed), "notice", "pull_request" (the user published)
	// or "github" (the agent pushed or changed a pull request, or failed
	// to).
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Event       *claude.Event     `json:"event,omitempty"`
	Result      *turnResult       `json:"result,omitempty"`
	PullRequest *github.Published `json:"pull_request,omitempty"`
	// GitHub is a GitHub operation the agent had Mosso run ("github").
	GitHub *githubAction `json:"github,omitempty"`
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
	// queued is the queued message the turn sends, if it is one. Its chat
	// entry is added when the worker has it, and it leaves the queue then.
	queued *queuedMessage
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
//
// userIdx is the chat entry already showing the prompt, or -1 to add one:
// a workspace created with a first message shows it while it is set up.
func (s *Server) startTurn(ws *workspace, token claude.Token, prompt string, atts []attachment, sessionID *string, userIdx int) (*turnRun, bool) {
	if !s.acquire(ws.Name) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beginTurnLocked(ws, token, prompt, atts, sessionID, userIdx, nil), true
}

// beginTurnLocked is startTurn once the workspace is marked busy. With
// queued, the turn sends that queued message: no chat entry is added until
// the worker has it, and the worker runs it once however often it is sent
// (see worker.Message.Key). Call with s.mu held.
func (s *Server) beginTurnLocked(ws *workspace, token claude.Token, prompt string, atts []attachment, sessionID *string, userIdx int, queued *queuedMessage) *turnRun {
	run := &turnRun{done: make(chan struct{}), userIdx: userIdx, queued: queued}
	c := ws.chat
	sid := c.sessionID
	if sessionID != nil {
		sid = *sessionID
	}
	c.running, c.err = run, ""
	ws.taskRunning = true
	if run.userIdx < 0 && queued == nil {
		run.userIdx = c.add(chatEntry{Type: "user", Text: prompt, Attachments: atts}).ID
	}
	s.persistLocked(ws)
	c.notify() // the status, and a queued message now being sent

	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, setupTimeout)
		defer cancel()
		if err := s.submitTurn(ctx, ws, run, token, prompt, atts, sid); err != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			if ws.sandboxState == sandboxTerminated {
				return // terminating the workspace ended the turn
			}
			if s.ctx.Err() != nil {
				// Mosso is stopping. The turn stays saved as running, and the
				// next start finds out from the worker whether it got the
				// message (a queued one is sent again if not).
				run.err = err
				close(run.done)
				return
			}
			if run.stopped {
				c.add(chatEntry{Type: "notice", Text: "Stopped. Send a message to continue the conversation."})
				s.pauseQueueLocked(ws, queueStopped)
			} else {
				// Prompt text is deliberately not logged.
				log.Printf("claude in workspace %s: %v", ws.Name, err)
				c.err = err.Error()
				if queued != nil {
					c.add(chatEntry{Type: "error", Text: "Could not send the queued message: " + err.Error()})
				} else {
					c.add(chatEntry{Type: "error", Text: err.Error()})
				}
				s.pauseQueueLocked(ws, queueFailed)
			}
			run.err = err
			s.finishTurnLocked(ws, run)
		}
	}()
	return run
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
	msg := worker.Message{
		Text:         prompt,
		Attachments:  attsJSON,
		Prompt:       claudePrompt(prompt, atts),
		SessionID:    sid,
		Dir:          ws.Dir,
		SystemPrompt: workspacePrompt(ws),
		Model:        ws.Model,
		Token:        token.Reveal(),
	}
	if run.queued != nil {
		msg.Key = run.queued.ID
	}
	acc, err := s.workerSubmit(ctx, session, msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws.sandboxState == sandboxTerminated {
		return nil // terminated meanwhile; the sandbox is going away
	}
	run.taskID = acc.TaskID
	ws.queuedPrompt = false // the worker has it
	switch {
	case acc.Duplicate:
		// The worker already had this queued message (Mosso sent it before
		// a restart, or its answer was lost). Its events that Mosso has not
		// applied yet come through the follower, which takes the message
		// off the queue and ends the turn.
		if acc.Seq <= ws.lastSeq {
			removeQueued(ws, run.queued.ID)
		}
		if acc.EndSeq != 0 && acc.EndSeq <= ws.lastSeq {
			// It ended, and that is recorded: nothing more will come.
			s.finishTurnLocked(ws, run)
			return nil
		}
	case acc.Seq > ws.lastSeq:
		// The follower has not delivered the worker's user event yet.
		ws.lastSeq = acc.Seq
		switch {
		case run.queued != nil:
			s.queuedSentLocked(ws, run, acc.Seq, time.Now().UTC())
		case run.userIdx >= 0 && run.userIdx < len(ws.chat.entries):
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
	if e.Seq <= ws.lastSeq || ws.sandboxState == sandboxTerminated {
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
			ws.queuedPrompt = false
			break
		}
		if ws.queuedPrompt {
			// The worker got the first message before Mosso stopped and
			// recorded it: this is its copy, not a new message.
			ws.queuedPrompt = false
			if i := queuedEntry(c); i >= 0 {
				c.entries[i].Seq = e.Seq
				s.persistLocked(ws)
				break
			}
		}
		var atts []attachment
		_ = json.Unmarshal(e.Attachments, &atts)
		entry := chatEntry{Type: "user", Text: e.Text, Attachments: atts, Seq: e.Seq, Time: e.Time}
		var idx int
		if e.Key != "" && removeQueued(ws, e.Key) {
			// A queued message the worker has: it leaves the queue.
			idx = s.addWithStateLocked(ws, entry)
		} else {
			idx = c.add(entry).ID
		}
		if e.Key != "" {
			if run != nil && run.queued != nil && run.queued.ID == e.Key {
				run.userIdx = idx
				if run.taskID == "" {
					run.taskID = e.TaskID
				}
			}
		}
	case worker.EventSession:
		if e.SessionID != "" {
			c.sessionID = e.SessionID
		}
	case worker.EventClaude:
		if e.Event != nil {
			c.add(chatEntry{Type: "claude", Event: e.Event, Seq: e.Seq, Time: e.Time})
			s.cacheImagesLocked(ws, e.Event.Images)
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
		if run != nil && run.stopped {
			// Stopped as it finished: the queue still waits, as for a stop.
			s.pauseQueueLocked(ws, queueStopped)
		}
		if r.IsError {
			c.err = "Claude Code reported an error"
			if r.Text != "" {
				c.err = r.Text
			}
			s.pauseQueueLocked(ws, queueFailed)
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
		s.pauseQueueLocked(ws, queueFailed)
		if run != nil {
			run.err = errors.New(e.Text)
		}
		s.finishTurnLocked(ws, run)
	case worker.EventStopped:
		log.Printf("claude in workspace %s: stopped by the user", ws.Name)
		c.add(chatEntry{Type: "notice", Text: "Stopped. Send a message to continue the conversation.", Seq: e.Seq, Time: e.Time})
		s.pauseQueueLocked(ws, queueStopped)
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
	case worker.EventToolCall:
		// Claude's own tool_use and tool_result show the call in the chat;
		// Mosso runs it, and adds an entry for what changed on GitHub.
		if e.Call != nil {
			go s.handleToolCall(ws, e.Call.ID)
		}
	}
	if e.Terminal() || e.Type == worker.EventSession || e.Type == worker.EventUser {
		s.persistLocked(ws)
	}
}

// finishTurnLocked frees the workspace once its turn has ended, and sends
// the next queued message if there is one. run is nil when the turn was
// started by an earlier Mosso process; if another turn is in progress
// then, the event was about an earlier task and nothing ends.
func (s *Server) finishTurnLocked(ws *workspace, run *turnRun) {
	if run == nil && ws.chat.running != nil {
		return
	}
	delete(s.busy, ws.Name)
	ws.taskRunning = false
	ws.chat.running = nil
	for i := range ws.Queue {
		ws.Queue[i].Sending = false // not sent, if still here: it goes again
	}
	if len(ws.Queue) > 0 {
		go s.dispatchQueue(ws)
	}
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

// acceptMessage finds the workspace a message is for and resolves its
// attachments, or writes an error if the workspace cannot take messages.
func (s *Server) acceptMessage(w http.ResponseWriter, name string, req messageRequest) (*workspace, []attachment, bool) {
	live, ok := s.workspaceByName(name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown workspace")
		return nil, nil, false
	}
	atts, err := s.takeAttachments(live, req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	s.mu.Lock()
	gone := noSandbox(live.sandboxState)
	creating := live.creating
	s.mu.Unlock()
	if gone != "" {
		writeError(w, http.StatusGone, gone)
		return nil, nil, false
	}
	if creating {
		writeError(w, http.StatusConflict, "the workspace is still being set up")
		return nil, nil, false
	}
	return live, atts, true
}

// startTurnWithAttachments resolves a message's attachments and starts its
// turn, or writes an error. It does not queue: while queued messages wait,
// the workspace is busy for it.
func (s *Server) startTurnWithAttachments(w http.ResponseWriter, name string, token claude.Token, req messageRequest, sessionID *string) (*turnRun, bool) {
	live, atts, ok := s.acceptMessage(w, name, req)
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	waiting := queueWaitingLocked(live)
	s.mu.Unlock()
	if waiting {
		writeError(w, http.StatusConflict, "messages are queued in this workspace; they are sent first")
		return nil, false
	}
	run, ok := s.startTurn(live, token, req.Prompt, atts, sessionID, -1)
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

// workspacePrompt tells Claude how Git and GitHub work in a workspace (no
// credentials in the sandbox; Mosso fetches, and pushes and opens pull
// requests through its tools) and, if the workspace has agent
// instructions, adds them after Mosso's own, which take precedence. It is
// appended to Claude Code's system prompt on every turn, including turns
// that resume a session, so the instructions always apply. They come from
// the workspace's saved configuration, not from its section, so editing
// the section or moving the workspace does not change them.
func workspacePrompt(ws *workspace) string {
	return gitPrompt(ws) + instructionsPrompt(ws.Instructions)
}

// instructionsPrompt frames agent instructions for the system prompt, or
// returns "" for none. They guide how Claude works; they are not the task,
// and they cannot lift Mosso's rules: what they ask that this sandbox
// cannot do is left to the user, and said so.
func instructionsPrompt(instructions string) string {
	if instructions == "" {
		return ""
	}
	return `

The user saved the following instructions for this workspace in Mosso. Follow them on every turn, alongside the user's messages: they describe how to work, not the task itself, and the user's messages come first. They do not override the rules above. If they ask you to push or open pull requests, do it with Mosso's GitHub tools as described above. If they ask for something those rules or tools do not allow (for example force-pushing, merging pull requests, pushing to other branches, or using GitHub credentials), do the parts you can, then say clearly in your reply what is left for the user to do in Mosso or on GitHub.
<workspace-instructions>
` + escapeInstructions(instructions) + `
</workspace-instructions>`
}

// escapeInstructions escapes instructions as XML character data, so that
// text such as "</workspace-instructions>" cannot appear to close or reopen
// the block. Newlines stay as they are; &, <, >, quotes, tabs and carriage
// returns become entity references. This keeps the prompt's structure
// readable; it is not a security boundary: Mosso's rules are enforced in
// code, not by the prompt.
func escapeInstructions(instructions string) string {
	var b strings.Builder
	e := xml.NewEncoder(&b)
	// Encoding character data into a strings.Builder cannot fail.
	_ = e.EncodeToken(xml.CharData(instructions))
	_ = e.Flush()
	return b.String()
}

// gitPrompt tells Claude how Git and GitHub work in a workspace. It names
// the tools without assuming how many pull requests, or which base
// branches, a task needs: that comes from the user and the instructions.
func gitPrompt(ws *workspace) string {
	return fmt.Sprintf(`You are working in a Mosso workspace: a checkout of the GitHub repository %[1]s in %[2]s, on the working branch %[3]s. The workspace's base branch is %[4]s.
Git in this sandbox has no GitHub credentials, on purpose: git push, git fetch, git pull and the gh CLI do not work for this repository. Do not try to authenticate, and do not ask for a token. Instead, Mosso gives you GitHub tools (its "mosso" MCP server), which run on Mosso's server with the user's GitHub access, for this repository only:
- github_fetch: fetch a branch into origin/<branch> here (for example another base branch).
- github_push: push HEAD or a local branch to one of this workspace's own branches on GitHub: %[3]s, or %[3]s-<suffix> (for example %[3]s-uat). Only committed work is pushed, and only as a fast-forward: Mosso never force-pushes for you.
- github_create_pr: open a pull request from one of those branches into a base branch (or get the one already open).
- github_list_prs, github_get_pr, github_update_pr (title, description, draft status) and github_checks (CI results) for this workspace's pull requests.
How to work with them:
- Before each of your turns, Mosso fetches origin/%[4]s, origin/%[3]s and every branch you fetched or pushed, so those refs are current. Use them, for example "git rebase origin/%[4]s".
- Push and open or update pull requests when the user's messages or the workspace instructions ask for it. Otherwise commit your work and leave publishing to the user, who can also click Create PR in Mosso (it commits any uncommitted work, pushes HEAD to %[3]s and opens or updates its pull request into %[4]s).
- Use one head branch per pull request. For a pull request into another base branch, fetch that branch, create a local branch from origin/<that base>, bring the change over (cherry-pick it, or adapt it where the branches differ), check it there as you would the original (build, tests), commit, push it to %[3]s-<suffix> and open the pull request from that branch. Do not assume a change made for one base applies cleanly to another.
- If a push is refused because the branch on GitHub has commits yours lacks, rebase onto or merge origin/<branch> and push again, or push to a new %[3]s-<suffix> branch.
- Force-pushing, deleting branches, merging or closing pull requests, and pushing to any other branch are not available to you. If the task needs them, say so in your reply for the user to do.
- Give the links of the pull requests you open or update in your reply. If a tool fails, read its message: it says what to do, or what to tell the user.
- If a rebase or merge stops on conflicts, resolve them and finish it before pushing or ending your turn.`,
		ws.Repo, ws.Dir, ws.Branch, ws.Base)
}

// workspaceChat starts a Claude Code turn that continues the workspace's
// conversation and returns immediately; follow its progress with
// GET /workspaces/{name}/events. While Claude is working, the message is
// queued instead (see queue.go).
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
	s.sendOrQueue(w, ws.Name, token, req)
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
		if run != nil || busy {
			// Whatever ends the turn now, the queue waits for the user.
			s.pauseQueueLocked(ws, queueStopped)
			s.persistLocked(ws)
			ws.chat.notify()
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
//	event: closed              the workspace was removed; the stream ends
//
// A terminated workspace's stream replays its saved chat and then only
// reports status changes (archiving). Disconnecting does not affect a
// running turn.
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
		s.loadChatLocked(ws)
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

// loadChatLocked reads a terminated workspace's chat from the database the
// first time it is opened after a start. Call with s.mu held.
func (s *Server) loadChatLocked(ws *workspace) {
	if !ws.chat.unloaded || s.store == nil {
		return
	}
	entries, err := s.store.loadEntries(ws.Name)
	if err != nil {
		log.Printf("workspace %s: load the chat: %v", ws.Name, err)
		return
	}
	ws.chat.load(entries)
	ws.chat.unloaded = false
}
