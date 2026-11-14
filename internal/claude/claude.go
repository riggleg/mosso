// Package claude runs the official Claude Code CLI headlessly inside a Vercel
// Sandbox, authenticated with a `claude setup-token` subscription token.
//
// Each message is one `claude -p` run with stream-json output. Follow-ups
// pass the previous run's session ID to --resume; Claude Code keeps the
// transcript on the sandbox's disk, so a conversation lives as long as its
// sandbox.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/vercel"
)

const (
	// WorkDir is where Claude Code runs unless a request sets Dir.
	WorkDir = "/vercel/sandbox"
	// MaxPromptBytes bounds a prompt. It is passed in an environment
	// variable, and Linux limits each one to 128 KiB.
	MaxPromptBytes = 64 * 1024

	// maxLineBytes caps one stream-json line; longer lines are skipped.
	maxLineBytes = 16 << 20
	// maxEventBytes caps the stream-json output whose events are kept in
	// a Turn. Later events still reach Request.OnEvent, and the final
	// result is still read.
	maxEventBytes = 16 << 20
	// maxToolResultBytes caps each tool result returned to API callers.
	maxToolResultBytes = 16 << 10
)

// installScript installs Claude Code with the official native installer
// (stable channel) for the sandbox user, unless it is already there, then
// prints the version.
const installScript = `set -euo pipefail
bin="$HOME/.local/bin/claude"
if [ ! -x "$bin" ]; then
  curl -fsSL https://claude.ai/install.sh | bash -s stable >&2
fi
"$bin" --version`

