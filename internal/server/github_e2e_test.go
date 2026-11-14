package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/vercel"
)

// TestGitHubEndToEnd is the GitHub milestone acceptance test: create a
// workspace from a real repository, have Claude make a change, and open a
// real (draft) pull request. It uses real credentials and accounts, so it
// only runs when asked:
//
//	MOSSO_E2E=1 MOSSO_E2E_GITHUB_REPO=you/disposable-repo \
//	  go test ./internal/server -run TestGitHubEndToEnd -v -timeout 30m
//
// It needs the Vercel and Claude credentials of TestClaudeEndToEnd plus a
// GitHub token (`mosso auth github` or MOSSO_GITHUB_TOKEN) that can write to
// the repository. Use a disposable repository: the test pushes a mosso/*
// branch and opens a draft pull request against MOSSO_E2E_GITHUB_BASE (the
// default branch if unset). It never merges or writes to the base branch,
// and afterwards closes the pull request and deletes the branch unless
// MOSSO_E2E_KEEP_PR=1.
func TestGitHubEndToEnd(t *testing.T) {
	if os.Getenv("MOSSO_E2E") != "1" {
		t.Skip("set MOSSO_E2E=1 to run against real Vercel, Claude and GitHub accounts")
	}
	repo := os.Getenv("MOSSO_E2E_GITHUB_REPO")
	if !github.ValidRepo(repo) {
		t.Skip("set MOSSO_E2E_GITHUB_REPO=owner/repo to a disposable repository the GitHub token can write to")
	}
	cfg, err := vercel.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := claude.LoadToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	ghToken, source, err := github.LoadToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GitHub token source: %s", source)

	srv := New(vercel.NewClient(cfg))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("destroy sandbox: %v (check the Vercel dashboard for mosso-* sandboxes)", err)
		}
	})
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"repo": repo, "base": os.Getenv("MOSSO_E2E_GITHUB_BASE"), "timeout_seconds": 1200})
	start := time.Now()
	rec := do(t, h, http.MethodPost, "/workspaces", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rec.Code, rec.Body)
	}
	var ws testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	t.Logf("workspace %s: %s@%s (%s) on branch %s in %s", ws.Name, ws.Repo, ws.Base, ws.BaseSHA, ws.Branch, time.Since(start).Round(time.Second))

	var prNumber int
	t.Cleanup(func() {
		if os.Getenv("MOSSO_E2E_KEEP_PR") == "1" {
			t.Logf("keeping branch %s and pull request #%d; close and delete them when done", ws.Branch, prNumber)
			return
		}
		if prNumber != 0 {
			e2eGitHub(t, ghToken, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", ws.Repo, prNumber), `{"state":"closed"}`)
		}
		// Only ever delete the branch this test created.
		if strings.HasPrefix(ws.Branch, github.BranchPrefix) && github.ValidBranch(ws.Branch) {
			e2eGitHub(t, ghToken, http.MethodDelete, "/repos/"+ws.Repo+"/git/refs/heads/"+ws.Branch, "")
		}
	})

	prPath := "/workspaces/" + ws.Name + "/pull-request"
	if rec := do(t, h, http.MethodPost, prPath, `{"title":"nothing yet"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pull request before any change: %d %s, want 422", rec.Code, rec.Body)
	}

	file := "mosso-e2e/" + strings.TrimPrefix(ws.Branch, github.BranchPrefix) + ".md"
	prompt, _ := json.Marshal(map[string]string{"prompt": "Create the file " + file +
		" containing exactly one line: Hello from Mosso. Do not change any other file and do not commit."})
	start = time.Now()
	rec = do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", string(prompt))
	if rec.Code != http.StatusOK {
		t.Fatalf("claude message: %d %s", rec.Code, rec.Body)
	}
	var turn claude.Turn
	_ = json.Unmarshal(rec.Body.Bytes(), &turn)
	t.Logf("claude in %s: events=%d result=%q", time.Since(start).Round(time.Second), len(turn.Events), turn.Result)
	if turn.IsError {
		t.Fatalf("claude reported an error: %s", turn.Result)
	}

	rec = do(t, h, http.MethodPost, prPath, `{"title":"Mosso end-to-end test (safe to close)","body":"Opened by TestGitHubEndToEnd.","draft":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("pull request: %d %s", rec.Code, rec.Body)
	}
	var pub github.Published
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	prNumber = pub.Number
	t.Logf("pull request #%d: %s (commit %s)", pub.Number, pub.URL, pub.Commit)
	if !strings.HasPrefix(pub.URL, "https://github.com/"+ws.Repo+"/pull/") || pub.Branch != ws.Branch {
		t.Fatalf("unexpected result: %s", rec.Body)
	}

	// Publishing again without new work keeps the same pull request.
	rec = do(t, h, http.MethodPost, prPath, `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), pub.URL) {
		t.Fatalf("second publish: %d %s", rec.Code, rec.Body)
	}

	// A follow-up that rewrites the published commit (as a rebase does)
	// updates the same pull request with --force-with-lease.
	prompt, _ = json.Marshal(map[string]string{"prompt": "Append a second line, Updated by Mosso., to " + file +
		", then run exactly: git add -A && git commit --amend --no-edit. Do not change anything else and do not push."})
	if rec := do(t, h, http.MethodPost, "/workspaces/"+ws.Name+"/claude/messages", string(prompt)); rec.Code != http.StatusOK {
		t.Fatalf("follow-up: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, prPath, `{"title":"Amend"}`)
	var again github.Published
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	t.Logf("update: %s", rec.Body)
	if rec.Code != http.StatusOK || again.URL != pub.URL || again.Commit == pub.Commit || !again.Forced {
		t.Fatalf("amended publish: %d %s, want the same pull request updated with a forced push", rec.Code, rec.Body)
	}
}

// e2eGitHub sends one cleanup request to the GitHub API and reports a
// failure (without the token) so the resource can be removed by hand.
func e2eGitHub(t *testing.T, token github.Token, method, path, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, github.DefaultBaseURL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("cleanup %s %s: %v: clean up by hand", method, path, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("cleanup %s %s: status %d: clean up by hand", method, path, resp.StatusCode)
	}
}
