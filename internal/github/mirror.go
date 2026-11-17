package github

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/riggleg/mosso/internal/vercel"
)

const (
	// MaxPublishBytes caps the Git bundle of new commits one publish
	// downloads from the sandbox.
	MaxPublishBytes = 16 << 20
	// maxSyncBytes caps the Git bundle one sync uploads to the sandbox.
	maxSyncBytes = 64 << 20
	// maxScanBytes caps the uncompressed size of the objects one publish
	// checks for credentials.
	maxScanBytes = 512 << 20
	// uploadArgBytes and uploadArgs size the base64 chunks that carry a
	// bundle into the sandbox as command arguments: Linux limits one
	// argument to 128 KiB and all of them to about 2 MiB.
	uploadArgBytes = 96 << 10
	uploadArgs     = 8
)

// ErrNoChanges means the working branch has no changes against its base.
var ErrNoChanges = errors.New("no changes to publish: the working branch has no commits beyond its base")

// ErrBranchMoved means the working branch on GitHub has commits the sandbox
// does not, so publishing would discard them.
var ErrBranchMoved = errors.New("the working branch on GitHub has commits that are not in the workspace (pushed from somewhere else?), so Mosso will not overwrite it")

// Mirror carries one workspace's authenticated Git operations, fetch and
// push, between GitHub and the workspace's sandbox. Git runs on the Mosso
// server, in a temporary bare repository, with the token; commits move
// between that repository and the sandbox as Git bundles through the Vercel
// Sandbox API. The token therefore never enters the sandbox, where Claude
// and repository code run.
//
// The Mirror keeps the sandbox's refs/remotes/origin/* current for the
// branches it tracks: the base branch, the working branch, and the extra
// branches added with Fetch or Push (other base branches, the agent's other
// head branches). Mosso only ever pushes the workspace's own branches: the
// working branch, and branches named after it (see OwnBranch). Publish may
// replace the working branch with --force-with-lease after a rebase; Push,
// which the agent asks for, only ever fast-forwards. Methods are safe for
// concurrent use and run one at a time.
type Mirror struct {
	vc   *vercel.Client
	opts MirrorOptions

	// session is the sandbox session commands run in. It has its own
	// lock so pointing the Mirror at a resumed session never waits for a
	// Git operation in progress.
	sessMu  sync.Mutex
	session string

	mu     sync.Mutex
	local  string            // bare repository on the server; "" until first use
	closed bool              // Close was called
	known  map[string]bool   // commits the sandbox is known to have
	extra  []string          // tracked branches besides the base and working branch
	seen   map[string]string // branch -> the head the sandbox's origin/<branch> was last set to
	pushed string            // the working branch head Mosso last pushed
}

// MaxExtraBranches bounds the branches a Mirror tracks besides the base and
// working branch, so syncing before each turn stays quick.
const MaxExtraBranches = 20

// MirrorOptions describes a workspace.
type MirrorOptions struct {
	Repo    string // owner/repo
	Base    string // base branch
	BaseSHA string // commit the checkout was cloned at
	Branch  string // working branch, mosso/*
	Dir     string // checkout in the sandbox
	// Pushed is the working branch head Mosso last pushed, when a saved
	// workspace is restored; empty for a new one.
	Pushed string
	// Extra are the other branches the Mirror tracks, when a saved
	// workspace is restored (see Fetch and Push).
	Extra []string
}

// Heads are the base and working branch heads on GitHub. Branch is "" while
// the working branch does not exist there.
type Heads struct {
	Base   string `json:"base"`
	Branch string `json:"branch,omitempty"`
}

