package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/riggleg/mosso/internal/github"
)

// TestSaveCheckedGitHubToken: `mosso auth github` saves a token only after
// GitHub accepts it, so a failed check keeps the stored token.
func TestSaveCheckedGitHubToken(t *testing.T) {
	const (
		previous    = "github_pat_11PREVIOUS_workingtoken0"
		fineGrained = "github_pat_11NEW_finegrainedtoken00"
		classic     = "ghp_CLASSICtoken0000000000000000000000"
		rejected    = "github_pat_11REVOKED_rejectedtoken0"
	)
	var scopes *string // X-OAuth-Scopes for the classic token; nil: none
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + fineGrained:
		case "Bearer " + classic:
			if scopes != nil {
				w.Header().Set("X-OAuth-Scopes", *scopes)
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		_, _ = io.WriteString(w, `{"login":"octo","id":42}`)
	}))
	defer api.Close()
	newClient := func(tok github.Token) *github.Client {
		c := github.NewClient(tok)
		c.BaseURL = api.URL
		return c
	}
	str := func(s string) *string { return &s }

	for _, tc := range []struct {
		name, token string
		scopes      *string
		want        string // error substring; "" for success
	}{
		{"malformed", "not a token", nil, "does not look like"},
		{"rejected by GitHub", rejected, nil, "GitHub rejected the token"},
		{"classic without the scopes header", classic, nil, "cannot verify"},
		{"classic without repo", classic, str("public_repo"), "lacks the repo scope"},
		{"classic with repo", classic, str("repo, read:org"), ""},
		{"fine-grained", fineGrained, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := github.Token(previous)
			save := func(_ context.Context, tok github.Token) error { stored = tok; return nil }
			scopes = tc.scopes

			user, err := saveCheckedGitHubToken(context.Background(), tc.token, newClient, save)
			if tc.want == "" {
				if err != nil || user.Login != "octo" || stored.Reveal() != tc.token {
					t.Fatalf("user %v, err %v, stored the new token: %v", user, err, stored.Reveal() == tc.token)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if stored.Reveal() != previous {
				t.Fatal("a token that failed the check replaced the stored one")
			}
			if strings.Contains(err.Error(), tc.token) && tc.token != "not a token" {
				t.Fatal("the error includes the token")
			}
		})
	}
}
