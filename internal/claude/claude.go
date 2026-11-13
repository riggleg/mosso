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
	// WorkDir is where Claude Code runs. Every turn uses the same directory.
	WorkDir = "/vercel/sandbox"
	// MaxPromptBytes bounds a prompt. It is passed in an environment
	// variable, and Linux limits each one to 128 KiB.
	MaxPromptBytes = 64 * 1024

	// maxStreamBytes caps the stream-json output kept from one run.
	maxStreamBytes = 16 << 20
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
	args := []string{"-c", runScript, "claude"}
	if req.SessionID != "" {
		if !ValidSessionID(req.SessionID) {
			return nil, errors.New("invalid session_id")
		}
		args = append(args, "--resume", req.SessionID)
	}
	res, err := vc.Run(ctx, sandboxSession, vercel.Command{
		Name: "bash",
		Args: args,
		Cwd:  WorkDir,
		Env: map[string]string{
			EnvToken:              token.Reveal(),
			"MOSSO_PROMPT":        req.Prompt,
			"DISABLE_AUTOUPDATER": "1",
		},
		MaxOutputBytes: maxStreamBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("run claude code: %w", err)
	}
	turn, err := ParseStream(token.redact(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("run claude code: exit code %d: %w: %s", res.ExitCode, err, tail(token.redact(res.Stderr)))
	}
	return turn, nil
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
	turn := &Turn{Events: []Event{}}
	var sawResult bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m streamMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.SessionID != "" && turn.SessionID == "" {
			turn.SessionID = m.SessionID
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" && m.Model != "" {
				turn.Model = m.Model
			}
		case "assistant", "user":
			turn.Events = append(turn.Events, contentEvents(m.Message.Content, m.ParentToolUseID)...)
		case "result":
			sawResult = true
			if m.SessionID != "" {
				turn.SessionID = m.SessionID
			}
			turn.Result, turn.IsError, turn.Subtype = m.Result, m.IsError, m.Subtype
			turn.NumTurns, turn.DurationMS, turn.TotalCostUSD = m.NumTurns, m.DurationMS, m.TotalCostUSD
		}
	}
	if !sawResult {
		return nil, errors.New("claude code produced no result")
	}
	return turn, nil
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
