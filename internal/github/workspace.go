package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/vercel"
)

const (
	// BranchPrefix starts every working branch Mosso creates.
	BranchPrefix = "mosso/"
	// MaxPublishBytes caps the total size of changed files in one publish.
	MaxPublishBytes = 16 << 20
	// maxChangedFiles caps the number of changed paths in one publish.
	maxChangedFiles = 2000
	// envCloneToken carries the token to the clone command only.
	envCloneToken = "MOSSO_GITHUB_TOKEN"
)

// gitBaseURL is where repositories are cloned from; tests replace it.
var gitBaseURL = "https://github.com"

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// NewBranchName returns a unique working branch name.
func NewBranchName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return BranchPrefix + hex.EncodeToString(b[:])
}

// cloneScript clones one branch and checks out the working branch.
// Parameters arrive as "$@", never interpolated into the script.
//
// The token, if any, is only in this command's environment. The credential
// helper is given on the command line (the empty value first clears any other
// helper) and reads the token from the environment when Git asks for it, so
// it is never written to .git/config, the remote URL, or the command line.
const cloneScript = `set -euo pipefail
dir=$1 url=$2 base=$3 branch=$4 name=$5 email=$6
if ! command -v git >/dev/null 2>&1; then sudo dnf install -y -q git >&2; fi
git -c credential.helper= \
  -c 'credential.helper=!f() { test "$1" = get && test -n "$MOSSO_GITHUB_TOKEN" || return 0; echo username=x-access-token; echo "password=$MOSSO_GITHUB_TOKEN"; }; f' \
  clone --quiet --single-branch --branch "$base" -- "$url" "$dir" >&2
cd "$dir"
git checkout --quiet -b "$branch"
git config user.name "$name"
git config user.email "$email"
git rev-parse HEAD`

// CloneOptions describes a repository checkout in a sandbox.
type CloneOptions struct {
	Repo   string // owner/repo
	Base   string // branch to clone and target with the pull request
	Branch string // new working branch
	Dir    string // absolute path of the checkout
	// AuthorName and AuthorEmail configure Git's identity in the checkout.
	AuthorName, AuthorEmail string
	// Token authenticates the clone. Leave it empty for public
	// repositories so the token never enters the sandbox.
	Token Token
}

// Clone clones opts.Base into opts.Dir, creates opts.Branch and configures
// the commit identity. It returns the base commit SHA.
func Clone(ctx context.Context, vc *vercel.Client, sandboxSession string, opts CloneOptions) (string, error) {
	switch {
	case !ValidRepo(opts.Repo):
		return "", errors.New("clone: invalid repository")
	case !ValidBranch(opts.Base) || !ValidBranch(opts.Branch):
		return "", errors.New("clone: invalid branch name")
	case !strings.HasPrefix(opts.Dir, "/"):
		return "", errors.New("clone: dir must be absolute")
	}
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if opts.Token != "" {
		env[envCloneToken] = opts.Token.Reveal()
	}
	res, err := vc.Run(ctx, sandboxSession, vercel.Command{
		Name: "bash",
		Args: []string{"-c", cloneScript, "clone",
			opts.Dir, gitBaseURL + "/" + opts.Repo + ".git", opts.Base, opts.Branch, opts.AuthorName, opts.AuthorEmail},
		Env: env,
	})
	if err != nil {
		return "", fmt.Errorf("clone %s: %w", opts.Repo, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("clone %s: exit code %d: %s", opts.Repo, res.ExitCode, tail(opts.Token.redact(res.Stderr)))
	}
	sha := strings.TrimSpace(res.Stdout)
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("clone %s: unexpected output %q", opts.Repo, tail(opts.Token.redact(sha)))
	}
	return sha, nil
}

// commitScript commits any uncommitted work (hooks and signing disabled:
// they are not Mosso's to run), then prints the base tree, the HEAD tree,
// and the base64 of `git diff-tree -z` between the base commit and HEAD.
const commitScript = `set -euo pipefail
base=$1 msg=$2
git add -A
if ! git diff --cached --quiet; then
  git -c core.hooksPath=/dev/null -c commit.gpgsign=false commit --quiet --no-verify -m "$msg"
fi
git rev-parse "$base^{tree}" "HEAD^{tree}"
git diff-tree -r -z --no-renames "$base" HEAD | base64 | tr -d '\n'
echo`

// readScript prints each blob given as an argument as one base64 line,
// after checking their total size against the limit in $1.
const readScript = `set -euo pipefail
max=$1; shift
total=0
while read -r n; do total=$((total + n)); done < <(printf '%s\n' "$@" | git cat-file --batch-check='%(objectsize)')
if [ "$total" -gt "$max" ]; then
  echo "changed files total $total bytes, more than the $max-byte limit" >&2
  exit 3
fi
for sha in "$@"; do git cat-file blob "$sha" | base64 | tr -d '\n'; echo; done`

