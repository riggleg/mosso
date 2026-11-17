package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// Messages sent while Claude is working wait in the workspace's queue and
// are sent one at a time, in order, each when the turn before it has ended,
// continuing the same Claude session. The queue is saved with the
// workspace (workspaceState.Queue), so it survives a Mosso restart.
//
// A queued message leaves the queue when the worker has it: when the worker
// accepts it, or when its user event arrives, whichever Mosso sees first.
// Either happens under Server.mu, and the queue is saved in the same
// transaction as the chat entry that replaces the message (see
// addWithStateLocked), so it is either queued or in the chat, never both or
// neither. Its ID goes to the worker as the message's key, and the
// worker runs a key once (see worker.Message.Key): sending it again after
// a restart, a lost answer or a retry cannot run it twice.
//
// When a turn fails or the user stops it, the queue pauses
// (Workspace.QueuePaused) until the user resumes it, so the rest of the
// queue does not run into the same failure or past the stop. When the
// sandbox stops, the queue waits and goes on once the sandbox runs again.

// queuedMessage is a message waiting in a workspace's queue.
type queuedMessage struct {
	// ID identifies the message for cancelling it, and is its worker key.
	ID          string       `json:"id"`
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments,omitempty"`
	Time        time.Time    `json:"time"`
	// Sending says the message is being sent to the worker; it can no
	// longer be cancelled. It is saved, so a restart knows the message may
	// have reached the worker; snapshots say whether it still is (see
	// sendingLocked).
	Sending bool `json:"sending,omitempty"`
}

// Why a workspace's queue is paused (Workspace.QueuePaused).
const (
	queueStopped = "stopped" // the user stopped Claude
	queueFailed  = "failed"  // a turn failed, or a queued message could not be sent
)

func newQueueID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "q_" + hex.EncodeToString(b[:])
}

// queueWaitingLocked says queued messages wait to be sent, so a new
// message must go behind them.
func queueWaitingLocked(ws *workspace) bool {
	return len(ws.Queue) > 0 && ws.QueuePaused == ""
}

// pauseQueueLocked pauses the queue after a failure or a stop, if there
// is anything in it.
func (s *Server) pauseQueueLocked(ws *workspace, reason string) {
	if len(ws.Queue) > 0 {
		ws.QueuePaused = reason
	}
}

// removeQueued removes a message from the queue, if it is there. An empty
// queue is never paused.
func removeQueued(ws *workspace, id string) bool {
	i := slices.IndexFunc(ws.Queue, func(q queuedMessage) bool { return q.ID == id })
	if i < 0 {
		return false
	}
	ws.Queue = slices.Delete(ws.Queue, i, i+1)
	if len(ws.Queue) == 0 {
		ws.QueuePaused = ""
	}
	return true
}

// sendingLocked says the queued message is on its way to the worker: a
// turn is sending it, or Mosso stopped while sending it and is now finding
// out from the worker whether it arrived.
func (s *Server) sendingLocked(ws *workspace, q queuedMessage) bool {
	if run := ws.chat.running; run != nil {
		return run.queued != nil && run.queued.ID == q.ID
	}
	return q.Sending && s.busy[ws.Name]
}

// queuedSentLocked records that the worker has the run's queued message:
// it moves from the queue to the chat, numbered seq. Call with s.mu held.
func (s *Server) queuedSentLocked(ws *workspace, run *turnRun, seq int64, t time.Time) {
	q := run.queued
	removeQueued(ws, q.ID)
	run.userIdx = s.addWithStateLocked(ws, chatEntry{Type: "user", Text: q.Text, Attachments: q.Attachments, Seq: seq, Time: t})
}

// addWithStateLocked adds an entry to the chat and saves it in the same
// transaction as the workspace's record, rather than on its own as chat
// entries usually are, so that the queue it left is saved with it. It
// returns the entry's ID. Call with s.mu held.
func (s *Server) addWithStateLocked(ws *workspace, e chatEntry) int {
	c := ws.chat
	persist := c.persist
	c.persist = nil
	id := c.add(e).ID
	c.persist = persist
	s.persistLocked(ws)
	return id
}

// queueReadyLocked says the next queued message can be sent now: the
// queue is not paused and nothing else runs in a workspace whose sandbox
// is running.
func (s *Server) queueReadyLocked(ws *workspace) bool {
	return queueWaitingLocked(ws) && s.ctx.Err() == nil && s.vercel != nil &&
		!s.busy[ws.Name] && ws.chat.running == nil && !ws.creating && !ws.connecting &&
		!ws.queuedPrompt && ws.sandboxState == sandboxRunning
}