// ownSuffix is what may follow "<working branch>-" in the name of another
// of the workspace's head branches.
var ownSuffix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,49}$`)

// OwnBranch reports whether branch belongs to the workspace whose working
// branch is working: it is the working branch itself, or the working branch
// followed by "-" and a short suffix (letters, digits, ".", "_", "-"), such
// as "mosso/0a1b2c3d4e5f-uat". These are the only branches Mosso pushes for
// the workspace. Working branches are random, so one workspace's branches
// never match another's.
func OwnBranch(working, branch string) bool {
	if branch == working {
		return ValidBranch(branch)
	}
	suffix, ok := strings.CutPrefix(branch, working+"-")
	return ok && ownSuffix.MatchString(suffix) && ValidBranch(branch)
}

// NewMirror returns the Mirror of a workspace whose checkout was just
// cloned. The server-side repository is created on first use; call Close
// to remove it.
func NewMirror(vc *vercel.Client, sandboxSession string, opts MirrorOptions) (*Mirror, error) {
	switch {
	case !ValidRepo(opts.Repo):
		return nil, errors.New("mirror: invalid repository")
	case !ValidBranch(opts.Base) || !ValidBranch(opts.Branch) || !strings.HasPrefix(opts.Branch, BranchPrefix):
		return nil, errors.New("mirror: invalid branch")
	case !shaPattern.MatchString(opts.BaseSHA):
		return nil, errors.New("mirror: invalid base commit")
	case !strings.HasPrefix(opts.Dir, "/"):
		return nil, errors.New("mirror: dir must be absolute")
	case opts.Pushed != "" && !shaPattern.MatchString(opts.Pushed):
		return nil, errors.New("mirror: invalid pushed commit")
	}
	m := &Mirror{
		vc: vc, session: sandboxSession, opts: opts,
		known:  map[string]bool{opts.BaseSHA: true},
		seen:   map[string]string{opts.Base: opts.BaseSHA},
		pushed: opts.Pushed,
	}
	m.opts.Extra = nil
	for _, b := range opts.Extra {
		if !ValidBranch(b) {
			return nil, errors.New("mirror: invalid extra branch")
		}
		if err := m.track(b); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// track adds branch to the tracked branches, if it is not one already.
func (m *Mirror) track(branch string) error {
	if slices.Contains(m.tracked(), branch) {
		return nil
	}
	if len(m.extra) >= MaxExtraBranches {
		return fmt.Errorf("Mosso keeps at most %d extra branches per workspace in sync, and this one has that many", MaxExtraBranches)
	}
	m.extra = append(m.extra, branch)
	return nil
}

// tracked returns every branch the Mirror keeps current: the base branch,
// the working branch, then the extra ones.
func (m *Mirror) tracked() []string {
	return append([]string{m.opts.Base, m.opts.Branch}, m.extra...)
}

// Branches returns the extra branches the Mirror tracks, to save with the
// workspace.
func (m *Mirror) Branches() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.extra)
}

// Options returns the workspace the Mirror was created for.
func (m *Mirror) Options() MirrorOptions { return m.opts }

// SetSandboxSession points the Mirror at a new session of the same sandbox,
// after the sandbox was resumed. The checkout is the same filesystem.
func (m *Mirror) SetSandboxSession(id string) {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	m.session = id
}

// sandboxSession returns the session commands run in.
func (m *Mirror) sandboxSession() string {
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	return m.session
}

// Pushed returns the working branch head Mosso last pushed, to save with
// the workspace.
func (m *Mirror) Pushed() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pushed
}

// Close removes the server-side repository. Later calls fail.
func (m *Mirror) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.local == "" {
		return nil
	}
	dir := m.local
	m.local = ""
	return os.RemoveAll(dir)
}

// Synced is the outcome of a sync: the sandbox's origin refs moved from
// Before to After.
type Synced struct {
	Before Heads `json:"before"`
	After  Heads `json:"after"`
}

// Changed reports whether the sync moved a ref.
func (s Synced) Changed() bool { return s.Before != s.After }

// Sync fetches the tracked branches from GitHub and sets the sandbox's
// refs/remotes/origin/<branch> to them, sending the commits the sandbox
// lacks. Claude can then rebase onto or merge them without credentials.
// Nothing else in the checkout changes. The result reports the base and
// working branches.
func (m *Mirror) Sync(ctx context.Context, tok Token) (Synced, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.headsLocked()
	heads, err := m.fetch(ctx, tok, m.tracked())
	if err != nil {
		return Synced{Before: before, After: before}, err
	}
	if err := m.sendRequired(ctx, heads); err != nil {
		return Synced{Before: before, After: before}, err
	}
	return Synced{Before: before, After: m.headsLocked()}, nil
}

// headsLocked returns the base and working branch heads the sandbox was
// last shown.
func (m *Mirror) headsLocked() Heads {
	return Heads{Base: m.seen[m.opts.Base], Branch: m.seen[m.opts.Branch]}
}

// Fetch fetches branches from GitHub into the sandbox's
// refs/remotes/origin/<branch>, and keeps them current there from then on
// (see Sync), along with every other tracked branch. It returns the
// branches' heads; a branch that does not exist on GitHub is "" and is not
// tracked.
func (m *Mirror) Fetch(ctx context.Context, tok Token, branches ...string) (map[string]string, error) {
	for _, b := range branches {
		switch {
		case !ValidBranch(b):
			return nil, fmt.Errorf("fetch: %q is not a branch name Mosso accepts", b)
		case strings.HasPrefix(b, BranchPrefix) && !OwnBranch(m.opts.Branch, b):
			return nil, fmt.Errorf("fetch: %s belongs to another Mosso workspace", b)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := m.tracked()
	for _, b := range branches {
		if !slices.Contains(names, b) {
			names = append(names, b)
		}
	}
	heads, err := m.fetch(ctx, tok, names)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var add []string
	for _, b := range branches {
		out[b] = heads[b]
		if heads[b] != "" && !slices.Contains(m.tracked(), b) {
			if len(m.extra)+len(add) >= MaxExtraBranches {
				return nil, fmt.Errorf("Mosso keeps at most %d extra branches per workspace in sync, and this one has that many", MaxExtraBranches)
			}
			add = append(add, b)
		}
	}
	// Tracked only once the sandbox has them, so a branch that cannot be
	// sent is not kept.
	if err := m.sendRequired(ctx, heads, branches...); err != nil {
		return nil, err
	}
	for _, b := range add {
		_ = m.track(b)
	}
	return out, nil
}

// PublishOptions describes the pull request to publish.
type PublishOptions struct {
	Title string // pull request title, and the message for uncommitted work
	Body  string
	Draft bool
	// Secrets must not appear in any published commit. The client's own
	// token is always checked.
	Secrets []string
}

// Published is the outcome of a publish.
type Published struct {
	PullRequest
	Branch string `json:"branch"`
	// Commit is the head of the working branch on GitHub.
	Commit string `json:"commit"`
	// Created is true when this call opened the pull request.
	Created bool `json:"created"`
	// Forced is true when the push replaced the branch's history (after a
	// rebase), with --force-with-lease.
	Forced bool `json:"forced,omitempty"`
}

// Publish commits any uncommitted work in the sandbox, pushes the sandbox's
// HEAD to the working branch on GitHub and opens a pull request into the
// base branch, or returns the one already open. Calling it again after more
// work updates the same pull request.
//
// A fast-forward is pushed as is. A rewritten history (Claude rebased onto a
// newer base) is pushed with --force-with-lease, but only if the branch on
// GitHub is still the commit Mosso last pushed; otherwise ErrBranchMoved.
// Nothing in the sandbox is lost if a publish fails.
func (m *Mirror) Publish(ctx context.Context, gh *Client, opts PublishOptions) (*Published, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return nil, errors.New("publish: title is required")
	}
	message := opts.Title
	if strings.TrimSpace(opts.Body) != "" {
		message += "\n\n" + opts.Body
	}
	tok := gh.Token()

	m.mu.Lock()
	defer m.mu.Unlock()
	heads, err := m.fetch(ctx, tok, m.tracked())
	if err != nil {
		return nil, err
	}
	if err := m.sendRequired(ctx, heads); err != nil {
		return nil, err
	}
	base, branch := heads[m.opts.Base], heads[m.opts.Branch]
	have := haveCommits(heads)
	head, _, err := m.receive(ctx, message, "HEAD", have)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}

	out := &Published{Branch: m.opts.Branch, Commit: head}
	if branch == "" {
		n, err := m.git(ctx, nil, "", "rev-list", "--count", head, "^"+base)
		if err != nil {
			return nil, fmt.Errorf("publish: %w", err)
		}
		if n == "0" {
			return nil, ErrNoChanges
		}
	}
	if head != branch {
		if branch != "" && !m.isAncestor(ctx, branch, head) {
			if branch != m.pushed {
				return nil, fmt.Errorf("%w. Mosso has fetched it as origin/%s in the sandbox: ask Claude to rebase onto or merge origin/%s, then publish again", ErrBranchMoved, m.opts.Branch, m.opts.Branch)
			}
			out.Forced = true
		}
		secrets := append([]string{tok.Reveal()}, opts.Secrets...)
		if err := m.checkSecrets(ctx, head, have, secrets); err != nil {
			return nil, err
		}
		if err := m.push(ctx, tok, m.opts.Branch, head, branch); err != nil {
			return nil, err
		}
		m.pushed = head
		heads[m.opts.Branch] = head
		_, _ = m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+m.opts.Branch, head)
		// Show the sandbox the published branch; it already has the commit.
		_ = m.send(ctx, heads)
	}

	pr, created, err := gh.OpenPullRequest(ctx, m.opts.Repo, NewPullRequest{
		Title: opts.Title, Body: opts.Body, Head: m.opts.Branch, Base: m.opts.Base, Draft: opts.Draft,
	})
	if err != nil {
		return nil, err
	}
	out.PullRequest, out.Created = *pr, created
	return out, nil
}

// haveCommits returns the branch heads that exist, for bundles and secret
// checks: commits GitHub already has.
func haveCommits(heads map[string]string) []string {
	var have []string
	for _, sha := range heads {
		if sha != "" && !slices.Contains(have, sha) {
			have = append(have, sha)
		}
	}
	slices.Sort(have)
	return have
}

// PushOptions describes a push the agent asked for.
type PushOptions struct {
	// Branch is the branch on GitHub to push to: the working branch, or
	// another of the workspace's own branches (see OwnBranch).
	Branch string
	// Source is what to push from the sandbox: "HEAD" (the default) or
	// a local branch name. Uncommitted changes are not pushed.
	Source string
	// Secrets must not appear in any pushed commit. The token is always
	// checked.
	Secrets []string
}

// Pushed is the outcome of a Push.
type Pushed struct {
	Branch string `json:"branch"`
	// Commit is the branch's head on GitHub after the push.
	Commit string `json:"commit"`
	// Previous is the branch's head on GitHub before, "" if it was new.
	Previous string `json:"previous,omitempty"`
	// Updated is false when GitHub already had exactly this commit.
	Updated bool `json:"updated"`
	// Dirty reports uncommitted changes in the sandbox's checkout, which
	// were not pushed.
	Dirty bool `json:"dirty,omitempty"`
}

// ErrNotFastForward means a push would replace commits on GitHub: Push
// never force-pushes.
var ErrNotFastForward = errors.New("the push is not a fast-forward")

// Push pushes a commit from the sandbox (HEAD or a local branch) to one of
// the workspace's own branches on GitHub, creating it if needed, and tracks
// that branch from then on. It never commits for the agent, never pushes
// to any other branch, and never rewrites history: if the branch on GitHub
// has commits the pushed one does not contain, it fails with
// ErrNotFastForward and leaves everything as it was. The same checks as
// Publish apply: the commits are fsck'd, refused if they contain a
// credential, and pushed with a lease.
func (m *Mirror) Push(ctx context.Context, tok Token, opts PushOptions) (*Pushed, error) {
	source := opts.Source
	if source == "" {
		source = "HEAD"
	}
	switch {
	case !OwnBranch(m.opts.Branch, opts.Branch):
		return nil, fmt.Errorf("push: Mosso only pushes this workspace's own branches: %s, or %s-<suffix> (suffix: letters, digits, '.', '_' or '-'), not %q", m.opts.Branch, m.opts.Branch, opts.Branch)
	case source != "HEAD" && !ValidBranch(source):
		return nil, fmt.Errorf("push: source must be HEAD or a local branch name, not %q", source)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := m.tracked()
	if !slices.Contains(names, opts.Branch) {
		if len(m.extra) >= MaxExtraBranches {
			return nil, fmt.Errorf("push: Mosso keeps at most %d extra branches per workspace in sync, and this one has that many: push to one of its existing branches", MaxExtraBranches)
		}
		names = append(names, opts.Branch)
	}
	heads, err := m.fetch(ctx, tok, names)
	if err != nil {
		return nil, err
	}
	if err := m.sendRequired(ctx, heads, opts.Branch); err != nil {
		return nil, err
	}
	have := haveCommits(heads)
	head, dirty, err := m.receive(ctx, "", source, have)
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	remote := heads[opts.Branch]
	out := &Pushed{Branch: opts.Branch, Commit: head, Previous: remote, Dirty: dirty}
	if head == remote {
		_ = m.track(opts.Branch)
		return out, nil
	}
	if remote != "" && !m.isAncestor(ctx, remote, head) {
		hint := ""
		if opts.Branch == m.opts.Branch {
			hint = ", or ask the user to click Update PR in Mosso, which may replace a branch Mosso itself pushed"
		}
		return nil, fmt.Errorf("push: %w: %s on GitHub (%s) has commits that %s (%s) does not contain, and Mosso does not force-push for agents. Mosso has fetched it as origin/%s in the sandbox: rebase onto or merge it, then push again; or push to a new branch (%s-<suffix>)%s",
			ErrNotFastForward, opts.Branch, short(remote), source, short(head), opts.Branch, m.opts.Branch, hint)
	}
	secrets := append([]string{tok.Reveal()}, opts.Secrets...)
	if err := m.checkSecrets(ctx, head, have, secrets); err != nil {
		return nil, err
	}
	if err := m.push(ctx, tok, opts.Branch, head, remote); err != nil {
		return nil, err
	}
	if opts.Branch == m.opts.Branch {
		m.pushed = head
	}
	out.Updated = true
	_ = m.track(opts.Branch)
	heads[opts.Branch] = head
	_, _ = m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+opts.Branch, head)
	// Show the sandbox the pushed branch; it already has the commit.
	_ = m.send(ctx, heads)
	return out, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fetch brings branches from GitHub into the server's repository, as
// refs/remotes/origin/*, and returns their heads: "" for a branch that does
// not exist on GitHub (its ref is removed). The base branch must exist.
func (m *Mirror) fetch(ctx context.Context, tok Token, names []string) (map[string]string, error) {
	if err := m.init(ctx); err != nil {
		return nil, err
	}
	args := []string{"ls-remote", "--", repoURL(m.opts.Repo)}
	heads := map[string]string{}
	for _, b := range names {
		args = append(args, "refs/heads/"+b)
		heads[b] = ""
	}
	out, err := m.git(ctx, nil, tok, args...)
	if err != nil {
		return nil, gitHubError("fetch from GitHub", m.opts.Repo, err)
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, _ := strings.Cut(line, "\t")
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if _, wanted := heads[name]; ok && wanted && shaPattern.MatchString(sha) {
			heads[name] = sha
		}
	}
	if base, ok := heads[m.opts.Base]; ok && base == "" {
		return nil, fmt.Errorf("fetch from GitHub: the base branch %s no longer exists in %s", m.opts.Base, m.opts.Repo)
	}
	var refspecs []string
	for _, b := range names {
		if h := heads[b]; h != "" && !m.hasCommit(ctx, h) {
			refspecs = append(refspecs, "+refs/heads/"+b+":refs/remotes/origin/"+b)
		}
	}
	if len(refspecs) > 0 {
		args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--", repoURL(m.opts.Repo)}, refspecs...)
		if _, err := m.git(ctx, nil, tok, args...); err != nil {
			return nil, gitHubError("fetch from GitHub", m.opts.Repo, err)
		}
		// If a branch was force-pushed between ls-remote and fetch, the
		// head ls-remote reported may not have arrived.
		for _, h := range heads {
			if h != "" && !m.hasCommit(ctx, h) {
				return nil, errors.New("fetch from GitHub: the branch changed while fetching; try again")
			}
		}
	}
	// Record the heads so later bundles and secret checks can use them.
	for _, b := range names {
		if h := heads[b]; h != "" {
			if _, err := m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+b, h); err != nil {
				return nil, err
			}
		} else {
			_, _ = m.git(ctx, nil, "", "update-ref", "-d", "refs/remotes/origin/"+b)
		}
	}
	return heads, nil
}

// applyScript fetches the uploaded bundle, if any, then points the
// sandbox's origin refs at the given commits ("-" deletes a ref). Hooks are
// disabled: they are not Mosso's to run.
const applyScript = `set -euo pipefail
b64=$1; shift
if [ -n "$b64" ]; then
  trap 'rm -f "$b64" "$b64.bundle"' EXIT
  base64 -d < "$b64" > "$b64.bundle"
  # Only refs with new commits are in the bundle.
  refs=$(git bundle list-heads "$b64.bundle" | cut -d' ' -f2)
  git -c core.hooksPath=/dev/null fetch --quiet --no-tags --no-write-fetch-head "$b64.bundle" $refs >&2
