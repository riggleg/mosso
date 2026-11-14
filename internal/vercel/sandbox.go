// Package vercel is a minimal client for the Vercel Sandbox REST API.
//
// It mirrors the endpoints used by the official @vercel/sandbox SDK and only
// implements what Mosso needs today: create a sandbox, run a command (with
// optional environment and working directory), read its output and exit
// status, and destroy the sandbox.
package vercel

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
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultBaseURL is Vercel's documented public REST API endpoint.
	DefaultBaseURL = "https://api.vercel.com"
	// DefaultRuntime is the sandbox image used when none is requested.
	DefaultRuntime = "node22"
	// DefaultTimeout is how long Vercel keeps a sandbox alive before stopping
	// it on its own. It is the backstop against abandoned, chargeable sandboxes.
	DefaultTimeout = 10 * time.Minute
	// MaxTimeout is the longest sandbox lifetime Mosso will request. It is the
	// Vercel Hobby plan limit (Pro/Enterprise allow more), so requests work on
	// every plan and a forgotten sandbox cannot run for hours.
	MaxTimeout = 45 * time.Minute
	// MaxOutputBytes caps how much stdout/stderr is kept per stream.
	MaxOutputBytes = 1 << 20

	envToken     = "VERCEL_TOKEN"
	envTeamID    = "VERCEL_TEAM_ID"
	envProjectID = "VERCEL_PROJECT_ID"
)

// Config holds the credentials and identifiers needed to call the API.
type Config struct {
	Token     string
	TeamID    string
	ProjectID string
	// BaseURL overrides DefaultBaseURL (used by tests).
	BaseURL string
	// HTTPClient overrides http.DefaultClient's settings (used by tests).
	HTTPClient *http.Client
}

// ErrMissingConfig is returned when required environment variables are unset.
var ErrMissingConfig = errors.New("vercel sandbox credentials are not configured")

// ConfigFromEnv reads VERCEL_TOKEN, VERCEL_TEAM_ID and VERCEL_PROJECT_ID.
// The returned error names missing variables but never includes their values.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Token:     strings.TrimSpace(os.Getenv(envToken)),
		TeamID:    strings.TrimSpace(os.Getenv(envTeamID)),
		ProjectID: strings.TrimSpace(os.Getenv(envProjectID)),
	}
	var missing []string
	if cfg.Token == "" {
		missing = append(missing, envToken)
	}
	if cfg.TeamID == "" {
		missing = append(missing, envTeamID)
	}
	if cfg.ProjectID == "" {
		missing = append(missing, envProjectID)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("%w: set %s", ErrMissingConfig, strings.Join(missing, ", "))
	}
	return cfg, nil
}

// Client calls the Vercel Sandbox API.
type Client struct {
	cfg  Config
	http *http.Client
	// stream is http without an overall timeout, for log streams.
	stream *http.Client
}

// NewClient returns a client for cfg.
func NewClient(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Minute}
	}
	stream := *hc
	stream.Timeout = 0
	return &Client{cfg: cfg, http: hc, stream: &stream}
}

// Sandbox identifies a provisioned sandbox. Commands run in its current
// session; a persistent sandbox gets a new session each time it resumes.
type Sandbox struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	// Status is the current session's status: pending, running, stopping,
	// stopped, snapshotting, failed or aborted.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Persistent sandboxes snapshot their filesystem when a session stops
	// and resume from it.
	Persistent bool `json:"persistent,omitempty"`
	// StartedAt and Timeout describe the current session: Vercel stops it
	// at StartedAt+Timeout. StartedAt is zero when the session has not
	// started yet.
	StartedAt time.Time     `json:"started_at,omitzero"`
	Timeout   time.Duration `json:"-"`
}

// Running reports whether the sandbox has a session that is starting or
// running.
func (sb Sandbox) Running() bool { return sb.Status == "pending" || sb.Status == "running" }

// ExpiresAt is when Vercel stops the current session, or the zero time
// when unknown.
func (sb Sandbox) ExpiresAt() time.Time {
	if sb.StartedAt.IsZero() || sb.Timeout <= 0 {
		return time.Time{}
	}
	return sb.StartedAt.Add(sb.Timeout)
}

