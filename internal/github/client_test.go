package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidRepoAndBranch(t *testing.T) {
	for s, want := range map[string]bool{
		"octo/widgets": true, "octo-cat/my.repo_1": true,
		"": false, "octo": false, "octo/": false, "/x": false, "octo/../x": false, "octo/..": false,
		"octo/x/y": false, "-octo/x": false, "octo/x.git": false, "octo/x y": false,
	} {
		if got := ValidRepo(s); got != want {
			t.Errorf("ValidRepo(%q) = %v, want %v", s, got, want)
		}
	}
	for s, want := range map[string]bool{
		"main": true, "release/1.2": true, "mosso/0a1b2c": true, "feature_x": true,
		"": false, "-x": false, "--upload-pack=x": false, "a..b": false, "a/": false, "a//b": false,
		"a.lock": false, "a/.b": false, "a@{1}": false, "a b": false, "a~1": false, "a:b": false, "x.": false,
	} {
		if got := ValidBranch(s); got != want {
			t.Errorf("ValidBranch(%q) = %v, want %v", s, got, want)
		}
	}
	if b := NewBranchName(); !strings.HasPrefix(b, BranchPrefix) || !ValidBranch(b) || b == NewBranchName() {
		t.Fatalf("NewBranchName() = %q", b)
	}
}

func TestClientSendsTokenAndRedactsErrors(t *testing.T) {
	var gotAuth, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotVersion = r.Header.Get("Authorization"), r.Header.Get("X-GitHub-Api-Version")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"message":"Bad credentials for %s","errors":[{"message":"echo %s"}]}`, testGitHubToken, testGitHubToken)
	}))
	defer srv.Close()
	c := NewClient(Token(testGitHubToken))
	c.BaseURL = srv.URL

	_, err := c.User(context.Background())
	if err == nil || strings.Contains(err.Error(), testGitHubToken) ||
		!strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "mosso auth github") {
		t.Fatalf("err = %v", err)
	}
	if gotAuth != "Bearer "+testGitHubToken || gotVersion == "" {
		t.Fatalf("Authorization = %q, version = %q", gotAuth, gotVersion)
	}
}

func TestRepoNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	c := NewClient("tok")
	c.BaseURL = srv.URL
	_, err := c.Repo(context.Background(), testRepo)
	if !IsNotFound(err) || !strings.Contains(err.Error(), "cannot access") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientReportsTokenExpirationAndPermissionHints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("GitHub-Authentication-Token-Expiration", "2026-12-01 08:00:00 UTC")
		if r.URL.Path == "/user" {
			_, _ = io.WriteString(w, `{"login":"octo","id":42}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by personal access token"}`)
	}))
	defer srv.Close()
	c := NewClient("tok")
	c.BaseURL = srv.URL
	if !c.TokenExpiration().IsZero() {
		t.Fatal("expiration should be unknown before a request")
	}
	if _, err := c.User(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.TokenExpiration(); got.Format(time.RFC3339) != "2026-12-01T08:00:00Z" {
		t.Fatalf("expiration = %v", got)
	}
	_, err := c.createPullRequest(context.Background(), testRepo, NewPullRequest{Title: "x", Head: "mosso/x", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "Pull requests set to Read and write") {
		t.Fatalf("err = %v, want a permissions hint", err)
	}
}
