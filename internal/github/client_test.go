package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestTreeEntryJSON(t *testing.T) {
	content, sha := "hi", strings.Repeat("a", 40)
	for _, tc := range []struct {
		e    treeEntry
		want string
	}{
		{treeEntry{Path: "a", Mode: "100644", Type: "blob", Content: &content}, `{"content":"hi","mode":"100644","path":"a","type":"blob"}`},
		{treeEntry{Path: "a", Mode: "100644", Type: "blob", SHA: &sha}, `{"mode":"100644","path":"a","sha":"` + sha + `","type":"blob"}`},
		{treeEntry{Path: "a", Mode: "100644", Type: "blob"}, `{"mode":"100644","path":"a","sha":null,"type":"blob"}`},
	} {
		got, _ := json.Marshal(tc.e)
		if string(got) != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
}