// CreateOptions configures a new sandbox. Zero values use defaults.
type CreateOptions struct {
	Runtime string
	Timeout time.Duration
	// Persistent keeps the filesystem across sessions: Vercel snapshots it
	// when a session stops (also at the timeout) and resumes from the
	// snapshot. Snapshots cost storage, so non-persistent is the default.
	Persistent bool
	// SnapshotExpiration is how long a persistent sandbox's snapshots are
	// kept after their last use (Vercel's default is 30 days; at least one
	// day). KeepLastSnapshots keeps only the most recent N snapshots (1 to
	// 10) and deletes the rest at once, keeping storage flat.
	SnapshotExpiration time.Duration
	KeepLastSnapshots  int
}

// SessionStopped reports whether err is Vercel saying the session is no
// longer running (410 Gone), for example because it timed out. A persistent
// sandbox can then be resumed with Get.
func SessionStopped(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusGone
}

// SessionOver reports whether err means commands can no longer run in the
// session: it stopped (410) or is unknown (404).
func SessionOver(err error) bool { return SessionStopped(err) || IsNotFound(err) }

// SnapshotGone reports whether err is Vercel refusing to resume a sandbox
// because its snapshot expired or failed. The sandbox cannot be used again.
func SnapshotGone(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusGone && apiErr.Code == "snapshot_not_found"
}

// InTransition reports whether err is Vercel saying the sandbox is
// stopping or snapshotting right now (422); the request can be retried in
// a few seconds.
func InTransition(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity &&
		(apiErr.Code == "sandbox_stopping" || apiErr.Code == "sandbox_snapshotting")
}

