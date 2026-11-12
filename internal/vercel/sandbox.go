// Package vercel is a minimal client for the Vercel Sandbox REST API.
//
// It mirrors the endpoints used by the official @vercel/sandbox SDK and only
// implements what Mosso needs today: create a sandbox, run a shell command,
// read its output and exit status, and destroy the sandbox.
package vercel

import (
	"bufio"
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
	return &Client{cfg: cfg, http: hc}
}

// Sandbox identifies a provisioned sandbox. Commands run in its session.
type Sandbox struct {
	Name      string    `json:"name"`
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateOptions configures a new sandbox. Zero values use defaults.
type CreateOptions struct {
	Runtime string
	Timeout time.Duration
}

// CommandResult is the outcome of a finished command.
type CommandResult struct {
	CommandID string `json:"command_id"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
}

// APIError is a non-2xx response from Vercel. It carries only the status code
// and Vercel's error message, never request headers.
type APIError struct {
	StatusCode int
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

// Create provisions a non-persistent sandbox called name. If the request fails
// after it may have reached Vercel, Create attempts to delete the sandbox so it
// does not linger and accrue charges.
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
		"persistent": false,
		"ports":      []int{},
	}
	var resp struct {
		Sandbox struct {
			Name             string `json:"name"`
			CurrentSessionID string `json:"currentSessionId"`
			Status           string `json:"status"`
		} `json:"sandbox"`
		Session struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"session"`
	}
	defer func() {
		if err != nil {
			// Best effort, independent of the caller's (possibly cancelled) context.
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if derr := c.deleteSandbox(cctx, name); derr != nil && !IsNotFound(derr) {
				err = fmt.Errorf("%w (cleanup of sandbox %s also failed: %v)", err, name, derr)
			}
		}
	}()
	if err := c.doJSON(ctx, http.MethodPost, "/v2/sandboxes", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	sessionID := resp.Session.ID
	if sessionID == "" {
		sessionID = resp.Sandbox.CurrentSessionID
	}
	if sessionID == "" {
		return nil, errors.New("create sandbox: response did not include a session id")
	}
	if resp.Sandbox.Name != "" && resp.Sandbox.Name != name {
		name = resp.Sandbox.Name
	}
	status := resp.Session.Status
	if status == "" {
		status = resp.Sandbox.Status
	}
	return &Sandbox{Name: name, SessionID: sessionID, Status: status, CreatedAt: time.Now().UTC()}, nil
}

type apiCommand struct {
	ID       string `json:"id"`
	ExitCode *int   `json:"exitCode"`
}

// Exec runs script with `bash -c` in the sandbox session, waits for it to
// finish, and returns its exit code and output.
func (c *Client) Exec(ctx context.Context, sessionID, script string) (*CommandResult, error) {
	sid := url.PathEscape(sessionID)
	var started struct {
		Command apiCommand `json:"command"`
	}
	body := map[string]any{"command": "bash", "args": []string{"-c", script}}
	if err := c.doJSON(ctx, http.MethodPost, "/v2/sandboxes/sessions/"+sid+"/cmd", nil, body, &started); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}
	if started.Command.ID == "" {
		return nil, errors.New("start command: response did not include a command id")
	}
	cmdPath := "/v2/sandboxes/sessions/" + sid + "/cmd/" + url.PathEscape(started.Command.ID)

	cmd := started.Command
	for cmd.ExitCode == nil {
		var finished struct {
			Command apiCommand `json:"command"`
		}
		if err := c.doJSON(ctx, http.MethodGet, cmdPath, url.Values{"wait": {"true"}}, nil, &finished); err != nil {
			return nil, fmt.Errorf("wait for command: %w", err)
		}
		cmd = finished.Command
		if cmd.ExitCode == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	stdout, stderr, err := c.logs(ctx, cmdPath+"/logs")
	if err != nil {
		return nil, fmt.Errorf("read command logs: %w", err)
	}
	return &CommandResult{CommandID: started.Command.ID, ExitCode: *cmd.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

// logs reads the newline-delimited JSON log stream of a finished command.
func (c *Client) logs(ctx context.Context, path string) (string, string, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = MaxOutputBytes, MaxOutputBytes
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*MaxOutputBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Stream string          `json:"stream"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return "", "", fmt.Errorf("decode log line: %w", err)
		}
		switch entry.Stream {
		case "stdout", "stderr":
			var s string
			if err := json.Unmarshal(entry.Data, &s); err != nil {
				return "", "", fmt.Errorf("decode log data: %w", err)
			}
			if entry.Stream == "stdout" {
				stdout.WriteString(s)
			} else {
				stderr.WriteString(s)
			}
		case "error":
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(entry.Data, &e)
			return "", "", fmt.Errorf("log stream error %s: %s", e.Code, e.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	return stdout.String(), stderr.String(), nil
}

// Destroy stops the sandbox's running session and deletes the sandbox. It is
// idempotent: resources that are already gone are not treated as errors.
func (c *Client) Destroy(ctx context.Context, sb Sandbox) error {
	var errs []error
	if sb.SessionID != "" {
		path := "/v2/sandboxes/sessions/" + url.PathEscape(sb.SessionID) + "/stop"
		if err := c.doJSON(ctx, http.MethodPost, path, nil, nil, nil); err != nil && !IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop session: %w", err))
		}
	}
	if err := c.deleteSandbox(ctx, sb.Name); err != nil && !IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete sandbox: %w", err))
	}
	return errors.Join(errs...)
}

func (c *Client) deleteSandbox(ctx context.Context, name string) error {
	q := url.Values{"projectId": {c.cfg.ProjectID}}
	return c.doJSON(ctx, http.MethodDelete, "/v2/sandboxes/"+url.PathEscape(name), q, nil, nil)
}

// doJSON sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, q url.Values, in, out any) error {
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

// do sends an authenticated request and returns the response if it is 2xx.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
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
	resp, err := c.http.Do(req)
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
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return &APIError{StatusCode: resp.StatusCode, Message: parsed.Error.Message}
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
