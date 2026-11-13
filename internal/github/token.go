package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

	// SourceKeychain and SourceEnv report where a token was loaded from.
	SourceKeychain = "keychain"
	SourceEnv      = "env"
)

// ErrNoToken means neither the Keychain nor the environment has a token.
var ErrNoToken = errors.New("no GitHub token configured: run `mosso auth github` on macOS, or set " + EnvToken)

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

// StoreInKeychain saves a token in the login Keychain, replacing any
// existing one. `security` prompts for it without echoing.
func StoreInKeychain(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if !keychain.Available() {
		return fmt.Errorf("the macOS Keychain is only available on macOS; set %s in the server's environment instead", EnvToken)
	}
	return keychain.Store(ctx, KeychainAccount, "Mosso: GitHub token", stdin, stdout, stderr)
}
