package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func stubKeychain(t *testing.T, value string, err error) {
	t.Helper()
	orig := readKeychain
	readKeychain = func(context.Context) (string, error) { return value, err }
	t.Cleanup(func() { readKeychain = orig })
}

func TestLoadToken(t *testing.T) {
	ctx := context.Background()

	stubKeychain(t, testGitHubToken, nil)
	t.Setenv(EnvToken, "ghp_fromenv")
	if tok, src, err := LoadToken(ctx); err != nil || tok.Reveal() != testGitHubToken || src != SourceKeychain {
		t.Fatalf("keychain should win: src=%q err=%v", src, err)
	}

	stubKeychain(t, "", nil)
	if tok, src, err := LoadToken(ctx); err != nil || tok.Reveal() != "ghp_fromenv" || src != SourceEnv {
		t.Fatalf("env fallback: src=%q err=%v", src, err)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_unrelated")
	t.Setenv(EnvToken, "")
	if _, _, err := LoadToken(ctx); err != ErrNoToken {
		t.Fatalf("err = %v, want ErrNoToken (GITHUB_TOKEN must not be used implicitly)", err)
	}

	t.Setenv(EnvToken, "ghp_abc def")
	if _, _, err := LoadToken(ctx); err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("malformed token must be rejected without echoing it: %v", err)
	}

	stubKeychain(t, "", io.ErrUnexpectedEOF)
	if _, _, err := LoadToken(ctx); err == nil {
		t.Fatal("keychain errors must be reported, not silently skipped")
	}
}

func TestTokenNeverFormats(t *testing.T) {
	tok := Token(testGitHubToken)
	js, _ := json.Marshal(map[string]any{"t": tok})
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %s %q %#v %+v", tok, tok, tok, tok, tok), string(js)} {
		if strings.Contains(s, testGitHubToken) {
			t.Fatalf("token leaked through formatting: %s", s)
		}
	}
	if tok.Reveal() != testGitHubToken {
		t.Fatal("Reveal must return the raw token")
	}
}
