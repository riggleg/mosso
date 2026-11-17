package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/claude"
)

// DefaultClaudePath is where the official installer puts Claude Code for
// the sandbox user.
func DefaultClaudePath() string {
	home, _ := os.UserHomeDir()
	return home + "/.local/bin/claude"
}

// maxStderrBytes bounds the Claude Code stderr kept for error messages.
const maxStderrBytes = 64 << 10

// runClaude runs one Claude Code turn locally, as a child of the worker,
// and records its activity as it happens. The prompt goes in on stdin and
// the token only in the process environment; the token is redacted from
// every output line before it is recorded. It returns the final turn, or an
// error with the end of stderr. Cancelling ctx sends SIGTERM to Claude
// Code's process group.
func (s *Server) runClaude(ctx context.Context, msg Message, emit func(Event)) (*claude.Turn, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"}
	if cfg := s.mcpConfig(); cfg != "" {
		args = append(args, "--mcp-config", cfg)
	}
	if msg.SessionID != "" {
		args = append(args, "--resume", msg.SessionID)
	}
	if msg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", msg.SystemPrompt)
	}
	// Every turn, resumed or not, passes the model: Claude Code does not
	// remember it in the session.
	if msg.Model != "" {
		args = append(args, "--model", msg.Model)
	}
	cmd := exec.CommandContext(ctx, s.claudePath, args...)
	cmd.Dir = msg.Dir
	cmd.Stdin = strings.NewReader(msg.Prompt)
	cmd.Env = append(os.Environ(), claude.EnvToken+"="+msg.Token, "DISABLE_AUTOUPDATER=1",
		// A tool call can wait for Mosso longer than Claude Code's default;
		// the worker always answers within its own call timeout.
		fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", (s.callTimeout+time.Minute).Milliseconds()))
	// Claude Code's own children live in the same process group, so one
	// SIGTERM reaches them all.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude code: %w", err)
	}
	tok := claude.Token(msg.Token)
	p := claude.NewStreamParser(
		func(id string) { emit(Event{Type: EventSession, SessionID: id}) },
		func(e claude.Event) { emit(Event{Type: EventClaude, Event: &e}) },
	)
	lines := claude.NewLineSplitter(func(line string) { p.Line(tok.Redact(line)) })
	buf := make([]byte, 64<<10)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			lines.Write(string(buf[:n]))
		}
		if err != nil {
			break
		}
	}
	lines.Flush()
	waitErr := cmd.Wait()
	turn, perr := p.Finish()
	if perr != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("claude code exited with code %d: %s: %s", code, perr.Error(), tail(tok.Redact(stderr.String())))
	}
	return turn, nil
}

// mcpConfig is the --mcp-config value that makes Claude Code start the
// worker's MCP server as "mosso", or "" when the worker offers no tools.
func (s *Server) mcpConfig() string {
	if len(s.mcp) == 0 {
		return ""
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"mosso": map[string]any{"type": "stdio", "command": s.mcp[0], "args": s.mcp[1:]},
	}})
	return string(b)
}

// limitedBuffer keeps the first maxStderrBytes written to it and accepts
// (and discards) the rest, so the writer never sees a short write.
type limitedBuffer struct {
	b strings.Builder
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := maxStderrBytes - l.b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.b.Write(p)
	}
	return n, nil
}

func (l *limitedBuffer) String() string { return l.b.String() }

var _ io.Writer = (*limitedBuffer)(nil)

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
