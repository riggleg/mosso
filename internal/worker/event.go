// Package worker is the Mosso worker that runs inside each Vercel Sandbox.
//
// The worker owns the Claude Code processes of one workspace: the Mosso
// server submits a message, the worker acknowledges it at once and runs
// Claude Code asynchronously, recording everything Claude does to an
// append-only, fsync'd event log in the sandbox's filesystem. The server
// polls the worker for events by sequence number, so it can disconnect,
// restart or come back after the sandbox session has stopped and resumed,
// and still read everything that happened. The worker's HTTP API listens on
// loopback only; the server reaches it through the Vercel Sandbox command
// API (see internal/server/remote.go).
package worker

import (
	"encoding/json"
	"time"

	"github.com/riggleg/mosso/internal/claude"
)

// DefaultStateDir is where the worker keeps its state in a sandbox. It is
// outside the repository checkout but inside the sandbox's persisted
// filesystem, so it survives a session stop and resume.
const DefaultStateDir = "/vercel/sandbox/.mosso"

// DefaultPort is the loopback port the worker's API listens on.
const DefaultPort = 7777

// Event types.
const (
	// EventUser is a prompt submitted by the user (Text, Attachments).
	EventUser = "user"
	// EventClaude is a piece of Claude Code's activity (Event).
	EventClaude = "claude"
	// EventSession reports the Claude Code session ID of the task
	// (SessionID), as soon as Claude Code prints it.
	EventSession = "session"
	// EventResult ends a task Claude Code finished (Result).
	EventResult = "result"
	// EventError ends a task that failed before Claude Code produced a
	// result (Text explains).
	EventError = "error"
	// EventStopped ends a task the user interrupted.
	EventStopped = "stopped"
	// EventInterrupted ends a task that was still running when the worker
	// last stopped: the sandbox session ended or the worker was restarted.
	EventInterrupted = "interrupted"
	// EventNotice is a message from the worker, for example that the log
	// reached its size limit.
	EventNotice = "notice"
	// EventPing is a heartbeat on a followed event stream. It has no
	// sequence number and is never recorded.
	EventPing = "ping"
)

// Event is one entry of the worker's log. Seq increases by one per event;
// it is the cursor the server polls with.
type Event struct {
	Seq  int64     `json:"seq,omitempty"`
	Time time.Time `json:"time"`
	Type string    `json:"type"`
	// TaskID groups the events of one message.
	TaskID string `json:"task_id,omitempty"`
	Text   string `json:"text,omitempty"`
	// Attachments is opaque to the worker: the server's description of the
	// files attached to a user message, stored and returned as is.
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Event       *claude.Event   `json:"event,omitempty"`
	Result      *Result         `json:"result,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
}

// Terminal reports whether the event ends its task.
func (e Event) Terminal() bool {
	switch e.Type {
	case EventResult, EventError, EventStopped, EventInterrupted:
		return true
	}
	return false
}

// Result is how Claude Code ended a task.
type Result struct {
	Text         string  `json:"text"`
	IsError      bool    `json:"is_error"`
	Subtype      string  `json:"subtype,omitempty"`
	Model        string  `json:"model,omitempty"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Status is the worker's answer to GET /status.
type Status struct {
	// State is "idle" or "running".
	State string `json:"state"`
	// Seq is the sequence number of the latest event.
	Seq int64 `json:"seq"`
	// SessionID is the Claude Code session the worker last saw.
	SessionID string `json:"session_id,omitempty"`
	// TaskID and StartedAt describe the running task.
	TaskID    string     `json:"task_id,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Version identifies the worker build.
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Message is the body of POST /messages: one prompt for Claude Code.
type Message struct {
	// Text is what the user typed; it is recorded as the user event.
	Text        string          `json:"text"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	// Prompt is what Claude Code receives: Text plus, for example, a note
	// about the attachments.
	Prompt string `json:"prompt"`
	// SessionID continues a Claude Code conversation; empty starts one.
	SessionID string `json:"session_id,omitempty"`
	// Dir is the absolute working directory for Claude Code.
	Dir          string `json:"dir"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Token is the Claude Code OAuth token for this task. The worker
	// holds it in memory for the task only and never writes it to disk.
	Token string `json:"token"`
}

// Accepted is the body of a 202 response to POST /messages.
type Accepted struct {
	TaskID string `json:"task_id"`
	// Seq is the sequence number of the recorded user event.
	Seq int64 `json:"seq"`
}
