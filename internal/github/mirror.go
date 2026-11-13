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
// The working branch is the only ref Mosso ever pushes, and pushes use
// --force-with-lease so a rebased branch replaces only what Mosso itself
// published. Methods are safe for concurrent use and run one at a time.
type Mirror struct {
	vc      *vercel.Client
	session string
	opts    MirrorOptions

	mu     sync.Mutex
	local  string          // bare repository on the server; "" until first use
	closed bool            // Close was called
	known  map[string]bool // commits the sandbox is known to have
	seen   Heads           // the heads the sandbox's origin refs were last set to
	pushed string          // the working branch head Mosso last pushed
}

// MirrorOptions describes a workspace.
type MirrorOptions struct {
	Repo    string // owner/repo
	Base    string // base branch
	BaseSHA string // commit the checkout was cloned at
	Branch  string // working branch, mosso/*
	Dir     string // checkout in the sandbox
}

// Heads are the base and working branch heads on GitHub. Branch is "" while
// the working branch does not exist there.
type Heads struct {
	Base   string `json:"base"`
	Branch string `json:"branch,omitempty"`
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
	}
	return &Mirror{
		vc: vc, session: sandboxSession, opts: opts,
		known: map[string]bool{opts.BaseSHA: true},
		seen:  Heads{Base: opts.BaseSHA},
	}, nil
}

// Options returns the workspace the Mirror was created for.
func (m *Mirror) Options() MirrorOptions { return m.opts }

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

// Sync fetches the base and working branches from GitHub and sets the
// sandbox's refs/remotes/origin/<base> and origin/<branch> to them, sending
// the commits the sandbox lacks. Claude can then rebase onto or merge them
// without credentials. Nothing else in the checkout changes.
func (m *Mirror) Sync(ctx context.Context, tok Token) (Synced, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.seen
	heads, err := m.fetch(ctx, tok)
	if err != nil {
		return Synced{Before: before, After: before}, err
	}
	if err := m.send(ctx, heads); err != nil {
		return Synced{Before: before, After: before}, err
	}
	return Synced{Before: before, After: heads}, nil
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
	heads, err := m.fetch(ctx, tok)
	if err != nil {
		return nil, err
	}
	if err := m.send(ctx, heads); err != nil {
		return nil, err
	}
	have := []string{heads.Base}
	if heads.Branch != "" {
		have = append(have, heads.Branch)
	}
	head, err := m.receive(ctx, message, have)
	if err != nil {
		return nil, err
	}

	out := &Published{Branch: m.opts.Branch, Commit: head}
	if heads.Branch == "" {
		n, err := m.git(ctx, nil, "", "rev-list", "--count", head, "^"+heads.Base)
		if err != nil {
			return nil, fmt.Errorf("publish: %w", err)
		}
		if n == "0" {
			return nil, ErrNoChanges
		}
	}
	if head != heads.Branch {
		if heads.Branch != "" && !m.isAncestor(ctx, heads.Branch, head) {
			if heads.Branch != m.pushed {
				return nil, fmt.Errorf("%w. Mosso has fetched it as origin/%s in the sandbox: ask Claude to rebase onto or merge origin/%s, then publish again", ErrBranchMoved, m.opts.Branch, m.opts.Branch)
			}
			out.Forced = true
		}
		secrets := append([]string{tok.Reveal()}, opts.Secrets...)
		if err := m.checkSecrets(ctx, head, have, secrets); err != nil {
			return nil, err
		}
		if err := m.push(ctx, tok, head, heads.Branch); err != nil {
			return nil, err
		}
		m.pushed = head
		heads.Branch = head
		_, _ = m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+m.opts.Branch, head)
		// Show the sandbox the published branch; it already has the commit.
		_ = m.send(ctx, heads)
	}

	pr, err := gh.openPullRequest(ctx, m.opts.Repo, m.opts.Branch, m.opts.Base)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		body := opts.Body
		if strings.TrimSpace(body) == "" {
			body = "Created with Mosso."
		}
		pr, err = gh.createPullRequest(ctx, m.opts.Repo, NewPullRequest{
			Title: opts.Title, Body: body, Head: m.opts.Branch, Base: m.opts.Base, Draft: opts.Draft,
		})
		if err != nil {
			return nil, err
		}
		out.Created = true
	}
	out.PullRequest = *pr
	return out, nil
}

