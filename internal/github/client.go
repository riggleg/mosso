// Package github connects Mosso workspaces to GitHub: it clones a repository
// into a Vercel Sandbox and publishes the sandbox's changes as a pull request.
//
// Only the initial clone of a private repository sends the token into the
// sandbox, before any agent or repository code has run there. Commits,
// branches and pull requests are created by the Mosso server through the
// GitHub REST API, so code running in the sandbox afterwards never sees the
// GitHub token.
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
	"time"
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

// branchHead returns the commit a branch points to, or "" if it does not
// exist.
func (c *Client) branchHead(ctx context.Context, repo, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+branch, nil, &ref); err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get branch %s: %w", branch, err)
	}
	return ref.Object.SHA, nil
}

func (c *Client) commitTree(ctx context.Context, repo, commit string) (string, error) {
	var out struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/commits/"+commit, nil, &out); err != nil {
		return "", fmt.Errorf("get commit %s: %w", commit, err)
	}
	return out.Tree.SHA, nil
}

func (c *Client) createBlob(ctx context.Context, repo string, base64Content string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]string{"content": base64Content, "encoding": "base64"}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/blobs", in, &out); err != nil {
		return "", fmt.Errorf("create blob: %w", err)
	}
	return out.SHA, nil
}

// treeEntry is one change in a create-tree request. A nil SHA with no
// Content deletes the path.
type treeEntry struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	SHA     *string `json:"sha"`
	Content *string `json:"content,omitempty"`
}

// MarshalJSON omits "sha" when Content is set: GitHub rejects entries that
// carry both, and reads an explicit null sha as a deletion.
func (e treeEntry) MarshalJSON() ([]byte, error) {
	m := map[string]any{"path": e.Path, "mode": e.Mode, "type": e.Type}
	if e.Content != nil {
		m["content"] = *e.Content
	} else {
		m["sha"] = e.SHA
	}
	return json.Marshal(m)
}

func (c *Client) createTree(ctx context.Context, repo, baseTree string, entries []treeEntry) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]any{"base_tree": baseTree, "tree": entries}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/trees", in, &out); err != nil {
		return "", fmt.Errorf("create tree: %w", err)
	}
	return out.SHA, nil
}

func (c *Client) createCommit(ctx context.Context, repo, message, tree, parent string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	in := map[string]any{"message": message, "tree": tree, "parents": []string{parent}}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/commits", in, &out); err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}
	return out.SHA, nil
}

func (c *Client) createBranch(ctx context.Context, repo, branch, sha string) error {
	in := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/git/refs", in, nil); err != nil {
		return fmt.Errorf("create branch %s: %w", branch, err)
	}
	return nil
}

// updateBranch fast-forwards a branch; GitHub rejects non-fast-forward moves.
func (c *Client) updateBranch(ctx context.Context, repo, branch, sha string) error {
	in := map[string]any{"sha": sha, "force": false}
	if err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/git/refs/heads/"+branch, in, nil); err != nil {
		return fmt.Errorf("update branch %s: %w", branch, err)
	}
	return nil
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
	if resp.StatusCode == http.StatusUnauthorized {
		msg += " (the GitHub token is invalid or expired; run `mosso auth github`)"
	}
	return &APIError{StatusCode: resp.StatusCode, Message: c.token.redact(msg)}
}