// runScript feeds the prompt on stdin, so it needs no shell quoting and cannot
// be mistaken for a flag. Extra flags (--resume) arrive as "$@". exec makes
// claude the command's process, so a kill reaches it directly.
//
// Permissions are bypassed because nobody is there to approve tool calls; the
// Vercel Sandbox is the isolation boundary. Claude Code's own Bash sandbox
// (and CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, which requires it) cannot start in
// Vercel Sandbox: bubblewrap fails to mount /proc there. See
// docs/claude-code.md.
const runScript = `exec "$HOME/.local/bin/claude" -p --output-format stream-json --verbose --permission-mode bypassPermissions "$@" <<<"$MOSSO_PROMPT"`

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidSessionID reports whether id looks like a Claude Code session ID.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// Install makes sure Claude Code is installed in the sandbox session and
// returns its version string. It is safe to call more than once.
func Install(ctx context.Context, vc *vercel.Client, sandboxSession string) (string, error) {
	res, err := vc.Exec(ctx, sandboxSession, installScript)
	if err != nil {
		return "", fmt.Errorf("install claude code: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("install claude code: exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Request is one message to Claude.
type Request struct {
	Prompt string
	// SessionID continues an earlier conversation in the same sandbox.
	// Empty starts a new one.
	SessionID string
	// Dir is the absolute working directory; empty uses WorkDir. Claude
	// Code stores sessions per directory, so a conversation must keep the
	// same one.
	Dir string
	// SystemPrompt, if set, is appended to Claude Code's system prompt for
	// this turn, for example to explain the workspace's Git setup.
	SystemPrompt string

	// OnSession, if set, is called with the session ID as soon as Claude
	// Code reports it, so an interrupted turn can still be resumed.
	OnSession func(id string)
	// OnEvent, if set, is called with each event as Claude produces it,
	// before Send returns. The same events are in the returned Turn.
	OnEvent func(Event)
}

// Turn is the outcome of one message: Claude's final answer plus the text
// and tool activity that led to it.
type Turn struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitempty"`
	// Result is Claude's final response text, or the error message when
	// IsError is true (for example an expired token or a usage limit).
	Result     string `json:"result"`
	IsError    bool   `json:"is_error"`
	Subtype    string `json:"subtype,omitempty"`
	NumTurns   int    `json:"num_turns"`
	DurationMS int64  `json:"duration_ms"`
	// TotalCostUSD is Claude Code's client-side estimate for the whole
	// session. Subscription usage is not billed per token.
	TotalCostUSD float64 `json:"total_cost_usd"`
	Events       []Event `json:"events"`
}

// Event is a piece of Claude's activity during a turn.
type Event struct {
	// Type is "text", "tool_use" or "tool_result".
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ToolUseID links a tool_result to its tool_use.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// ParentToolUseID is set for activity inside a subagent.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
}

// Send runs one Claude Code turn in the sandbox and waits for it to finish.
// The token reaches Claude Code only through the command's environment; any
// occurrence of it in the output is redacted before parsing.
func Send(ctx context.Context, vc *vercel.Client, sandboxSession string, token Token, req Request) (*Turn, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if len(req.Prompt) > MaxPromptBytes || strings.ContainsRune(req.Prompt, 0) {
		return nil, fmt.Errorf("prompt must be at most %d bytes of text", MaxPromptBytes)
	}
	dir := req.Dir
	if dir == "" {
		dir = WorkDir
	}
	if !strings.HasPrefix(dir, "/") {
		return nil, errors.New("dir must be an absolute path")
	}
	args := []string{"-c", runScript, "claude"}
	if req.SessionID != "" {
		if !ValidSessionID(req.SessionID) {
			return nil, errors.New("invalid session_id")
		}
		args = append(args, "--resume", req.SessionID)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	// Output is parsed line by line as it streams out of the sandbox. The
	// token cannot contain a newline, so redacting whole lines is enough.
	p := NewStreamParser(req.OnSession, req.OnEvent)
	lines := NewLineSplitter(func(line string) { p.Line(token.redact(line)) })
	res, err := vc.Run(ctx, sandboxSession, vercel.Command{
		Name: "bash",
		Args: args,
		Cwd:  dir,
		Env: map[string]string{
			EnvToken:              token.Reveal(),
			"MOSSO_PROMPT":        req.Prompt,
			"DISABLE_AUTOUPDATER": "1",
		},
		Stdout: lines.Write,
	})
	if err != nil {
		return nil, fmt.Errorf("run claude code: %w", err)
	}
	lines.Flush()
	turn, err := p.Finish()
	if err != nil {
		return nil, fmt.Errorf("run claude code: exit code %d: %w: %s", res.ExitCode, err, tail(token.redact(res.Stderr)))
	}
	return turn, nil
}

// LineSplitter turns a stream of chunks into complete lines. Lines longer
// than maxLineBytes are dropped, so one huge line cannot exhaust memory.
type LineSplitter struct {
	fn       func(line string)
	line     strings.Builder
	skipping bool // inside a line longer than maxLineBytes
}

// NewLineSplitter returns a splitter that calls fn with each complete line,
// without its newline.
func NewLineSplitter(fn func(line string)) *LineSplitter { return &LineSplitter{fn: fn} }

// Write feeds the next chunk of the stream.
func (s *LineSplitter) Write(chunk string) {
	for chunk != "" {
		part, rest, complete := strings.Cut(chunk, "\n")
		chunk = rest
		if !s.skipping && s.line.Len()+len(part) <= maxLineBytes {
			s.line.WriteString(part)
		} else {
			s.skipping = true
			s.line.Reset()
		}
		if complete {
			if !s.skipping {
				s.fn(s.line.String())
			}
			s.line.Reset()
			s.skipping = false
		}
	}
}

// Flush delivers an unterminated last line, if any.
func (s *LineSplitter) Flush() {
	if !s.skipping && s.line.Len() > 0 {
		s.fn(s.line.String())
	}
	s.line.Reset()
	s.skipping = false
}

// streamMessage is the subset of a stream-json line Mosso reads.
type streamMessage struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	SessionID       string `json:"session_id"`
	Model           string `json:"model"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	Message         struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Fields of the final "result" message.
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ParseStream turns `claude -p --output-format stream-json --verbose` output
// into a Turn. Lines that are not JSON, and message types Mosso does not use,
// are skipped. It fails if the stream has no final result message.
func ParseStream(out string) (*Turn, error) {
	p := NewStreamParser(nil, nil)
	for _, line := range strings.Split(out, "\n") {
		p.Line(line)
	}
	return p.Finish()
}

// StreamParser builds a Turn from stream-json output one line at a time.
// It is what Send uses, exported so the sandbox worker can parse the same
// output from a local Claude Code process.
type StreamParser struct {
	turn      Turn
	sawResult bool
	bytes     int // stream-json bytes whose events are in turn.Events
	onSession func(string)
	onEvent   func(Event)
}

// NewStreamParser returns a parser. onSession, if set, is called once with
// the session ID as soon as it is known; onEvent, if set, with each event.
func NewStreamParser(onSession func(string), onEvent func(Event)) *StreamParser {
	return &StreamParser{turn: Turn{Events: []Event{}}, onSession: onSession, onEvent: onEvent}
}

// Line parses one line of output. Lines that are not JSON are skipped.
func (p *StreamParser) Line(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return
	}
	var m streamMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return
	}
	if m.SessionID != "" && p.turn.SessionID == "" {
		p.turn.SessionID = m.SessionID
		if p.onSession != nil {
			p.onSession(m.SessionID)
		}
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" && m.Model != "" {
			p.turn.Model = m.Model
		}
	case "assistant", "user":
		keep := p.bytes+len(line) <= maxEventBytes
		if keep {
			p.bytes += len(line)
		}
		for _, e := range contentEvents(m.Message.Content, m.ParentToolUseID) {
			if keep {
				p.turn.Events = append(p.turn.Events, e)
			}
			if p.onEvent != nil {
				p.onEvent(e)
			}
		}
	case "result":
		p.sawResult = true
		if m.SessionID != "" {
			p.turn.SessionID = m.SessionID
		}
		t := &p.turn
		t.Result, t.IsError, t.Subtype = m.Result, m.IsError, m.Subtype
		t.NumTurns, t.DurationMS, t.TotalCostUSD = m.NumTurns, m.DurationMS, m.TotalCostUSD
	}
}

// Finish returns the Turn. It fails if no final result message was seen.
func (p *StreamParser) Finish() (*Turn, error) {
	if !p.sawResult {
		return nil, errors.New("claude code produced no result")
	}
	return &p.turn, nil
}

func contentEvents(raw json.RawMessage, parent string) []Event {
	var blocks []contentBlock
	// User prompts echo back as a plain string; only block arrays matter.
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var events []Event
	for _, b := range blocks {
		e := Event{Type: b.Type, ParentToolUseID: parent}
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			e.Text = b.Text
		case "tool_use":
			e.ToolUseID, e.Name, e.Input = b.ID, b.Name, b.Input
		case "tool_result":
			e.ToolUseID, e.IsError = b.ToolUseID, b.IsError
			e.Content = truncate(toolResultText(b.Content), maxToolResultBytes)
		default: // thinking, images, etc.
			continue
		}
		events = append(events, e)
	}
	return events
}

// toolResultText flattens tool_result content, which is either a string or
// an array of content blocks.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else if b.Type != "" {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n[mosso: truncated]"
}

// tail returns the end of command output for error messages.
func tail(s string) string {
	s = strings.TrimSpace(s)
	const n = 2048
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
