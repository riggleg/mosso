package claude

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
	// EnvToken is the variable Claude Code reads a `claude setup-token`
	// OAuth token from. Mosso also falls back to it on the server.
	EnvToken = "CLAUDE_CODE_OAUTH_TOKEN"
	// KeychainService and KeychainAccount identify the macOS Keychain item
	// that holds the token.
	KeychainService = keychain.Service
	KeychainAccount = "claude-code-oauth-token"

	// SourceKeychain and SourceEnv report where a token was loaded from.
	SourceKeychain = "keychain"
	SourceEnv      = "env"
)

// ErrNoToken means neither the Keychain nor the environment has a token.
var ErrNoToken = errors.New("no Claude Code token configured: run `mosso auth claude` on macOS, or set " + EnvToken)

// Token is a Claude Code OAuth token. It formats as "[REDACTED]" so it
// cannot leak through fmt, logs or JSON by accident; call Reveal only where
// the raw value must be handed to Claude Code.
type Token string

func (Token) String() string               { return "[REDACTED]" }
func (Token) GoString() string             { return "[REDACTED]" }
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }
func (t Token) Reveal() string             { return string(t) }
func (t Token) redact(s string) string     { return redact(s, string(t)) }

// Redact replaces every occurrence of the token in s with "[REDACTED]".
func (t Token) Redact(s string) string { return t.redact(s) }

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}

// readKeychain returns the stored token, or "" if there is none. It is a
// variable so tests can replace it.
var readKeychain = func(ctx context.Context) (string, error) {
	return keychain.Read(ctx, KeychainAccount)
}

// LoadToken returns the Claude Code OAuth token and where it came from. It
// prefers the macOS Keychain item and falls back to CLAUDE_CODE_OAUTH_TOKEN.
// It reads the token on every call so a rotated token is picked up without a
// restart.
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
	if err := validateToken(v); err != nil {
		return "", "", fmt.Errorf("%s token: %w", source, err)
	}
	return Token(v), source, nil
}

// validateToken rejects values that would not work, without echoing them.
func validateToken(v string) error {
	if strings.HasPrefix(v, "sk-ant-api") {
		return errors.New("this is an Anthropic API key, not a subscription token; generate one with `claude setup-token`")
	}
	if len(v) > 4096 || strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return errors.New("malformed token; paste exactly what `claude setup-token` printed")
	}
	return nil
}

// StoreInKeychain saves a token in the login Keychain, replacing any
// existing one. `security` prompts for the value on the terminal without
// echoing it, so the token never appears in arguments or shell history.
func StoreInKeychain(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if !keychain.Available() {
		return fmt.Errorf("the macOS Keychain is only available on macOS; set %s in the server's environment instead", EnvToken)
	}
	return keychain.Store(ctx, KeychainAccount, "Mosso: Claude Code OAuth token", stdin, stdout, stderr)
}