// PublishOptions describes the pull request to publish.
type PublishOptions struct {
	Repo    string
	Base    string // pull request target branch
	BaseSHA string // commit the working branch started from
	Branch  string // working branch, created on GitHub if needed
	Dir     string // checkout in the sandbox
	Title   string
	Body    string
	Draft   bool
	// Secrets must not appear in any published file. The client's own
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
}

// ErrNoChanges means the working branch has no changes against its base.
var ErrNoChanges = errors.New("no changes to publish: the working branch is identical to its base")

// change is one path that differs between the base commit and HEAD.
type change struct {
	path    string
	oldMode string
	newMode string // "000000" for a deletion
	newSHA  string
	content []byte
}

// Publish commits the checkout's changes in the sandbox, recreates them on
// the working branch on GitHub through the API, and opens a pull request
// into the base branch (or returns the one already open). Calling it again
// after more work adds a commit to the branch and returns the same pull
// request.
//
// The GitHub token never enters the sandbox: Mosso reads the changed files
// out of the sandbox and creates the blobs, tree, commit and branch itself.
func Publish(ctx context.Context, vc *vercel.Client, sandboxSession string, gh *Client, opts PublishOptions) (*Published, error) {
	switch {
	case !ValidRepo(opts.Repo):
		return nil, errors.New("publish: invalid repository")
	case !ValidBranch(opts.Base) || !ValidBranch(opts.Branch) || !strings.HasPrefix(opts.Branch, BranchPrefix):
		return nil, errors.New("publish: invalid branch")
	case !shaPattern.MatchString(opts.BaseSHA):
		return nil, errors.New("publish: invalid base commit")
	case strings.TrimSpace(opts.Title) == "":
		return nil, errors.New("publish: title is required")
	}
	message := opts.Title
	if strings.TrimSpace(opts.Body) != "" {
		message += "\n\n" + opts.Body
	}

	baseTree, headTree, changes, err := commitChanges(ctx, vc, sandboxSession, opts.Dir, opts.BaseSHA, message)
	if err != nil {
		return nil, err
	}
	if err := readContents(ctx, vc, sandboxSession, opts.Dir, changes); err != nil {
		return nil, err
	}
	secrets := append([]string{gh.Token().Reveal()}, opts.Secrets...)
	for _, c := range changes {
		for _, s := range secrets {
			if len(s) >= 8 && bytes.Contains(c.content, []byte(s)) {
				return nil, fmt.Errorf("publish: refusing to push %s: it contains one of Mosso's credentials; revoke and rotate that credential if it may have left the sandbox", c.path)
			}
		}
	}

	head, err := gh.branchHead(ctx, opts.Repo, opts.Branch)
	if err != nil {
		return nil, err
	}
	// Nothing differs from the base and nothing was published before. (If
	// an earlier publish exists and the work was since reverted, fall
	// through so the branch is updated back to the base tree.)
	if len(changes) == 0 && head == "" {
		return nil, ErrNoChanges
	}
	parent, parentTree := opts.BaseSHA, baseTree
	if head != "" {
		if parentTree, err = gh.commitTree(ctx, opts.Repo, head); err != nil {
			return nil, err
		}
		parent = head
	}
	// Trees are content-addressed, so if GitHub's tree matches the
	// sandbox's HEAD tree the pushed files are exactly Claude's.
	if parentTree != headTree {
		tree := baseTree // no changes against the base: reuse its tree
		if len(changes) > 0 {
			tree, err = createTree(ctx, gh, opts.Repo, baseTree, changes, true)
		}
		if err == nil && tree != headTree {
			// Retry without inline text content in case GitHub
			// normalized something.
			tree, err = createTree(ctx, gh, opts.Repo, baseTree, changes, false)
		}
		if err != nil {
			return nil, err
		}
		if tree != headTree {
			return nil, fmt.Errorf("publish: GitHub tree %s does not match the sandbox tree %s", tree, headTree)
		}
		commit, err := gh.createCommit(ctx, opts.Repo, message, tree, parent)
		if err != nil {
			return nil, err
		}
		if head == "" {
			err = gh.createBranch(ctx, opts.Repo, opts.Branch, commit)
		} else {
			err = gh.updateBranch(ctx, opts.Repo, opts.Branch, commit)
		}
		if err != nil {
			return nil, err
		}
		head = commit
	}

	out := &Published{Branch: opts.Branch, Commit: head}
	pr, err := gh.openPullRequest(ctx, opts.Repo, opts.Branch, opts.Base)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		body := opts.Body
		if strings.TrimSpace(body) == "" {
			body = "Created with Mosso."
		}
		pr, err = gh.createPullRequest(ctx, opts.Repo, NewPullRequest{
			Title: opts.Title, Body: body, Head: opts.Branch, Base: opts.Base, Draft: opts.Draft,
		})
		if err != nil {
			return nil, err
		}
		out.Created = true
	}
	out.PullRequest = *pr
	return out, nil
}

