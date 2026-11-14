// Package github connects Mosso workspaces to GitHub: it clones a repository
// into a Vercel Sandbox, keeps the sandbox's remote-tracking branches up to
// date, pushes the sandbox's commits and opens pull requests.
//
// Only the initial clone of a private repository sends the token into the
// sandbox, before any agent or repository code has run there. Later fetches
// and pushes run on the Mosso server (see Mirror), which exchanges commits
// with the sandbox as Git bundles, and pull requests are created through the
// GitHub REST API. Code running in the sandbox never sees the GitHub token.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Hints appended to GitHub errors so the user knows what to fix.
const (
	hintExpired     = "the GitHub token is invalid, expired or revoked: connect GitHub again in the Mosso web UI with a new token, or run `mosso auth github`"
	hintPermissions = "the GitHub token lacks access: give it this repository under Repository access, with Contents and Pull requests set to Read and write"
)

// DefaultBaseURL is the GitHub REST API endpoint.
const DefaultBaseURL = "https://api.github.com"

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	// branchPattern is a conservative subset of valid Git branch names.
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,199}$`)
)

// ValidRepo reports whether s looks like "owner/repo".
func ValidRepo(s string) bool {
	if !repoPattern.MatchString(s) {
		return false
	}
	name := s[strings.IndexByte(s, '/')+1:]
	return name != "." && name != ".." && !strings.HasSuffix(name, ".git")
}

// ValidBranch reports whether s is a branch name Mosso accepts. It is
// stricter than Git so names are safe in URLs and command arguments.
func ValidBranch(s string) bool {
	return branchPattern.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock") &&
		!strings.Contains(s, "/.") && !strings.Contains(s, "@{")
}

// Client calls the GitHub REST API with a personal access token.
type Client struct {
	token Token
	// BaseURL defaults to DefaultBaseURL; tests point it at a fake.
	BaseURL string
	HTTP    *http.Client

	mu         sync.Mutex
	expiration time.Time // from the last response, if GitHub sent it
}

// TokenExpiration returns when the token expires, as reported by GitHub on
// the last API response, or the zero time if unknown (or it never expires).
func (c *Client) TokenExpiration() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiration
}

// parseExpiration reads GitHub's github-authentication-token-expiration
// header, e.g. "2026-02-11 00:00:00 UTC".
func parseExpiration(v string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// NewClient returns a client that authenticates with token.
func NewClient(token Token) *Client {
	return &Client{token: token, BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: time.Minute}}
}

// Token returns the client's token, for redaction and secret checks.
func (c *Client) Token() Token { return c.token }

// APIError is a non-2xx response from GitHub. It never includes request
// headers.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github api: status %d", e.StatusCode)
	}
	return fmt.Sprintf("github api: status %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a 404 from GitHub. GitHub also answers
// 404 for private resources the token cannot see.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// User is the account a token belongs to.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
}

// CommitIdentity returns a Git author name and the account's GitHub noreply
// email, so commits are attributed to the user without exposing their
// address.
func (u *User) CommitIdentity() (name, email string) {
	name = u.Name
	if strings.TrimSpace(name) == "" {
		name = u.Login
	}
	return name, fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
}

// User returns the token's account.
func (c *Client) User(ctx context.Context) (*User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, fmt.Errorf("get authenticated user: %w", err)
	}
	if u.Login == "" || u.ID == 0 {
		return nil, errors.New("get authenticated user: response is missing login or id")
	}
	return &u, nil
}

// Repository is the subset of a GitHub repository Mosso uses.
type Repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// Repo returns repository metadata. repo is "owner/repo".
func (c *Client) Repo(ctx context.Context, repo string) (*Repository, error) {
	var r Repository
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("repository %s not found, or the GitHub token cannot access it: %w", repo, err)
		}
		return nil, fmt.Errorf("get repository %s: %w", repo, err)
	}
	return &r, nil
}

// Repos returns up to 100 repositories the token can access, most recently
// pushed first.
func (c *Client) Repos(ctx context.Context) ([]Repository, error) {
	repos := []Repository{}
	if err := c.do(ctx, http.MethodGet, "/user/repos?per_page=100&sort=pushed", nil, &repos); err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return repos, nil
}

// Branches returns the names of up to 100 branches of repo ("owner/repo").
func (c *Client) Branches(ctx context.Context, repo string) ([]string, error) {
	var branches []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/branches?per_page=100", nil, &branches); err != nil {
		return nil, fmt.Errorf("list branches of %s: %w", repo, err)
	}
	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}
	return names, nil
}

// PullRequest is a created or existing pull request.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type apiPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// NewPullRequest describes a pull request to open.
type NewPullRequest struct {
	Title string
	Body  string
	Head  string // branch in the same repository
	Base  string
	Draft bool
}

// openPullRequest returns the open pull request from head into base, if any.
func (c *Client) openPullRequest(ctx context.Context, repo, head, base string) (*PullRequest, error) {
	owner := repo[:strings.IndexByte(repo, '/')]
	q := url.Values{"head": {owner + ":" + head}, "base": {base}, "state": {"open"}}
	var pulls []apiPull
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return nil, fmt.Errorf("list pull requests: %w", err)
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	return &PullRequest{Number: pulls[0].Number, URL: pulls[0].HTMLURL}, nil
}

func (c *Client) createPullRequest(ctx context.Context, repo string, pr NewPullRequest) (*PullRequest, error) {
	in := map[string]any{"title": pr.Title, "body": pr.Body, "head": pr.Head, "base": pr.Base, "draft": pr.Draft}
	var out apiPull
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", in, &out); err != nil {
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	return &PullRequest{Number: out.Number, URL: out.HTMLURL}, nil
}

// do sends an authenticated JSON request and decodes a 2xx JSON response
// into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "mosso")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error includes the URL but never headers.
		return err
	}
	defer resp.Body.Close()
	if v := resp.Header.Get("Github-Authentication-Token-Expiration"); v != "" {
		if t := parseExpiration(v); !t.IsZero() {
			c.mu.Lock()
			c.expiration = t
			c.mu.Unlock()
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.apiError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) apiError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &parsed)
	msg := parsed.Message
	for _, e := range parsed.Errors {
		detail := e.Message
		if detail == "" {
			detail = strings.TrimSpace(e.Field + " " + e.Code)
		}
		if detail != "" {
			msg += "; " + detail
		}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		msg += " (" + hintExpired + ")"
	case resp.StatusCode == http.StatusForbidden && strings.Contains(msg, "not accessible by"):
		msg += " (" + hintPermissions + ")"
	}
	return &APIError{StatusCode: resp.StatusCode, Message: c.token.redact(msg)}
}
