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
	"slices"
	"strings"
	"sync"
	"time"
)

// Hints appended to GitHub errors so the user knows what to fix.
const (
	hintExpired     = "the GitHub token is invalid, expired or revoked: connect GitHub again in the Mosso web UI with a new token, or run `mosso auth github`"
	hintPermissions = "the GitHub token lacks access: give it this repository under Repository access, with Contents and Pull requests set to Read and write (" + hintOwners + ")"
	// hintOwners explains the most surprising cause of missing access.
	hintOwners = "a fine-grained token only reaches the repositories of its resource owner, one account or organization; to work across several, connect a classic token with the repo scope"
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
	// scopes are a classic token's OAuth scopes from the last response;
	// scopesKnown is whether that response reported them (never for
	// fine-grained tokens).
	scopes      []string
	scopesKnown bool
}

// TokenExpiration returns when the token expires, as reported by GitHub on
// the last API response, or the zero time if unknown (or it never expires).
func (c *Client) TokenExpiration() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiration
}

// Scopes returns a classic token's OAuth scopes, as reported by GitHub on
// the last API response, and false if that response did not report them
// (GitHub never does for fine-grained tokens).
func (c *Client) Scopes() ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scopes, c.scopesKnown
}

// CheckScopes returns an error unless the client's token can do what Mosso
// needs, judging by the last API response; call it after a successful
// request such as User. A fine-grained token's permissions are not
// reported, so it passes. A classic token needs the repo scope, and fails
// closed: if GitHub did not report its scopes, the scope cannot be
// verified and it is refused.
func (c *Client) CheckScopes() error {
	if !c.token.Classic() {
		return nil
	}
	scopes, known := c.Scopes()
	switch {
	case !known:
		return errors.New("GitHub did not report this classic token's scopes, so Mosso cannot verify that it has the repo scope it needs; try again, or use a fine-grained token")
	case !slices.Contains(scopes, "repo"):
		return errors.New("this classic token lacks the repo scope, which Mosso needs to clone, push and open pull requests: generate one with the classic token link (repo is preselected)")
	}
	return nil
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
			return nil, fmt.Errorf("repository %s not found, or the GitHub token cannot access it (%s): %w", repo, hintOwners, err)
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

// BranchHead returns the commit branch points to in repo, or an error that
// IsNotFound recognizes if there is no such branch.
func (c *Client) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	if !ValidBranch(branch) {
		return "", fmt.Errorf("%q is not a branch name Mosso accepts", branch)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	// git/ref takes the ref with its slashes; ValidBranch keeps it safe in
	// the path.
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/heads/"+branch, nil, &ref); err != nil {
		if IsNotFound(err) {
			return "", fmt.Errorf("branch %s not found in %s: %w", branch, repo, err)
		}
		return "", fmt.Errorf("read branch %s: %w", branch, err)
	}
	if !shaPattern.MatchString(ref.Object.SHA) {
		return "", fmt.Errorf("read branch %s: unexpected answer", branch)
	}
	return ref.Object.SHA, nil
}

// PullRequest is a created or existing pull request.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// PullRequestInfo describes a pull request in more detail.
type PullRequestInfo struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	// State is "open" or "closed"; Merged says whether a closed pull
	// request was merged.
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	// Head and Base are branch names; HeadRepo is the repository the
	// head branch is in ("owner/repo"), which differs for forks.
	Head     string `json:"head"`
	HeadSHA  string `json:"head_sha"`
	HeadRepo string `json:"-"`
	Base     string `json:"base"`
	// Mergeable and MergeableState are GitHub's view of whether the pull
	// request can merge (only on GetPullRequest; nil while GitHub is
	// still computing it).
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	nodeID         string
}

