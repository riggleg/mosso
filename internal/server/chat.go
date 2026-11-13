package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

// streamPing is how often an idle event stream sends a keep-alive comment.
const streamPing = 15 * time.Second

// maxChatBytes bounds the memory one chat log uses. Past it, tool inputs and
// outputs are no longer kept; Claude's progress is still recorded.
const maxChatBytes = 32 << 20

// omitted replaces tool data once a chat log is full.
const omitted = "[mosso: not kept, the chat log is full]"

// chat is a workspace's conversation with Claude Code: a log of prompts,
// Claude's activity and pull request results that every browser tab replays.
// It lives in memory, so it is lost when the workspace is destroyed or Mosso
// stops. All fields are guarded by Server.mu.
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
}

func newChat() *chat { return &chat{changed: make(chan struct{})} }

func (c *chat) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *chat) add(e chatEntry) {
	n := len(e.Text)
	if e.Event != nil {
		n += len(e.Event.Text) + len(e.Event.Input) + len(e.Event.Content)
	}
	if e.Result != nil {
		n += len(e.Result.Text)
	}
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
	c.append(e)
}

func (c *chat) append(e chatEntry) {
	e.ID = len(c.entries)
	e.Time = time.Now().UTC()
	c.entries = append(c.entries, e)
	c.notify()
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

// turnRun is a Claude Code turn running in the background.
type turnRun struct {
	done    chan struct{} // closed when the turn has ended and been recorded
	cancel  context.CancelFunc
	stopped bool // guarded by Server.mu
	turn    *claude.Turn
	err     error
}

// stopLocked cancels run if it is still the workspace's running turn.
func (c *chat) stopLocked(run *turnRun) bool {
	if run == nil || c.running != run {
		return false
	}
	run.stopped = true
	run.cancel()
	return true
}

// startTurn records prompt in the workspace's chat and runs a Claude Code
// turn in the background. The turn belongs to the server, not to a request,
// so it keeps running if the browser disconnects; it ends when Claude
// finishes, the user stops it, the workspace is destroyed or Mosso shuts
// down. A nil sessionID continues the chat's conversation. It reports false
// if something else is running in the workspace.
func (s *Server) startTurn(ws workspace, token claude.Token, prompt string, sessionID *string) (*turnRun, bool) {
	if !s.acquire(ws.Name) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(s.ctx, vercel.MaxTimeout)
	run := &turnRun{done: make(chan struct{}), cancel: cancel}
	c := ws.chat
	s.mu.Lock()
	sid := c.sessionID
	if sessionID != nil {
		sid = *sessionID
	}
	c.running, c.err = run, ""
	c.add(chatEntry{Type: "user", Text: prompt})
	installed := s.claudeInstalled[ws.Name]
	s.mu.Unlock()

	go func() {
		defer close(run.done)
		defer cancel()
		if !installed {
			s.addEntry(c, chatEntry{Type: "notice", Text: "Installing Claude Code in the sandbox…"})
		}
		run.err = s.ensureClaude(ctx, ws.Name, ws.Sandbox.SessionID)
		if run.err == nil {
			run.turn, run.err = claude.Send(ctx, s.vercel, ws.Sandbox.SessionID, token, claude.Request{
				Prompt:    prompt,
				SessionID: sid,
				Dir:       ws.Dir,
				OnSession: func(id string) {
					s.mu.Lock()
					c.sessionID = id
					s.mu.Unlock()
				},
				OnEvent: func(e claude.Event) { s.addEntry(c, chatEntry{Type: "claude", Event: &e}) },
			})
		}
		s.finishTurn(ws, c, run)
	}()
	return run, true
}

// finishTurn records how a turn ended and frees the workspace, atomically,
// so no other request can slip in between.
func (s *Server) finishTurn(ws workspace, c *chat, run *turnRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, ws.Name)
	c.running = nil
	turn, err := run.turn, run.err
	switch {
	case err != nil && run.stopped:
		log.Printf("claude in workspace %s: stopped by the user", ws.Name)
		c.add(chatEntry{Type: "notice", Text: "Stopped. Send a message to continue the conversation."})
	case err != nil:
		// Prompt text is deliberately not logged.
		log.Printf("claude in workspace %s: %v", ws.Name, err)
		msg := err.Error()
		if time.Now().After(ws.ExpiresAt) {
			msg = "The sandbox has expired, so its files and conversation are gone. Terminate this workspace and create a new one. (" + msg + ")"
		}
		c.err = msg
		c.add(chatEntry{Type: "error", Text: msg})
	default:
		log.Printf("claude in workspace %s: session %s, %d events, is_error=%t", ws.Name, turn.SessionID, len(turn.Events), turn.IsError)
		if turn.IsError {
			c.err = "Claude Code reported an error"
			if turn.Result != "" {
				c.err = turn.Result
			}
		}
		c.add(chatEntry{Type: "result", Result: &turnResult{
			Text: turn.Result, IsError: turn.IsError, Subtype: turn.Subtype,
			NumTurns: turn.NumTurns, DurationMS: turn.DurationMS, TotalCostUSD: turn.TotalCostUSD,
		}})
	}
	if turn != nil && turn.SessionID != "" {
		c.sessionID = turn.SessionID
	}
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
	if _, ok := s.startTurn(ws, token, req.Prompt, nil); !ok {
		writeBusy(w)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "working"})
}

// workspaceStop stops the running Claude Code turn. The conversation so far
// is kept, so the next message continues it.
func (s *Server) workspaceStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ws, ok := s.workspaces[r.PathValue("name")]
	running := ok && ws.chat.stopLocked(ws.chat.running)
	s.mu.Unlock()
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "unknown workspace")
	case !running:
		writeError(w, http.StatusConflict, "Claude is not running in this workspace")
	default:
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