// dispatchQueue sends the next queued message if the workspace is ready
// for it. It is called whenever that may have become true: a turn or a
// publish ended, the sandbox was connected, the queue was resumed. Its
// turn's end sends the one after.
func (s *Server) dispatchQueue(ws *workspace) {
	s.mu.Lock()
	ready := s.queueReadyLocked(ws)
	s.mu.Unlock()
	if !ready {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	token, _, err := s.claudeToken(ctx)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.queueReadyLocked(ws) {
		return
	}
	if err != nil {
		log.Printf("workspace %s: send the next queued message: %v", ws.Name, err)
		ws.chat.add(chatEntry{Type: "error", Text: "Could not send the next queued message: " + err.Error()})
		s.pauseQueueLocked(ws, queueFailed)
		s.persistLocked(ws)
		return
	}
	q := &ws.Queue[0]
	q.Sending = true
	s.busy[ws.Name] = true
	queued := *q
	log.Printf("workspace %s: sending queued message %s (%d queued)", ws.Name, q.ID, len(ws.Queue))
	s.beginTurnLocked(ws, token, queued.Text, queued.Attachments, nil, -1, &queued)
}

// sendOrQueue starts a turn for the message if the workspace is free, and
// queues it if Claude (or a publish) is busy or earlier queued messages are
// waiting to be sent. A queued message is saved before the answer, and is
// not queued if it cannot be.
func (s *Server) sendOrQueue(w http.ResponseWriter, name string, token claude.Token, req messageRequest) {
	ws, atts, ok := s.acceptMessage(w, name, req)
	if !ok {
		return
	}
	for {
		s.mu.Lock()
		if s.busy[ws.Name] || queueWaitingLocked(ws) {
			q := queuedMessage{ID: newQueueID(), Text: req.Prompt, Attachments: atts, Time: time.Now().UTC()}
			ws.Queue = append(ws.Queue, q)
			unsaved := ws.unsaved
			if err := s.saveLocked(ws); err != nil {
				// Not saved is not queued: it would be lost in a restart.
				ws.Queue, ws.unsaved = ws.Queue[:len(ws.Queue)-1], unsaved
				s.mu.Unlock()
				log.Printf("workspace %s: save: %v", ws.Name, err)
				writeError(w, http.StatusInternalServerError, "the message could not be queued: "+err.Error())
				return
			}
			ws.chat.notify()
			// Sending to a stopped sandbox resumes it, queued or not; connect
			// then sends the queue. Otherwise, the workspace may have become
			// free meanwhile.
			resume := !s.busy[ws.Name] && ws.sandboxState == sandboxStopped
			s.mu.Unlock()
			if resume {
				go s.resumeForQueue(ws)
			} else {
				go s.dispatchQueue(ws)
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "id": q.ID})
			return
		}
		s.mu.Unlock()
		if _, ok := s.startTurn(ws, token, req.Prompt, atts, nil, -1); ok {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "working"})
			return
		}
		// Something started meanwhile: queue the message behind it.
	}
}

// cancelQueued removes a message from the workspace's queue before it is
// sent (DELETE /workspaces/{name}/queue/{id}). It only touches the
// database, so it works without Vercel credentials.
func (s *Server) cancelQueued(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ws, ok := s.workspaces[r.PathValue("name")]
	if !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	id := r.PathValue("id")
	i := slices.IndexFunc(ws.Queue, func(q queuedMessage) bool { return q.ID == id })
	if i >= 0 && s.sendingLocked(ws, ws.Queue[i]) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "the message is already being sent to Claude")
		return
	}
	queue, paused, unsaved := slices.Clone(ws.Queue), ws.QueuePaused, ws.unsaved
	if !removeQueued(ws, id) {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "no such queued message: it was sent or cancelled already")
		return
	}
	if err := s.saveLocked(ws); err != nil {
		// Not saved is not cancelled: it would come back after a restart.
		ws.Queue, ws.QueuePaused, ws.unsaved = queue, paused, unsaved
		s.mu.Unlock()
		log.Printf("workspace %s: save: %v", ws.Name, err)
		writeError(w, http.StatusInternalServerError, "the change could not be saved: "+err.Error())
		return
	}
	ws.chat.notify()
	snapshot := s.snapshotLocked(ws)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

// resumeQueue lifts a paused queue (POST /workspaces/{name}/queue/resume)
// and sends the next message as soon as the workspace is free, resuming a
// stopped sandbox for it.
func (s *Server) resumeQueue(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ws, ok := s.workspaces[r.PathValue("name")]
	if !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	}
	if gone := noSandbox(ws.sandboxState); gone != "" {
		s.mu.Unlock()
		writeError(w, http.StatusGone, gone)
		return
	}
	paused := ws.QueuePaused
	ws.QueuePaused = ""
	if err := s.saveLocked(ws); err != nil {
		ws.QueuePaused = paused
		s.mu.Unlock()
		log.Printf("workspace %s: save: %v", ws.Name, err)
		writeError(w, http.StatusInternalServerError, "the change could not be saved: "+err.Error())
		return
	}
	ws.chat.notify()
	snapshot := s.snapshotLocked(ws)
	running := ws.sandboxState == sandboxRunning
	s.mu.Unlock()
	if running {
		go s.dispatchQueue(ws)
	} else {
		go s.resumeForQueue(ws)
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// resumeForQueue resumes a stopped sandbox so that the queued messages are
// sent; connect sends them once the sandbox runs.
func (s *Server) resumeForQueue(ws *workspace) {
	ctx, cancel := context.WithTimeout(s.ctx, resumeTimeout)
	defer cancel()
	if err := s.connect(ctx, ws); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("workspace %s: resume the sandbox for the queue: %v", ws.Name, err)
		s.addEntry(ws.chat, chatEntry{Type: "error", Text: "Could not resume the sandbox to send the queued messages: " + err.Error()})
	}
}
