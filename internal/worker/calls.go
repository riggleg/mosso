package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Tool calls are relayed between Claude and the Mosso server through the
// event log, so they survive the server disconnecting or restarting:
//
//  1. Claude Code calls a tool; the worker's MCP server (mcp.go) posts it
//     to POST /tools/calls, which records a tool_call event and waits.
//  2. The Mosso server follows the event log (or lists the pending calls
//     in GET /status after reconnecting), claims the call with
//     POST /tools/calls/{id}/start, runs it on the server and answers with
//     POST /tools/calls/{id}/result, recorded as a tool_result event.
//  3. The waiting request returns the result to Claude.
//
// A call nobody claims within the pickup time is withdrawn (nothing ran),
// so Claude is not stuck while Mosso is not running. A claimed call is
// never withdrawn: it is running on the server.

const (
	// DefaultPickupTimeout is how long a call waits for the Mosso server
	// to claim it.
	DefaultPickupTimeout = 5 * time.Minute
	// DefaultCallTimeout is how long a call waits for its result in all.
	DefaultCallTimeout = 20 * time.Minute
	// MaxCallsPerTask bounds the tool calls of one task.
	MaxCallsPerTask = 100
	// maxCallBytes bounds a tool call's arguments and a result.
	maxCallBytes = 256 << 10
)

// call states.
const (
	callPending = "pending" // recorded, waiting for the server to claim it
	callStarted = "started" // claimed: running on the server
)

// call is a tool call in flight. Guarded by Server.mu.
type call struct {
	ToolCall
	task  string
	state string
	done  chan struct{} // closed with the result
	res   ToolCall
}

// CallRequest is the body of POST /tools/calls.
type CallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallAnswer is the worker's answer to POST /tools/calls, and the body the
// server posts to POST /tools/calls/{id}/result: the result (JSON), or an
// error message for the agent.
type CallAnswer struct {
	ID     string          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// toolCall records a tool call and waits for its result.
func (s *Server) toolCall(w http.ResponseWriter, r *http.Request) {
	var req CallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCallBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, ok := FindTool(req.Name); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown tool %q", req.Name))
		return
	}
	if len(req.Arguments) == 0 || string(req.Arguments) == "null" {
		req.Arguments = json.RawMessage(`{}`)
	}
	s.mu.Lock()
	t := s.cur
	switch {
	case t == nil:
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "Mosso's tools can only be used while Claude works on a message")
		return
	case t.calls >= MaxCallsPerTask:
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("this message already made %d tool calls, the most Mosso allows for one message", MaxCallsPerTask))
		return
	}
	t.calls++
	c := &call{ToolCall: ToolCall{ID: newCallID(), Name: req.Name, Arguments: req.Arguments}, task: t.id, state: callPending, done: make(chan struct{})}
	if s.calls == nil {
		s.calls = map[string]*call{}
	}
	s.calls[c.ID] = c
	// Recorded under the lock, so the server cannot try to claim the call
	// before it is listed.
	s.emit(Event{Type: EventToolCall, TaskID: t.id, Call: &c.ToolCall})
	s.mu.Unlock()

	pickup := time.NewTimer(s.pickupTimeout)
	defer pickup.Stop()
	limit := time.NewTimer(s.callTimeout)
	defer limit.Stop()
	for {
		select {
		case <-c.done:
			writeJSON(w, http.StatusOK, CallAnswer{ID: c.ID, Result: c.res.Result, Error: c.res.Error})
			return
		case <-pickup.C:
			s.withdraw(c, fmt.Sprintf("Mosso did not pick up this call within %s, so nothing was done: the Mosso server is probably not running or not connected to this sandbox. Leave your work committed and tell the user what is left to do.", s.pickupTimeout))
		case <-limit.C:
			writeError(w, http.StatusGatewayTimeout, fmt.Sprintf("Mosso started this call but did not report its result within %s. Its outcome is unknown: check with %s or %s before trying again.", s.callTimeout, ToolListPRs, ToolGetPR))
			return
		case <-r.Context().Done():
			s.withdraw(c, "withdrawn: the caller went away before Mosso picked up the call, so nothing was done")
			return
		}
	}
}

// withdraw ends a call nobody claimed with an error.
func (s *Server) withdraw(c *call, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.state == callPending {
		s.finishCallLocked(c, ToolCall{ID: c.ID, Error: msg})
	}
}

// finishCallLocked records a call's result and wakes its waiter.
func (s *Server) finishCallLocked(c *call, res ToolCall) {
	res.ID = c.ID
	s.emit(Event{Type: EventToolResult, TaskID: c.task, Call: &res})
	c.res = res
	c.state = ""
	delete(s.calls, c.ID)
	close(c.done)
}

// startCall lets the server claim a pending call; it answers the call's
// name and arguments. 409 if the call is not pending (claimed, finished,
// withdrawn or unknown), so a call runs at most once.
func (s *Server) startCall(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c := s.calls[r.PathValue("id")]
	if c == nil || c.state != callPending {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "the call is not waiting to be run")
		return
	}
	c.state = callStarted
	tc := c.ToolCall
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, tc)
}

// callResult records the server's answer to a call it claimed.
func (s *Server) callResult(w http.ResponseWriter, r *http.Request) {
	var ans CallAnswer
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCallBytes)).Decode(&ans); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if (len(ans.Result) == 0) == (ans.Error == "") {
		writeError(w, http.StatusBadRequest, "give either a result or an error")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls[r.PathValue("id")]
	if c == nil || c.state != callStarted {
		writeError(w, http.StatusConflict, "the call is not running")
		return
	}
	s.finishCallLocked(c, ToolCall{Result: ans.Result, Error: ans.Error})
	w.WriteHeader(http.StatusNoContent)
}

// pendingCallsLocked returns the IDs of the calls waiting to be claimed.
func (s *Server) pendingCallsLocked() []string {
	var ids []string
	for id, c := range s.calls {
		if c.state == callPending {
			ids = append(ids, id)
		}
	}
	return ids
}

// markOpenCalls ends the calls a previous worker process left without a
// result: the processes that made them are gone.
func (s *Server) markOpenCalls() {
	for _, c := range s.log.OpenCalls() {
		s.emit(Event{Type: EventToolResult, TaskID: c.task, Call: &ToolCall{ID: c.id,
			Error: "interrupted: the sandbox worker restarted before Mosso answered this call. If it was a push or a pull request change, check with " + ToolListPRs + " before trying again."}})
	}
}

func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// CallTool sends a tool call to the worker at base (for example
// http://127.0.0.1:7777) and waits for the answer. It is what the MCP
// server does for Claude.
func CallTool(ctx context.Context, base, name string, args json.RawMessage) (CallAnswer, error) {
	body, err := json.Marshal(CallRequest{Name: name, Arguments: args})
	if err != nil {
		return CallAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/tools/calls", bytes.NewReader(body))
	if err != nil {
		return CallAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CallAnswer{}, fmt.Errorf("the Mosso worker in this sandbox did not answer: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*maxCallBytes))
	if err != nil {
		return CallAnswer{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return CallAnswer{}, errors.New(e.Error)
		}
		return CallAnswer{}, fmt.Errorf("the Mosso worker answered HTTP %d", resp.StatusCode)
	}
	var ans CallAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return CallAnswer{}, fmt.Errorf("the Mosso worker's answer was not understood: %w", err)
	}
	return ans, nil
}
