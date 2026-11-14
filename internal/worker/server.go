package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// Version identifies the worker protocol and build.
const Version = "1"

// followPing is how often a followed event stream sends a heartbeat.
const followPing = 15 * time.Second

// maxMessageBytes bounds the body of POST /messages.
const maxMessageBytes = 1 << 20

// Runner runs one Claude Code turn for msg, emitting events as they
// happen, and returns the final turn. Cancelling ctx must stop it.
type Runner func(ctx context.Context, msg Message, emit func(Event)) (*claude.Turn, error)

// Server is the worker's HTTP API and task runner.
type Server struct {
	log        *Log
	claudePath string
	run        Runner

	mu  sync.Mutex
	cur *task // running task, if any
}

// task is one message being worked on.
type task struct {
	id      string
	msg     Message
	started time.Time
	cancel  context.CancelFunc
	stopped bool // the user asked to stop it
	done    chan struct{}
}

// New returns a worker over the log. If the log ends in a task that was
// still running (the sandbox session ended or the worker was restarted
// while Claude Code worked), that task is marked interrupted now, so the
// server can tell the user and nothing waits for a result that never comes.
func New(l *Log, claudePath string) *Server {
	s := &Server{log: l, claudePath: claudePath}
	s.run = s.runClaude
	s.markInterrupted()
	return s
}

// SetRunner replaces how tasks are run, for tests.
func (s *Server) SetRunner(r Runner) { s.run = r }

// markInterrupted closes a task left open by a previous worker process.
func (s *Server) markInterrupted() {
	if id := s.log.OpenTask(); id != "" {
		s.emit(Event{Type: EventInterrupted, TaskID: id,
			Text: "Interrupted: Claude Code was no longer running when the worker started again (the sandbox session ended, or the worker was restarted). Everything it recorded before that is above. Send a follow-up to let Claude continue."})
	}
}

func (s *Server) emit(e Event) Event {
	rec, err := s.log.Append(e)
	if err != nil {
		log.Printf("worker: record %s event: %v", e.Type, err)
	}
	return rec
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("POST /messages", s.message)
	mux.HandleFunc("POST /stop", s.stop)
	return mux
}

// Status returns the worker's current status.
func (s *Server) Status() Status {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	st := Status{State: "idle", Seq: s.log.Seq(), SessionID: s.log.LastSessionID(), Version: Version, PID: os.Getpid()}
	if cur != nil {
		started := cur.started
		st.State, st.TaskID, st.StartedAt = "running", cur.id, &started
	}
	return st
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}

// events writes the events after ?after=N as JSON lines. With ?follow=1
// the stream stays open and sends new events as they are recorded, plus a
// ping every followPing, until the client goes away.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		writeError(w, http.StatusBadRequest, "after must be a sequence number")
		return
	}
	follow := r.URL.Query().Get("follow") != ""
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	ping := time.NewTicker(followPing)
	defer ping.Stop()
	for {
		changed := s.log.Changed()
		for _, e := range s.log.After(after) {
			if err := enc.Encode(e); err != nil {
				return
			}
			after = e.Seq
		}
		if !follow {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-changed:
		case <-ping.C:
			if err := enc.Encode(Event{Type: EventPing, Time: time.Now().UTC()}); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// message records the prompt and starts Claude Code in the background. It
// answers 202 as soon as the user event is recorded; the task continues
// whether or not the caller stays connected. One task runs at a time.
func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var msg Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes)).Decode(&msg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch {
	case strings.TrimSpace(msg.Prompt) == "":
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	case !strings.HasPrefix(msg.Dir, "/"):
		writeError(w, http.StatusBadRequest, "dir must be an absolute path")
		return
	case msg.SessionID != "" && !claude.ValidSessionID(msg.SessionID):
		writeError(w, http.StatusBadRequest, "invalid session_id")
		return
	case msg.Token == "":
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	t := &task{id: newTaskID(), msg: msg, started: time.Now().UTC(), done: make(chan struct{})}
	s.mu.Lock()
	if s.cur != nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "a task is already running")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	s.cur = t
	s.mu.Unlock()
	user := s.emit(Event{Type: EventUser, TaskID: t.id, Text: msg.Text, Attachments: msg.Attachments})
	go s.work(ctx, t)
	writeJSON(w, http.StatusAccepted, Accepted{TaskID: t.id, Seq: user.Seq})
}

// work runs the task and records how it ended. The terminal event is
// recorded before the worker accepts another message, so the log never
// interleaves two tasks and the status never says idle with a task open.
func (s *Server) work(ctx context.Context, t *task) {
	defer close(t.done)
	defer t.cancel()
	emit := func(e Event) {
		e.TaskID = t.id
		s.emit(e)
	}
	turn, err := s.run(ctx, t.msg, emit)
	s.mu.Lock()
	stopped := t.stopped
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
	}()
	t.msg.Token = "" // this copy; the runner's command environment is gone with the process
	switch {
	case err != nil && (stopped || errors.Is(err, context.Canceled)):
		emit(Event{Type: EventStopped, Text: "Stopped. Send a message to continue the conversation."})
	case err != nil:
		emit(Event{Type: EventError, Text: err.Error()})
	default:
		emit(Event{Type: EventResult, SessionID: turn.SessionID, Result: &Result{
			Text: turn.Result, IsError: turn.IsError, Subtype: turn.Subtype, Model: turn.Model,
			NumTurns: turn.NumTurns, DurationMS: turn.DurationMS, TotalCostUSD: turn.TotalCostUSD,
		}})
	}
}

// stop interrupts the running task.
func (s *Server) stop(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	t := s.cur
	if t != nil {
		t.stopped = true
		t.cancel()
	}
	s.mu.Unlock()
	if t == nil {
		writeError(w, http.StatusConflict, "no task is running")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Wait blocks until the running task, if any, has ended.
func (s *Server) Wait() {
	s.mu.Lock()
	t := s.cur
	s.mu.Unlock()
	if t != nil {
		<-t.done
	}
}

func newTaskID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "task_" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ListenAndServe serves the API on addr (loopback only) until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	host, _, err := splitHostPort(addr)
	if err != nil || !(host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return fmt.Errorf("the worker only listens on loopback, not %q", addr)
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New("missing port")
	}
	return strings.Trim(addr[:i], "[]"), addr[i+1:], nil
}
