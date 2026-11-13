package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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

func TestParseToken(t *testing.T) {
	if tok, err := ParseToken("  " + testGitHubToken + "\n"); err != nil || tok.Reveal() != testGitHubToken {
		t.Fatalf("ParseToken trims and accepts a fine-grained token: %v", err)
	}
	for _, bad := range []string{"", "short_1", "github_pat_abc def0000000000000000", "github_pat_abc\"; rm -rf /", "ghp_" + strings.Repeat("é", 30)} {
		if _, err := ParseToken(bad); err == nil || (len(bad) > 8 && strings.Contains(err.Error(), bad)) {
			t.Errorf("ParseToken(%q) err = %v", bad, err)
		}
	}
}

func TestSaveToken(t *testing.T) {
	ctx := context.Background()
	origAvail, origWrite := keychainAvailable, writeKeychain
	t.Cleanup(func() { keychainAvailable, writeKeychain = origAvail, origWrite })

	keychainAvailable = func() bool { return false }
	if err := SaveToken(ctx, Token(testGitHubToken)); err != ErrNoKeychain {
		t.Fatalf("err = %v, want ErrNoKeychain", err)
	}

	var stored string
	keychainAvailable = func() bool { return true }
	writeKeychain = func(_ context.Context, v string) error { stored = v; return nil }
	if err := SaveToken(ctx, Token(testGitHubToken)); err != nil || stored != testGitHubToken {
		t.Fatalf("err = %v, stored %q", err, stored)
	}
	// What was stored is what LoadToken reads next: rotation needs no restart.
	stubKeychain(t, stored, nil)
	if tok, src, err := LoadToken(ctx); err != nil || tok.Reveal() != testGitHubToken || src != SourceKeychain {
		t.Fatalf("LoadToken after save: %q %v", src, err)
	}
	if err := SaveToken(ctx, Token("bad token")); err == nil || stored != testGitHubToken {
		t.Fatal("a malformed token must not be stored")
	}
}

func TestNewTokenURL(t *testing.T) {
	u, err := url.Parse(NewTokenURL)
	if err != nil || u.Host != "github.com" || u.Path != "/settings/personal-access-tokens/new" {
		t.Fatalf("NewTokenURL = %q (%v)", NewTokenURL, err)
	}
	want := url.Values{
		"name": {"Mosso"}, "description": {"GitHub access for Mosso coding workspaces"}, "expires_in": {"90"},
		"contents": {"write"}, "pull_requests": {"write"},
	}
	if got := u.Query(); got.Encode() != want.Encode() {
		t.Fatalf("query = %v, want exactly %v (no workflows, actions or administration)", got, want)
	}
}