// commitChanges commits in the sandbox and returns the base and HEAD trees
// and the paths that differ between them.
func commitChanges(ctx context.Context, vc *vercel.Client, session, dir, baseSHA, message string) (string, string, []change, error) {
	res, err := vc.Run(ctx, session, vercel.Command{
		Name: "bash", Args: []string{"-c", commitScript, "commit", baseSHA, message}, Cwd: dir,
		MaxOutputBytes: 8 << 20,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("commit in sandbox: %w", err)
	}
	if res.ExitCode != 0 {
		return "", "", nil, fmt.Errorf("commit in sandbox: exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
	if len(lines) < 2 || !shaPattern.MatchString(lines[0]) || !shaPattern.MatchString(lines[1]) {
		return "", "", nil, fmt.Errorf("commit in sandbox: unexpected output %q", tail(res.Stdout))
	}
	var raw []byte
	if len(lines) > 2 {
		if raw, err = base64.StdEncoding.DecodeString(lines[2]); err != nil {
			return "", "", nil, fmt.Errorf("commit in sandbox: decode diff: %w", err)
		}
	}
	changes, err := parseDiffTree(raw)
	if err != nil {
		return "", "", nil, err
	}
	return lines[0], lines[1], changes, nil
}

// parseDiffTree parses `git diff-tree -r -z --no-renames` output: for each
// path, ":oldmode newmode oldsha newsha status\0path\0".
func parseDiffTree(raw []byte) ([]change, error) {
	fields := strings.Split(string(raw), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		meta, path := strings.Fields(strings.TrimPrefix(fields[i], ":")), fields[i+1]
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff-tree entry %q", fields[i])
		}
		if !utf8.ValidString(path) || path == "" {
			return nil, fmt.Errorf("publish: path %q is not valid UTF-8", path)
		}
		c := change{path: path, oldMode: meta[0], newMode: meta[1], newSHA: meta[3]}
		for _, m := range []string{c.oldMode, c.newMode} {
			switch m {
			case "000000", "100644", "100755", "120000":
			case "160000":
				return nil, fmt.Errorf("publish: %s is a submodule; Mosso does not publish submodule changes", path)
			default:
				return nil, fmt.Errorf("publish: %s has unsupported mode %s", path, m)
			}
		}
		if c.newMode != "000000" && !shaPattern.MatchString(c.newSHA) {
			return nil, fmt.Errorf("unexpected diff-tree entry %q", fields[i])
		}
		changes = append(changes, c)
	}
	if len(changes) > maxChangedFiles {
		return nil, fmt.Errorf("publish: %d changed files, more than the %d limit", len(changes), maxChangedFiles)
	}
	return changes, nil
}

// readContents fills in the content of every added or modified path.
func readContents(ctx context.Context, vc *vercel.Client, session, dir string, changes []change) error {
	var shas []string
	seen := map[string]bool{}
	for _, c := range changes {
		if c.newMode != "000000" && !seen[c.newSHA] {
			seen[c.newSHA] = true
			shas = append(shas, c.newSHA)
		}
	}
	if len(shas) == 0 {
		return nil
	}
	res, err := vc.Run(ctx, session, vercel.Command{
		Name:           "bash",
		Args:           append([]string{"-c", readScript, "read", fmt.Sprint(MaxPublishBytes)}, shas...),
		Cwd:            dir,
		MaxOutputBytes: MaxPublishBytes/3*4 + 4*len(shas) + 1<<20,
	})
	if err != nil {
		return fmt.Errorf("read changed files: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("read changed files: exit code %d: %s", res.ExitCode, tail(res.Stderr))
	}
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != len(shas) {
		return fmt.Errorf("read changed files: got %d of %d files", len(lines), len(shas))
	}
	content := map[string][]byte{}
	for i, sha := range shas {
		b, err := base64.StdEncoding.DecodeString(lines[i])
		if err != nil {
			return fmt.Errorf("read changed files: decode %s: %w", sha, err)
		}
		content[sha] = b
	}
	for i := range changes {
		if changes[i].newMode != "000000" {
			changes[i].content = content[changes[i].newSHA]
		}
	}
	return nil
}

// createTree creates the tree for changes on top of baseTree. With inline
// set, UTF-8 text files are sent in the tree request itself; everything else
// is uploaded as a blob first.
func createTree(ctx context.Context, gh *Client, repo, baseTree string, changes []change, inline bool) (string, error) {
	entries := make([]treeEntry, 0, len(changes))
	for _, c := range changes {
		e := treeEntry{Path: c.path, Mode: c.newMode, Type: "blob"}
		switch {
		case c.newMode == "000000":
			e.Mode = c.oldMode // SHA stays nil: delete the path
		case inline && c.newMode != "120000" && utf8.Valid(c.content) && !bytes.ContainsRune(c.content, 0):
			s := string(c.content)
			e.Content = &s
		default:
			sha, err := gh.createBlob(ctx, repo, base64.StdEncoding.EncodeToString(c.content))
			if err != nil {
				return "", err
			}
			e.SHA = &sha
		}
		entries = append(entries, e)
	}
	return gh.createTree(ctx, repo, baseTree, entries)
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
