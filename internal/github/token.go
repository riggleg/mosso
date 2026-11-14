package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/riggleg/mosso/internal/keychain"
)

const (
	// EnvToken is the server-side fallback for the GitHub token. It is
	// deliberately not GITHUB_TOKEN, which is often a broad token exported
	// for other tools.
	EnvToken = "MOSSO_GITHUB_TOKEN"
	// KeychainAccount identifies the token's macOS Keychain item (service
	// "mosso").
	KeychainAccount = "github-token"

	// SourceKeychain, SourceEnv and SourceMemory report where a token was
	// loaded from. A memory token was pasted into the web UI on a system
	// without a Keychain and lasts until Mosso stops.
	SourceKeychain = "keychain"
	SourceEnv      = "env"
	SourceMemory   = "memory"

	// NewTokenURL opens GitHub's form for a fine-grained personal access
	// token with Mosso's settings preselected: name, description, a 90-day
	// expiration, and Contents and Pull requests: Read and write (Metadata:
	// Read is always included). The user still picks the repositories.
	// Nothing else is requested; in particular not Workflows, so the token
	// cannot change .github/workflows.
	NewTokenURL = "https://github.com/settings/personal-access-tokens/new" +
		"?name=Mosso&description=GitHub+access+for+Mosso+coding+workspaces&expires_in=90" +
		"&contents=write&pull_requests=write"

	keychainLabel = "Mosso: GitHub token"
)

var (
	// ErrNoToken means neither the Keychain nor the environment has a token.
	ErrNoToken = errors.New("GitHub is not connected: use Connect GitHub in the Mosso web UI, run `mosso auth github` on macOS, or set " + EnvToken)
	// ErrNoKeychain means SaveToken cannot store a token on this system.
	ErrNoKeychain = errors.New("the macOS Keychain is not available on this system")
)

// tokenPattern matches the characters of GitHub's token formats
// (github_pat_…, ghp_…).
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]{20,255}$`)

// ParseToken checks a token the user pasted, without echoing it in errors.
func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("paste the token GitHub generated")
	case !tokenPattern.MatchString(s):
		return "", errors.New("that does not look like a GitHub personal access token; paste exactly the token GitHub showed (it starts with github_pat_)")
	}
	return Token(s), nil
}

// Token is a GitHub personal access token. It formats as "[REDACTED]" so it
// cannot leak through fmt, logs or JSON by accident; call Reveal only where
// the raw value must be sent to GitHub.
type Token string

func (Token) String() string               { return "[REDACTED]" }
func (Token) GoString() string             { return "[REDACTED]" }
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }
func (t Token) Reveal() string             { return string(t) }

// redact replaces the token in s.
func (t Token) redact(s string) string {
	if t == "" {
		return s
	}
	return strings.ReplaceAll(s, string(t), "[REDACTED]")
}

// readKeychain is a variable so tests can replace it.
var readKeychain = func(ctx context.Context) (string, error) {
	return keychain.Read(ctx, KeychainAccount)
}

// LoadToken returns the GitHub token and where it came from. It prefers the
// macOS Keychain and falls back to MOSSO_GITHUB_TOKEN. It reads the token on
// every call so a rotated token is picked up without a restart.
func LoadToken(ctx context.Context) (Token, string, error) {
	v, err := readKeychain(ctx)
	if err != nil {
		return "", "", err
	}
	source := SourceKeychain
	if v == "" {
		v, source = strings.TrimSpace(os.Getenv(EnvToken)), SourceEnv
	}
	if v == "" {
		return "", "", ErrNoToken
	}
	if len(v) > 1024 || strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return "", "", fmt.Errorf("%s token: malformed GitHub token; paste exactly the token GitHub showed", source)
	}
	return Token(v), source, nil
}

// keychainAvailable and writeKeychain are variables so tests can replace
// them.
var keychainAvailable = keychain.Available

var writeKeychain = func(ctx context.Context, v string) error {
	return keychain.Write(ctx, KeychainAccount, keychainLabel, v)
}

// SaveToken stores tok in the macOS Keychain, replacing the previous token.
// Running servers pick it up on their next request. It returns
// ErrNoKeychain on other systems.
func SaveToken(ctx context.Context, tok Token) error {
	if !keychainAvailable() {
		return ErrNoKeychain
	}
	if !tokenPattern.MatchString(tok.Reveal()) {
		return errors.New("malformed GitHub token")
	}
	return writeKeychain(ctx, tok.Reveal())
}

// StoreInKeychain saves a token in the login Keychain, replacing any
// existing one. `security` prompts for it without echoing.
func StoreInKeychain(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if !keychain.Available() {
		return fmt.Errorf("the macOS Keychain is only available on macOS; set %s in the server's environment instead", EnvToken)
	}
	return keychain.Store(ctx, KeychainAccount, keychainLabel, stdin, stdout, stderr)
}
