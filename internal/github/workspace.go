package github

import (
	"context"
	"crypto/rand"
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
	// envCloneToken carries the token to Git commands' credential helper.
	envCloneToken = "MOSSO_GITHUB_TOKEN"
	// credentialHelper answers Git's credential requests from
	// $MOSSO_GITHUB_TOKEN. It is given on the command line after an empty
	// value that clears every other helper, so the token is never written to
	// a config file, the remote URL or the command line, and no other helper
	// (such as osxkeychain) is asked to store it.
	credentialHelper = `!f() { test "$1" = get && test -n "$MOSSO_GITHUB_TOKEN" || return 0; echo username=x-access-token; echo "password=$MOSSO_GITHUB_TOKEN"; }; f`
)

// gitBaseURL is where repositories are cloned from and pushed to; tests
// replace it.
var gitBaseURL = "https://github.com"

// repoURL is the HTTPS Git URL of repo. It never contains credentials.
func repoURL(repo string) string { return gitBaseURL + "/" + repo + ".git" }

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
// The token, if any, is only in this command's environment, read by
// credentialHelper (passed as $7).
const cloneScript = `set -euo pipefail
dir=$1 url=$2 base=$3 branch=$4 name=$5 email=$6 helper=$7
if ! command -v git >/dev/null 2>&1; then sudo dnf install -y -q git >&2; fi
git -c credential.helper= -c "credential.helper=$helper" \
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
			opts.Dir, repoURL(opts.Repo), opts.Base, opts.Branch, opts.AuthorName, opts.AuthorEmail, credentialHelper},
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