fi
while [ $# -gt 0 ]; do
  ref="refs/remotes/origin/$1" sha=$2; shift 2
  if [ "$sha" = - ]; then
    git update-ref -d "$ref" 2>/dev/null || true
  else
    git update-ref "$ref" "$sha"
  fi
done`

// uploadScript appends base64 chunks to a file.
const uploadScript = `set -euo pipefail
f=$1; shift
printf '%s' "$@" >> "$f"`

// sendRequired sends heads to the sandbox like send. If that fails, it
// tries again with only the base branch, the working branch and the
// required ones, so an extra branch that cannot be sent (one that grew past
// the size limit, say) does not stop syncs, publishes and pushes; its
// origin ref in the sandbox is then left as it was.
func (m *Mirror) sendRequired(ctx context.Context, heads map[string]string, required ...string) error {
	err := m.send(ctx, heads)
	if err == nil || ctx.Err() != nil {
		return err
	}
	keep := append([]string{m.opts.Base, m.opts.Branch}, required...)
	core := map[string]string{}
	for b, h := range heads {
		if slices.Contains(keep, b) {
			core[b] = h
		}
	}
	if len(core) == len(heads) {
		return err
	}
	return m.send(ctx, core)
}

// send points the sandbox's origin refs at heads ("" removes one), first
// uploading a bundle of the commits the sandbox does not have yet.
func (m *Mirror) send(ctx context.Context, heads map[string]string) error {
	err := m.sendWith(ctx, heads, true)
	if err != nil && strings.Contains(err.Error(), "prerequisite") {
		// The sandbox lost a commit Mosso thought it had (Claude may
		// have rewritten history and pruned). Send everything.
		m.known = map[string]bool{}
		err = m.sendWith(ctx, heads, false)
	}
	if err != nil {
		return fmt.Errorf("update the sandbox's origin branches: %w", err)
	}
	for b, h := range heads {
		if h != "" {
			m.known[h] = true
			m.seen[b] = h
		} else {
			delete(m.seen, b)
		}
	}
	return nil
}

func (m *Mirror) sendWith(ctx context.Context, heads map[string]string, incremental bool) error {
	names := make([]string, 0, len(heads))
	for b := range heads {
		names = append(names, b)
	}
	slices.Sort(names)
	var refs []string
	for _, b := range names {
		if heads[b] != "" {
			refs = append(refs, "refs/remotes/origin/"+b)
		}
	}
	var exclude []string
	if incremental {
		for sha := range m.known {
			if m.hasCommit(ctx, sha) {
				exclude = append(exclude, "^"+sha)
			}
		}
	}
	var bundle []byte
	if n, err := m.git(ctx, nil, "", append(append([]string{"rev-list", "--count"}, refs...), exclude...)...); err != nil {
		return err
	} else if n != "0" {
		f := filepath.Join(m.local, "sync.bundle")
		defer os.Remove(f)
		args := append(append([]string{"bundle", "create", "--quiet", f}, refs...), exclude...)
		if _, err := m.git(ctx, nil, "", args...); err != nil {
			return err
		}
		if bundle, err = os.ReadFile(f); err != nil {
			return err
		}
		if len(bundle) > maxSyncBytes {
			return fmt.Errorf("%d bytes of new commits, more than the %d-byte limit", len(bundle), maxSyncBytes)
		}
	}

	path := ""
	if bundle != nil {
		path = "/tmp/mosso-sync-" + randomHex()
		if err := m.upload(ctx, path, base64.StdEncoding.EncodeToString(bundle)); err != nil {
			return err
		}
	}
	args := []string{"-c", applyScript, "apply", path}
	for _, b := range names {
		sha := heads[b]
		if sha == "" {
			sha = "-"
		}
		args = append(args, b, sha)
	}
	res, err := m.vc.Run(ctx, m.sandboxSession(), vercel.Command{
		Name: "bash",
		Args: args,
		Cwd:  m.opts.Dir,
		// send matches Git's English "prerequisite" message.
		Env: map[string]string{"LC_ALL": "C"},
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	return nil
}

// upload writes data to path in the sandbox, in command-argument chunks.
func (m *Mirror) upload(ctx context.Context, path, data string) error {
	for first := true; first || data != ""; first = false {
		args := []string{"-c", uploadScript, "upload", path}
		for i := 0; i < uploadArgs && data != ""; i++ {
			n := min(uploadArgBytes, len(data))
			args = append(args, data[:n])
			data = data[n:]
		}
		res, err := m.vc.Run(ctx, m.sandboxSession(), vercel.Command{Name: "bash", Args: args})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("upload: exit code %d: %s", res.ExitCode, tail(res.Stderr))
		}
	}
	return nil
}

// bundleScript refuses to run in the middle of a rebase or merge; with
// commit=1, commits any uncommitted work (hooks and signing disabled: they
// are not Mosso's to run); prints the commit of src (HEAD or a local
// branch) and whether the checkout has uncommitted changes, then the base64
// of a bundle of the commits that are not in the given commits, if there
// are any.
const bundleScript = `set -euo pipefail
msg=$1 max=$2 src=$3 commit=$4; shift 4
gitdir=$(git rev-parse --git-dir)
for f in rebase-merge rebase-apply MERGE_HEAD CHERRY_PICK_HEAD REVERT_HEAD; do
  if [ -e "$gitdir/$f" ]; then
    echo "a rebase, merge, cherry-pick or revert is in progress in the sandbox: finish it (or abort it), then try again" >&2
    exit 3
  fi
done
if [ -n "$(git diff --name-only --diff-filter=U)" ]; then
  echo "the sandbox has unresolved merge conflicts: resolve them, then try again" >&2
  exit 3
fi
if [ "$commit" = 1 ]; then
  git add -A
  if ! git diff --cached --quiet; then
    git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit --quiet --no-verify -m "$msg"
  fi
fi
ref=HEAD
if [ "$src" != HEAD ]; then ref="refs/heads/$src"; fi
if ! head=$(git rev-parse --verify --quiet "$ref^{commit}"); then
  echo "there is no local branch $src in the sandbox's checkout" >&2
  exit 3
fi
state=clean
if [ -n "$(git status --porcelain)" ]; then state=dirty; fi
echo "$head $state"
not=()
for c in "$@"; do
  if git cat-file -e "$c^{commit}" 2>/dev/null; then not+=("^$c"); fi
done
if [ "$(git rev-list --count "$head" ${not[@]+"${not[@]}"})" -gt 0 ]; then
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  git bundle create --quiet "$tmp" "$ref" ${not[@]+"${not[@]}"} >&2
  size=$(($(wc -c < "$tmp")))
  if [ "$size" -gt "$max" ]; then
    echo "the new commits are $size bytes as a bundle, more than the $max-byte limit" >&2
    exit 3
  fi
  base64 < "$tmp" | tr -d '\n'
  echo
fi`

// receive brings the commit of source (HEAD or a local branch) and its
// history from the sandbox into the server's repository, and returns it
// and whether the checkout has uncommitted changes. With a message, it
// first commits any uncommitted work in the sandbox with that message.
func (m *Mirror) receive(ctx context.Context, message, source string, have []string) (string, bool, error) {
	commit := "0"
	if message != "" {
		commit = "1"
	}
	res, err := m.vc.Run(ctx, m.sandboxSession(), vercel.Command{
		Name:           "bash",
		Args:           append([]string{"-c", bundleScript, "bundle", message, strconv.Itoa(MaxPublishBytes), source, commit}, have...),
		Cwd:            m.opts.Dir,
		MaxOutputBytes: MaxPublishBytes/3*4 + 1<<20,
	})
	if err != nil {
		return "", false, fmt.Errorf("read the sandbox's commits: %w", err)
	}
	if res.ExitCode == 3 {
		return "", false, errors.New(tail(res.Stderr))
	}
	if res.ExitCode != 0 {
		return "", false, fmt.Errorf("read the sandbox's commits: exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	first, data, _ := strings.Cut(strings.TrimSpace(res.Stdout), "\n")
	head, state, _ := strings.Cut(first, " ")
	if !shaPattern.MatchString(head) || (state != "clean" && state != "dirty") {
		return "", false, fmt.Errorf("read the sandbox's commits: unexpected output %q", tail(first))
	}
	dirty := state == "dirty"
	m.known[head] = true
	if data == "" {
		if !m.hasCommit(ctx, head) {
			return "", false, errors.New("the sandbox's commit is not one Mosso knows; try again")
		}
		return head, dirty, nil
	}
	bundle, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil {
		return "", false, fmt.Errorf("read the sandbox's commits: %w", err)
	}
	f := filepath.Join(m.local, "publish.bundle")
	if err := os.WriteFile(f, bundle, 0o600); err != nil {
		return "", false, err
	}
	defer os.Remove(f)
	ref := "HEAD"
	if source != "HEAD" {
		ref = "refs/heads/" + source
	}
	// fsckObjects: the bundle comes from the sandbox, so check every
	// object before the server's Git uses it.
	if _, err := m.git(ctx, nil, "", "-c", "fetch.fsckObjects=true", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		f, "+"+ref+":refs/mosso/head"); err != nil {
		return "", false, fmt.Errorf("read the sandbox's commits: %w", err)
	}
	if got, err := m.git(ctx, nil, "", "rev-parse", "refs/mosso/head"); err != nil || got != head {
		return "", false, errors.New("read the sandbox's commits: the bundle does not match the sandbox's commit")
	}
	return head, dirty, nil
}

// checkSecrets refuses to push if any new object (file, tree or commit
// message, in any of the new commits) contains a secret.
func (m *Mirror) checkSecrets(ctx context.Context, head string, have, secrets []string) error {
	args := []string{"rev-list", "--objects", head}
	for _, h := range have {
		args = append(args, "^"+h)
	}
	list, err := m.git(ctx, nil, "", args...)
	if err != nil {
		return fmt.Errorf("publish: list new objects: %w", err)
	}
	var shas, names []string
	for _, line := range strings.Split(list, "\n") {
		sha, name, _ := strings.Cut(line, " ")
		if shaPattern.MatchString(sha) {
			shas, names = append(shas, sha), append(names, name)
		}
	}
	if len(shas) == 0 {
		return nil
	}
	cmd := m.command(ctx, "", "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(shas, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Wait() }()
	defer stdout.Close()
	r := bufio.NewReader(stdout)
	total := 0
	for i := range shas {
		header, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("publish: read new objects: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return fmt.Errorf("publish: read new objects: unexpected %q", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return fmt.Errorf("publish: read new objects: unexpected %q", strings.TrimSpace(header))
		}
		if total += size; total > maxScanBytes {
			return fmt.Errorf("publish: the new commits hold more than %d bytes of content, more than Mosso publishes at once", maxScanBytes)
		}
		content := make([]byte, size+1) // plus the trailing newline
		if _, err := io.ReadFull(r, content); err != nil {
			return fmt.Errorf("publish: read new objects: %w", err)
		}
		for _, s := range secrets {
			if len(s) >= 8 && bytes.Contains(content, []byte(s)) {
				what := names[i]
				if what == "" {
					what = "a " + fields[1] + " (" + shas[i][:12] + ")"
				}
				return fmt.Errorf("publish: refusing to push %s: it contains one of Mosso's credentials; remove it from every commit, and revoke and rotate that credential if it may have left the sandbox", what)
			}
		}
	}
	return nil
}

// push pushes head to branch, with a lease on its current head (expect, ""
// if it must not exist yet): GitHub rejects the push if the branch moved
// since Mosso fetched it.
func (m *Mirror) push(ctx context.Context, tok Token, branch, head, expect string) error {
	ref := "refs/heads/" + branch
	_, err := m.git(ctx, nil, tok, "push", "--quiet", "--no-verify",
		"--force-with-lease="+ref+":"+expect, "--", repoURL(m.opts.Repo), head+":"+ref)
	if err != nil {
		return gitHubError("push to GitHub", m.opts.Repo, err)
	}
	return nil
}

// gitHubError turns Git's error output from talking to GitHub into an
// actionable message. Git's own words are kept at the end.
func gitHubError(op, repo string, err error) error {
	msg := err.Error()
	lower := strings.ToLower(msg)
	var hint string
	switch {
	case strings.Contains(lower, "workflow") && strings.Contains(lower, "refusing to allow"):
		hint = "GitHub refused the push because it changes files in .github/workflows, and Mosso's token deliberately has no Workflows permission. Ask Claude to drop those changes, or push them yourself"
	case strings.Contains(lower, "stale info") || strings.Contains(lower, "fetch first") || strings.Contains(lower, "non-fast-forward"):
		hint = "the branch changed on GitHub while Mosso was pushing; try again"
	case strings.Contains(lower, "invalid username or token") || strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "returned error: 401") || strings.Contains(lower, "could not read username"):
		hint = hintExpired
	case strings.Contains(lower, "permission to") || strings.Contains(lower, "write access to repository not granted") ||
		strings.Contains(lower, "returned error: 403"):
		hint = hintPermissions
	case strings.Contains(lower, "repository not found") || strings.Contains(lower, "returned error: 404"):
		hint = "repository " + repo + " was not found, or the GitHub token cannot access it: check the token's Repository access"
	case strings.Contains(lower, "gh006") || strings.Contains(lower, "gh013") || strings.Contains(lower, "protected branch"):
		hint = "a branch protection rule or ruleset rejected the push to the workspace's branch"
	}
	if hint == "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: %s (%w)", op, hint, err)
}

// init creates the server-side bare repository.
func (m *Mirror) init(ctx context.Context) error {
	if m.closed {
		return errors.New("the workspace has been terminated")
	}
	if m.local != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "mosso-git-")
	if err != nil {
		return fmt.Errorf("create the server's Git repository: %w", err)
	}
	m.local = dir
	if _, err := m.git(ctx, nil, "", "init", "--quiet", "--bare"); err != nil {
		m.local = ""
		_ = os.RemoveAll(dir)
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("git is not installed on the Mosso server; install it (on macOS: xcode-select --install)")
		}
		return fmt.Errorf("create the server's Git repository: %w", err)
	}
	return nil
}

func (m *Mirror) hasCommit(ctx context.Context, sha string) bool {
	_, err := m.git(ctx, nil, "", "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (m *Mirror) isAncestor(ctx context.Context, a, b string) bool {
	_, err := m.git(ctx, nil, "", "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// command prepares git in the server-side repository, isolated from the
// user's Git configuration (no system or global config, so no
// url.insteadOf, credential helpers or hooks of theirs) and never prompting.
// With a token, the token is in the environment for credentialHelper only.
func (m *Mirror) command(ctx context.Context, tok Token, args ...string) *exec.Cmd {
	full := append([]string{
		"--git-dir=" + m.local,
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=",
		"-c", "credential.helper=" + credentialHelper,
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = m.local
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + m.local,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	if tok != "" {
		cmd.Env = append(cmd.Env, envCloneToken+"="+tok.Reveal())
	}
	return cmd
}

// git runs a command and returns its trimmed stdout. Errors carry the end
// of stderr, with the token redacted.
func (m *Mirror) git(ctx context.Context, stdin io.Reader, tok Token, args ...string) (string, error) {
	cmd := m.command(ctx, tok, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", err
		}
		out := strings.TrimSpace(stderr.String() + "\n" + stdout.String() + "\n" + err.Error())
		return "", fmt.Errorf("git %s: %s", args[0], tail(tok.redact(out)))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func randomHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