type apiPull struct {
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	NodeID         string     `json:"node_id"`
	Title          string     `json:"title"`
	Body           *string    `json:"body"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p apiPull) info() *PullRequestInfo {
	out := &PullRequestInfo{
		Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: p.State, Draft: p.Draft,
		Merged: p.Merged || p.MergedAt != nil, Head: p.Head.Ref, HeadSHA: p.Head.SHA, Base: p.Base.Ref,
		Mergeable: p.Mergeable, MergeableState: p.MergeableState, nodeID: p.NodeID,
	}
	if p.Body != nil {
		out.Body = *p.Body
	}
	if p.Head.Repo != nil {
		out.HeadRepo = p.Head.Repo.FullName
	}
	return out
}

// NewPullRequest describes a pull request to open.
type NewPullRequest struct {
	Title string
	Body  string
	Head  string // branch in the same repository
	Base  string
	Draft bool
}

// OpenPullRequest returns the open pull request from pr.Head into pr.Base
// in repo, or opens one; created says which. A pull request is never
// opened twice for the same head and base.
func (c *Client) OpenPullRequest(ctx context.Context, repo string, pr NewPullRequest) (*PullRequest, bool, error) {
	open, err := c.PullRequests(ctx, repo, pr.Head, "open")
	if err != nil {
		return nil, false, err
	}
	for _, p := range open {
		if p.Base == pr.Base {
			return &PullRequest{Number: p.Number, URL: p.URL}, false, nil
		}
	}
	if strings.TrimSpace(pr.Body) == "" {
		pr.Body = "Created with Mosso."
	}
	in := map[string]any{"title": pr.Title, "body": pr.Body, "head": pr.Head, "base": pr.Base, "draft": pr.Draft}
	var out apiPull
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", in, &out); err != nil {
		return nil, false, fmt.Errorf("create pull request: %w", err)
	}
	return &PullRequest{Number: out.Number, URL: out.HTMLURL}, true, nil
}

// PullRequests returns up to 100 pull requests from the branch head of
// repo itself (not forks), newest first. state is "open", "closed" or
// "all".
func (c *Client) PullRequests(ctx context.Context, repo, head, state string) ([]PullRequestInfo, error) {
	owner := repo[:strings.IndexByte(repo, '/')]
	q := url.Values{"head": {owner + ":" + head}, "state": {state}, "per_page": {"100"}}
	var pulls []apiPull
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return nil, fmt.Errorf("list pull requests: %w", err)
	}
	out := make([]PullRequestInfo, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, *p.info())
	}
	return out, nil
}

// GetPullRequest returns pull request number of repo.
func (c *Client) GetPullRequest(ctx context.Context, repo string, number int) (*PullRequestInfo, error) {
	var p apiPull
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &p); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("pull request #%d not found in %s: %w", number, repo, err)
		}
		return nil, fmt.Errorf("get pull request #%d: %w", number, err)
	}
	return p.info(), nil
}

// PullRequestUpdate changes the fields that are not nil.
type PullRequestUpdate struct {
	Title *string
	Body  *string
	Draft *bool
}

// UpdatePullRequest changes a pull request's title, description or draft
// status, and returns it as it is then. GitHub's REST API cannot change the
// draft status, so that goes through GraphQL.
func (c *Client) UpdatePullRequest(ctx context.Context, repo string, pr *PullRequestInfo, u PullRequestUpdate) (*PullRequestInfo, error) {
	in := map[string]any{}
	if u.Title != nil {
		in["title"] = *u.Title
	}
	if u.Body != nil {
		in["body"] = *u.Body
	}
	if len(in) > 0 {
		var p apiPull
		if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr.Number), in, &p); err != nil {
			return nil, fmt.Errorf("update pull request #%d: %w", pr.Number, err)
		}
	}
	if u.Draft != nil && *u.Draft != pr.Draft {
		mutation := "markPullRequestReadyForReview"
		if *u.Draft {
			mutation = "convertPullRequestToDraft"
		}
		q := "mutation($id: ID!) { " + mutation + "(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"
		if err := c.graphQL(ctx, q, map[string]any{"id": pr.nodeID}); err != nil {
			return nil, fmt.Errorf("change the draft status of pull request #%d: %w", pr.Number, err)
		}
	}
	return c.GetPullRequest(ctx, repo, pr.Number)
}

// graphQL runs a GraphQL mutation or query and fails on any GraphQL error.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any) error {
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": vars}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return &APIError{StatusCode: http.StatusOK, Message: c.token.redact(out.Errors[0].Message)}
	}
	return nil
}

// Check is one CI result for a commit: a check run (GitHub Actions and
// other apps) or a commit status.
type Check struct {
	Name string `json:"name"`
	// Status is "queued", "in_progress" or "completed" for check runs,
	// and "completed" or "pending" for commit statuses.
	Status string `json:"status"`
	// Conclusion, once completed: "success", "failure", "neutral",
	// "cancelled", "skipped", "timed_out", "action_required" (check runs)
	// or "error" (commit statuses).
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// Checks are the CI results for a commit.
type Checks struct {
	Commit string `json:"commit"`
	// State sums them up: "success", "failure", "pending", "none" when
	// nothing reported on the commit, or "unknown" when not every result
	// could be read and none of those read failed or is still running:
	// what could not be read may have failed.
	State string `json:"state"`
	// Complete says both check runs and commit statuses were read. Only
	// a complete result can show that every check passed.
	Complete bool    `json:"complete"`
	Checks   []Check `json:"checks"`
	// Missing says which kind of result could not be read, and why; Note
	// says what that means for State.
	Missing []string `json:"missing,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// CommitChecks returns the check runs and commit statuses of commit sha in
// repo. One kind failing to load (for example, a token without the Checks
// permission) is reported in Missing and makes the result incomplete: its
// State is then never "success" or "none", since the results that could not
// be read may have failed. Both failing is an error.
func (c *Client) CommitChecks(ctx context.Context, repo, sha string) (*Checks, error) {
	if !shaPattern.MatchString(sha) {
		return nil, errors.New("checks: invalid commit")
	}
	out := &Checks{Commit: sha, Checks: []Check{}}
	var runs struct {
		CheckRuns []struct {
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
			HTMLURL    string  `json:"html_url"`
			Output     struct {
				Title *string `json:"title"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	runsErr := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/check-runs?per_page=100", nil, &runs)
	if runsErr == nil {
		for _, r := range runs.CheckRuns {
			ch := Check{Name: r.Name, Status: r.Status, URL: r.HTMLURL}
			if r.Conclusion != nil {
				ch.Conclusion = *r.Conclusion
			}
			if r.Output.Title != nil {
				ch.Summary = *r.Output.Title
			}
			out.Checks = append(out.Checks, ch)
		}
	} else {
		out.Missing = append(out.Missing, "check runs: "+checksHint(runsErr, "Checks"))
	}
	var status struct {
		Statuses []struct {
			Context     string `json:"context"`
			State       string `json:"state"`
			TargetURL   string `json:"target_url"`
			Description string `json:"description"`
		} `json:"statuses"`
	}
	statusErr := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+sha+"/status?per_page=100", nil, &status)
	if statusErr == nil {
		for _, st := range status.Statuses {
			ch := Check{Name: st.Context, Status: "completed", Conclusion: st.State, URL: st.TargetURL, Summary: st.Description}
			if st.State == "pending" {
				ch.Status, ch.Conclusion = "pending", ""
			}
			out.Checks = append(out.Checks, ch)
		}
	} else {
		out.Missing = append(out.Missing, "commit statuses: "+checksHint(statusErr, "Commit statuses"))
	}
	if runsErr != nil && statusErr != nil {
		return nil, fmt.Errorf("read the checks of %s: %s; %s", sha[:12], out.Missing[0], out.Missing[1])
	}
	out.State = checksState(out.Checks)
	out.Complete = runsErr == nil && statusErr == nil
	if !out.Complete {
		if out.State == "success" || out.State == "none" {
			out.State = "unknown"
		}
		out.Note = "Not every CI result could be read (see missing), so this does not show that all checks passed. " +
			"A failure or pending check listed here is real; anything else may have failed. Tell the user if it matters."
	}
	return out, nil
}

// checksHint explains a failure to read CI results.
func checksHint(err error, permission string) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound) {
		return err.Error() + " (the GitHub token may lack the " + permission + " permission: give it " + permission + ": Read-only)"
	}
	return err.Error()
}

// checksState sums up CI results like GitHub's merge box: any failure
// fails, then anything unfinished is pending.
func checksState(checks []Check) string {
	if len(checks) == 0 {
		return "none"
	}
	pending := false
	for _, ch := range checks {
		switch ch.Conclusion {
		case "failure", "error", "cancelled", "timed_out", "action_required", "startup_failure":
			return "failure"
		}
		if ch.Status != "completed" {
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "success"
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
	// Present, possibly empty, for classic tokens only. Every response
	// replaces what the previous one reported, so a missing header is
	// never mistaken for scopes an earlier response had.
	v, scopesKnown := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]
	var scopes []string
	for _, s := range strings.Split(strings.Join(v, ","), ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	c.mu.Lock()
	c.scopes, c.scopesKnown = scopes, scopesKnown
	c.mu.Unlock()
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