// fetch brings the base and working branches from GitHub into the server's
// repository, as refs/remotes/origin/*, and returns their heads.
func (m *Mirror) fetch(ctx context.Context, tok Token) (Heads, error) {
	if err := m.init(ctx); err != nil {
		return Heads{}, err
	}
	baseRef, branchRef := "refs/heads/"+m.opts.Base, "refs/heads/"+m.opts.Branch
	out, err := m.git(ctx, nil, tok, "ls-remote", "--", repoURL(m.opts.Repo), baseRef, branchRef)
	if err != nil {
		return Heads{}, gitHubError("fetch from GitHub", m.opts.Repo, err)
	}
	var heads Heads
	for _, line := range strings.Split(out, "\n") {
		sha, ref, _ := strings.Cut(line, "\t")
		switch {
		case !shaPattern.MatchString(sha):
		case ref == baseRef:
			heads.Base = sha
		case ref == branchRef:
			heads.Branch = sha
		}
	}
	if heads.Base == "" {
		return Heads{}, fmt.Errorf("fetch from GitHub: the base branch %s no longer exists in %s", m.opts.Base, m.opts.Repo)
	}
	refspecs := []string{}
	if !m.hasCommit(ctx, heads.Base) {
		refspecs = append(refspecs, "+"+baseRef+":refs/remotes/origin/"+m.opts.Base)
	}
	if heads.Branch != "" && !m.hasCommit(ctx, heads.Branch) {
		refspecs = append(refspecs, "+"+branchRef+":refs/remotes/origin/"+m.opts.Branch)
	}
	if len(refspecs) > 0 {
		args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--", repoURL(m.opts.Repo)}, refspecs...)
		if _, err := m.git(ctx, nil, tok, args...); err != nil {
			return Heads{}, gitHubError("fetch from GitHub", m.opts.Repo, err)
		}
		// If a branch was force-pushed between ls-remote and fetch, the
		// head ls-remote reported may not have arrived.
		for _, h := range []*string{&heads.Base, &heads.Branch} {
			if *h != "" && !m.hasCommit(ctx, *h) {
				return Heads{}, errors.New("fetch from GitHub: the branch changed while fetching; try again")
			}
		}
	}
	// Record the heads so later bundles and secret checks can use them.
	if _, err := m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+m.opts.Base, heads.Base); err != nil {
		return Heads{}, err
	}
	if heads.Branch != "" {
		if _, err := m.git(ctx, nil, "", "update-ref", "refs/remotes/origin/"+m.opts.Branch, heads.Branch); err != nil {
			return Heads{}, err
		}
	} else {
		_, _ = m.git(ctx, nil, "", "update-ref", "-d", "refs/remotes/origin/"+m.opts.Branch)
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

// send points the sandbox's origin refs at heads, first uploading a bundle
// of the commits the sandbox does not have yet.
func (m *Mirror) send(ctx context.Context, heads Heads) error {
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
	for _, h := range []string{heads.Base, heads.Branch} {
		if h != "" {
			m.known[h] = true
		}
	}
	m.seen = heads
	return nil
}

func (m *Mirror) sendWith(ctx context.Context, heads Heads, incremental bool) error {
	refs := []string{"refs/remotes/origin/" + m.opts.Base}
	if heads.Branch != "" {
		refs = append(refs, "refs/remotes/origin/"+m.opts.Branch)
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
	branchSHA := heads.Branch
	if branchSHA == "" {
		branchSHA = "-"
	}
	res, err := m.vc.Run(ctx, m.session, vercel.Command{
		Name: "bash",
		Args: []string{"-c", applyScript, "apply", path, m.opts.Base, heads.Base, m.opts.Branch, branchSHA},
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
		res, err := m.vc.Run(ctx, m.session, vercel.Command{Name: "bash", Args: args})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("upload: exit code %d: %s", res.ExitCode, tail(res.Stderr))
		}
	}
	return nil
}

// bundleScript refuses to run in the middle of a rebase or merge, commits
// any uncommitted work (hooks and signing disabled: they are not Mosso's to
// run), prints HEAD, then the base64 of a bundle of the commits that are not
// in the given commits, if there are any.
const bundleScript = `set -euo pipefail
msg=$1 max=$2; shift 2
gitdir=$(git rev-parse --git-dir)
for f in rebase-merge rebase-apply MERGE_HEAD CHERRY_PICK_HEAD REVERT_HEAD; do
  if [ -e "$gitdir/$f" ]; then
    echo "a rebase, merge, cherry-pick or revert is in progress in the sandbox; ask Claude to finish it (or abort it), then publish again" >&2
    exit 3
  fi
done
if [ -n "$(git diff --name-only --diff-filter=U)" ]; then
  echo "the sandbox has unresolved merge conflicts; ask Claude to resolve them, then publish again" >&2
  exit 3
fi
git add -A
if ! git diff --cached --quiet; then
  git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit --quiet --no-verify -m "$msg"
fi
head=$(git rev-parse --verify 'HEAD^{commit}')
echo "$head"
not=()
for c in "$@"; do
  if git cat-file -e "$c^{commit}" 2>/dev/null; then not+=("^$c"); fi
done
if [ "$(git rev-list --count HEAD ${not[@]+"${not[@]}"})" -gt 0 ]; then
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  git bundle create --quiet "$tmp" HEAD ${not[@]+"${not[@]}"} >&2
  size=$(($(wc -c < "$tmp")))
  if [ "$size" -gt "$max" ]; then
    echo "the new commits are $size bytes as a bundle, more than the $max-byte limit" >&2
    exit 3
  fi
  base64 < "$tmp" | tr -d '\n'
  echo
fi`

// receive commits in the sandbox and brings its new commits into the
// server's repository. It returns the sandbox's HEAD.
func (m *Mirror) receive(ctx context.Context, message string, have []string) (string, error) {
	res, err := m.vc.Run(ctx, m.session, vercel.Command{
		Name:           "bash",
		Args:           append([]string{"-c", bundleScript, "bundle", message, strconv.Itoa(MaxPublishBytes)}, have...),
		Cwd:            m.opts.Dir,
		MaxOutputBytes: MaxPublishBytes/3*4 + 1<<20,
	})
	if err != nil {
		return "", fmt.Errorf("commit in sandbox: %w", err)
	}
	if res.ExitCode == 3 {
		return "", fmt.Errorf("publish: %s", tail(res.Stderr))
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("commit in sandbox: exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	head, data, _ := strings.Cut(strings.TrimSpace(res.Stdout), "\n")
	if !shaPattern.MatchString(head) {
		return "", fmt.Errorf("commit in sandbox: unexpected output %q", tail(head))
	}
	m.known[head] = true
	if data == "" {
		if !m.hasCommit(ctx, head) {
			return "", errors.New("publish: the sandbox's HEAD is not a commit Mosso knows; try again")
		}
		return head, nil
	}
	bundle, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil {
		return "", fmt.Errorf("read commits from sandbox: %w", err)
	}
	f := filepath.Join(m.local, "publish.bundle")
	if err := os.WriteFile(f, bundle, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(f)
	// fsckObjects: the bundle comes from the sandbox, so check every
	// object before the server's Git uses it.
	if _, err := m.git(ctx, nil, "", "-c", "fetch.fsckObjects=true", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		f, "+HEAD:refs/mosso/head"); err != nil {
		return "", fmt.Errorf("read commits from sandbox: %w", err)
	}
	if got, err := m.git(ctx, nil, "", "rev-parse", "refs/mosso/head"); err != nil || got != head {
		return "", errors.New("read commits from sandbox: the bundle does not match the sandbox's HEAD")
	}
	return head, nil
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

// push pushes head to the working branch, with a lease on its current head
// (expect, "" if it must not exist yet): GitHub rejects the push if the
// branch moved since Mosso fetched it.
func (m *Mirror) push(ctx context.Context, tok Token, head, expect string) error {
	ref := "refs/heads/" + m.opts.Branch
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
		hint = "the branch changed on GitHub while Mosso was pushing; publish again"
	case strings.Contains(lower, "invalid username or token") || strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "returned error: 401") || strings.Contains(lower, "could not read username"):
		hint = hintExpired
	case strings.Contains(lower, "permission to") || strings.Contains(lower, "write access to repository not granted") ||
		strings.Contains(lower, "returned error: 403"):
		hint = hintPermissions
	case strings.Contains(lower, "repository not found") || strings.Contains(lower, "returned error: 404"):
		hint = "repository " + repo + " was not found, or the GitHub token cannot access it: check the token's Repository access"
	case strings.Contains(lower, "gh006") || strings.Contains(lower, "gh013") || strings.Contains(lower, "protected branch"):
		hint = "a branch protection rule or ruleset rejected the push to the working branch"
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