// CommandResult is the outcome of a finished command.
type CommandResult struct {
	CommandID string `json:"command_id"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
}

// APIError is a non-2xx response from Vercel. It carries only the status
// code and Vercel's error code and message, never request headers.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("vercel api: status %d", e.StatusCode)
	}
	return fmt.Sprintf("vercel api: status %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// NewSandboxName returns a unique name for a Mosso-created sandbox. Names are
// chosen client-side so a sandbox can be deleted even if the create response
// is lost.
func NewSandboxName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "mosso-" + hex.EncodeToString(b[:])
}

// sandboxResponse is the shape of Vercel's sandbox-and-session responses.
type sandboxResponse struct {
	Sandbox struct {
		Name             string `json:"name"`
		CurrentSessionID string `json:"currentSessionId"`
		Status           string `json:"status"`
		Persistent       bool   `json:"persistent"`
		Timeout          int64  `json:"timeout"`
		CreatedAt        int64  `json:"createdAt"`
	} `json:"sandbox"`
	Session struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Timeout   int64  `json:"timeout"`
		StartedAt int64  `json:"startedAt"`
		CreatedAt int64  `json:"createdAt"`
	} `json:"session"`
	Resumed bool `json:"resumed"`
}

// sandbox turns a response into a Sandbox, or fails if it has no session.
func (r sandboxResponse) sandbox(name string, created time.Time) (*Sandbox, error) {
	sessionID := r.Session.ID
	if sessionID == "" {
		sessionID = r.Sandbox.CurrentSessionID
	}
	if sessionID == "" {
		return nil, errors.New("response did not include a session id")
	}
	if r.Sandbox.Name != "" {
		name = r.Sandbox.Name
	}
	status := r.Session.Status
	if status == "" {
		status = r.Sandbox.Status
	}
	sb := &Sandbox{Name: name, SessionID: sessionID, Status: status, CreatedAt: created, Persistent: r.Sandbox.Persistent}
	if r.Sandbox.CreatedAt > 0 {
		sb.CreatedAt = time.UnixMilli(r.Sandbox.CreatedAt).UTC()
	}
	if ms := r.Session.Timeout; ms > 0 {
		sb.Timeout = time.Duration(ms) * time.Millisecond
	} else if r.Sandbox.Timeout > 0 {
		sb.Timeout = time.Duration(r.Sandbox.Timeout) * time.Millisecond
	}
	if start := r.Session.StartedAt; start > 0 {
		sb.StartedAt = time.UnixMilli(start).UTC()
	} else if r.Session.CreatedAt > 0 && (status == "pending" || status == "running") {
		sb.StartedAt = time.UnixMilli(r.Session.CreatedAt).UTC()
	}
	return sb, nil
}

// Create provisions a sandbox called name. If the request fails after it
// may have reached Vercel, Create attempts to delete the sandbox so it does
// not linger and accrue charges.
func (c *Client) Create(ctx context.Context, name string, opts CreateOptions) (sb *Sandbox, err error) {
	if opts.Runtime == "" {
		opts.Runtime = DefaultRuntime
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Timeout > MaxTimeout {
		return nil, fmt.Errorf("create sandbox: timeout %s exceeds the %s maximum", opts.Timeout, MaxTimeout)
	}
	body := map[string]any{
		"projectId":  c.cfg.ProjectID,
		"name":       name,
		"runtime":    opts.Runtime,
		"timeout":    opts.Timeout.Milliseconds(),
		"persistent": opts.Persistent,
		"ports":      []int{},
	}
	if opts.Persistent {
		if opts.SnapshotExpiration > 0 {
			body["snapshotExpiration"] = opts.SnapshotExpiration.Milliseconds()
		}
		if opts.KeepLastSnapshots > 0 {
			body["keepLastSnapshots"] = map[string]any{"count": opts.KeepLastSnapshots, "deleteEvicted": true}
		}
	}
	var resp sandboxResponse
	defer func() {
		if err != nil {
			// Best effort, independent of the caller's (possibly cancelled) context.
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if derr := c.deleteSandbox(cctx, name, true); derr != nil && !IsNotFound(derr) {
				err = fmt.Errorf("%w (cleanup of sandbox %s also failed: %v)", err, name, derr)
			}
		}
	}()
	if err := c.doJSON(ctx, http.MethodPost, "/v2/sandboxes", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	sb, err = resp.sandbox(name, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	if sb.StartedAt.IsZero() {
		sb.StartedAt = time.Now().UTC()
	}
	if sb.Timeout == 0 {
		sb.Timeout = opts.Timeout
	}
	return sb, nil
}

// Get looks up a sandbox by name. With resume, a stopped persistent sandbox
// is resumed first: Vercel boots a new session from the last snapshot, and
// the returned Sandbox has the new session ID and expiry. Errors:
// IsNotFound when the sandbox does not exist, SnapshotGone when it cannot
// resume any more, InTransition when it is stopping or snapshotting (retry
// shortly).
func (c *Client) Get(ctx context.Context, name string, resume bool) (*Sandbox, error) {
	q := url.Values{"projectId": {c.cfg.ProjectID}}
	if resume {
		q.Set("resume", "true")
	}
	var resp sandboxResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v2/sandboxes/"+url.PathEscape(name), q, nil, &resp); err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	sb, err := resp.sandbox(name, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	return sb, nil
}

// Stop stops the sandbox's current session. A persistent sandbox is
// snapshotted and can be resumed with Get; a non-persistent one loses its
// filesystem. An already stopped or unknown session is not an error.
func (c *Client) Stop(ctx context.Context, sessionID string) error {
	path := "/v2/sandboxes/sessions/" + url.PathEscape(sessionID) + "/stop"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, nil, nil); err != nil && !SessionOver(err) {
		return fmt.Errorf("stop session: %w", err)
	}
	return nil
}

type apiCommand struct {
	ID       string `json:"id"`
	ExitCode *int   `json:"exitCode"`
}

// Command is a process to start in a sandbox session.
type Command struct {
	// Name is the executable, e.g. "bash".
	Name string
	Args []string
	// Env adds environment variables for this command only. Values are sent
	// in the request body (never the URL) and are never logged; use it for
	// secrets rather than embedding them in Args.
	Env map[string]string
	// Cwd is the working directory; empty uses the sandbox default.
	Cwd string
	// MaxOutputBytes caps stdout and stderr each; zero uses MaxOutputBytes.
	MaxOutputBytes int
	// Stdout, if set, receives the command's stdout while it runs, in
	// order and exactly once, instead of collecting it: the result's
	// Stdout is then empty. It is called from Run's goroutine and never
	// after Run returns.
	Stdout func(chunk string)
}

// Exec runs script with `bash -c` in the sandbox session, waits for it to
// finish, and returns its exit code and output.
func (c *Client) Exec(ctx context.Context, sessionID, script string) (*CommandResult, error) {
	return c.Run(ctx, sessionID, Command{Name: "bash", Args: []string{"-c", script}})
}

// Run starts cmd in the sandbox session, waits for it to finish, and returns
// its exit code and output. If ctx ends first, Run asks Vercel to terminate
// the command so it does not keep running unobserved.
func (c *Client) Run(ctx context.Context, sessionID string, cmd Command) (*CommandResult, error) {
	sid := url.PathEscape(sessionID)
	var started struct {
		Command apiCommand `json:"command"`
	}
	body := map[string]any{"command": cmd.Name, "args": cmd.Args}
	if len(cmd.Env) > 0 {
		body["env"] = cmd.Env
	}
	if cmd.Cwd != "" {
		body["cwd"] = cmd.Cwd
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v2/sandboxes/sessions/"+sid+"/cmd", nil, body, &started); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}
	if started.Command.ID == "" {
		return nil, errors.New("start command: response did not include a command id")
	}
	cmdPath := "/v2/sandboxes/sessions/" + sid + "/cmd/" + url.PathEscape(started.Command.ID)

	limit := cmd.MaxOutputBytes
	if limit <= 0 {
		limit = MaxOutputBytes
	}
	if cmd.Stdout != nil {
		return c.follow(ctx, cmdPath, started.Command, cmd.Stdout, limit)
	}

	finished, err := c.wait(ctx, cmdPath, started.Command)
	if err != nil {
		if ctx.Err() != nil {
			c.kill(cmdPath)
		}
		return nil, err
	}

	stdout, stderr, err := c.logs(ctx, cmdPath+"/logs", limit)
	if err != nil {
		return nil, fmt.Errorf("read command logs: %w", err)
	}
	return &CommandResult{CommandID: started.Command.ID, ExitCode: *finished.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

// wait polls until the command has an exit code.
func (c *Client) wait(ctx context.Context, cmdPath string, cmd apiCommand) (apiCommand, error) {
	for cmd.ExitCode == nil {
		var finished struct {
			Command apiCommand `json:"command"`
		}
		if err := c.doJSON(ctx, http.MethodGet, cmdPath, url.Values{"wait": {"true"}}, nil, &finished); err != nil {
			return cmd, fmt.Errorf("wait for command: %w", err)
		}
		cmd = finished.Command
		if cmd.ExitCode == nil {
			select {
			case <-ctx.Done():
				return cmd, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return cmd, nil
}

// kill sends SIGTERM to a command whose caller has gone away. Best effort.
func (c *Client) kill(cmdPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.doJSON(ctx, http.MethodPost, cmdPath+"/kill", nil, map[string]int{"signal": 15}, nil)
}

// logs reads the complete output of a finished command.
func (c *Client) logs(ctx context.Context, path string, limit int) (string, string, error) {
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = limit, limit
	err := c.readLogs(ctx, path, func(stream, data string) {
		if stream == "stdout" {
			stdout.WriteString(data)
		} else {
			stderr.WriteString(data)
		}
	})
	if err != nil {
		return "", "", err
	}
	return stdout.String(), stderr.String(), nil
}

// readLogs reads a command's newline-delimited JSON log stream and calls fn
// with each stdout or stderr chunk. Vercel keeps the stream open while the
// command runs, so chunks arrive as the command produces them, and ends it
// when the command exits. Every request replays the output from the start.
func (c *Client) readLogs(ctx context.Context, path string, fn func(stream, data string)) error {
	resp, err := c.doStream(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A json.Decoder reads newline-delimited JSON with no per-line size limit,
	// so one very large log chunk cannot fail the whole exec.
	dec := json.NewDecoder(resp.Body)
	for {
		var entry struct {
			Stream string          `json:"stream"`
			Data   json.RawMessage `json:"data"`
		}
		if err := dec.Decode(&entry); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode log line: %w", err)
		}
		switch entry.Stream {
		case "stdout", "stderr":
			var s string
			if err := json.Unmarshal(entry.Data, &s); err != nil {
				return fmt.Errorf("decode log data: %w", err)
			}
			fn(entry.Stream, s)
		case "error":
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(entry.Data, &e)
			return fmt.Errorf("log stream error %s: %s", e.Code, e.Message)
		}
	}
}

// followRetries is how many times in a row follow reconnects to a log
// stream that fails without delivering anything new.
const followRetries = 3

// follow streams a running command's stdout to onStdout until the command
// exits. Like the official SDK, it reconnects if the log stream breaks and
// skips the output it has already delivered. If ctx ends first, it asks
// Vercel to terminate the command.
func (c *Client) follow(ctx context.Context, cmdPath string, cmd apiCommand, onStdout func(string), limit int) (*CommandResult, error) {
	var stderr limitedBuffer
	stderr.max = limit
	delivered := map[string]int{} // bytes delivered so far, per stream
	for failures := 0; ; {
		skip := map[string]int{"stdout": delivered["stdout"], "stderr": delivered["stderr"]}
		before := delivered["stdout"] + delivered["stderr"]
		err := c.readLogs(ctx, cmdPath+"/logs", func(stream, data string) {
			n := min(skip[stream], len(data))
			skip[stream] -= n
			if data = data[n:]; data == "" {
				return
			}
			delivered[stream] += len(data)
			if stream == "stdout" {
				onStdout(data)
			} else {
				stderr.WriteString(data)
			}
		})
		if err == nil {
			// The stream ended because the command exited.
			finished, err := c.wait(ctx, cmdPath, cmd)
			if err == nil {
				return &CommandResult{CommandID: cmd.ID, ExitCode: *finished.ExitCode, Stderr: stderr.String()}, nil
			}
			if ctx.Err() != nil {
				c.kill(cmdPath)
			}
			return nil, err
		}
		if ctx.Err() != nil {
			c.kill(cmdPath)
			return nil, ctx.Err()
		}
		if delivered["stdout"]+delivered["stderr"] > before {
			failures = 0
		}
		if failures++; failures > followRetries {
			c.kill(cmdPath)
			return nil, fmt.Errorf("follow command output: %w", err)
		}
		select {
		case <-ctx.Done():
			c.kill(cmdPath)
			return nil, ctx.Err()
		case <-time.After(time.Duration(200<<failures) * time.Millisecond):
		}
	}
}

// Destroy stops the sandbox's running session and deletes the sandbox,
// along with the snapshots no other sandbox uses, so nothing of it is kept
// or billed. It is idempotent: resources that are already gone are not
// treated as errors.
func (c *Client) Destroy(ctx context.Context, sb Sandbox) error {
	var errs []error
	if sb.SessionID != "" {
		path := "/v2/sandboxes/sessions/" + url.PathEscape(sb.SessionID) + "/stop"
		if err := c.doJSON(ctx, http.MethodPost, path, nil, nil, nil); err != nil && !SessionOver(err) {
			errs = append(errs, fmt.Errorf("stop session: %w", err))
		}
	}
	if err := c.deleteSandbox(ctx, sb.Name, true); err != nil && !IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete sandbox: %w", err))
	}
	return errors.Join(errs...)
}

func (c *Client) deleteSandbox(ctx context.Context, name string, orphanSnapshots bool) error {
	q := url.Values{"projectId": {c.cfg.ProjectID}}
	if orphanSnapshots {
		q.Set("deleteOrphanSnapshots", "true")
	}
	return c.doJSON(ctx, http.MethodDelete, "/v2/sandboxes/"+url.PathEscape(name), q, nil, nil)
}

// doJSON sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, q url.Values, in, out any) error {
	if in == nil && method == http.MethodPost {
		// Vercel rejects body-less POSTs (e.g. /stop) with 415 Unsupported
		// Media Type, so always send an empty JSON object.
		in = struct{}{}
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// doStream is do for a long-lived GET whose body is read as it arrives. It
// has no overall deadline (the client's Timeout would cut off a long
// command's log stream); ctx bounds it instead.
func (c *Client) doStream(ctx context.Context, path string) (*http.Response, error) {
	return c.send(ctx, c.stream, http.MethodGet, path, nil, nil, nil)
}

// do sends an authenticated request and returns the response if it is 2xx.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	return c.send(ctx, c.http, method, path, q, body, nil)
}

// send sends an authenticated request. A body is JSON unless hdr sets its
// Content-Type.
func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, q url.Values, body io.Reader, hdr map[string]string) (*http.Response, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("teamId", c.cfg.TeamID)
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path+"?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "mosso")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

func apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return &APIError{StatusCode: resp.StatusCode, Code: parsed.Error.Code, Message: parsed.Error.Message}
}

// limitedBuffer keeps at most max bytes and notes truncation.
type limitedBuffer struct {
	buf       strings.Builder
	max       int
	truncated bool
}

func (b *limitedBuffer) WriteString(s string) {
	if room := b.max - b.buf.Len(); room < len(s) {
		room = max(room, 0)
		// Back up to a rune boundary so the output stays valid UTF-8.
		for room > 0 && !utf8.RuneStart(s[room]) {
			room--
		}
		s = s[:room]
		b.truncated = true
	}
	b.buf.WriteString(s)
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[mosso: output truncated]"
	}
	return b.buf.String()
}
