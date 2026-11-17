package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type vercelStatus struct {
	Configured        bool   `json:"configured"`
	Plan              string `json:"plan"`
	Known             bool   `json:"plan_known"`
	MaxSessionSeconds int    `json:"max_session_seconds"`
	DefaultSession    int    `json:"default_session_seconds"`
	DefaultIdle       int    `json:"default_idle_timeout_seconds"`
	PlanError         string `json:"plan_error"`
}

func vercelAuth(t *testing.T, srv *Server, query string) vercelStatus {
	t.Helper()
	rec := do(t, srv.Handler(), http.MethodGet, "/vercel/auth"+query, "")
	var st vercelStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return st
}

func TestVercelPlanDetection(t *testing.T) {
	for _, tc := range []struct {
		plan  string
		fail  bool
		want  string
		known bool
		max   int
	}{
		{plan: "hobby", want: "hobby", known: true, max: 2700},
		{plan: "pro", want: "pro", known: true, max: 86400},
		{plan: "enterprise", want: "enterprise", known: true, max: 86400},
		{plan: "galaxy", want: "galaxy", max: 2700},
		{fail: true, want: "", max: 2700},
	} {
		fake, api := newFakeVercel(t)
		fake.plan, fake.planFail = tc.plan, tc.fail
		srv := newServerOver(t, fake, api, t.TempDir())
		st := vercelAuth(t, srv, "")
		if !st.Configured || st.Plan != tc.want || st.Known != tc.known || st.MaxSessionSeconds != tc.max {
			t.Errorf("plan %q (fail=%t): %+v", tc.plan, tc.fail, st)
		}
		if st.DefaultSession != 2700 || st.DefaultIdle != 900 {
			t.Errorf("defaults = %d, %d; want 45 and 15 minutes whatever the plan", st.DefaultSession, st.DefaultIdle)
		}
		if tc.fail && !strings.Contains(st.PlanError, "Hobby") {
			t.Errorf("plan error = %q, want it to say Hobby limits are used", st.PlanError)
		}
	}
}

func TestVercelPlanIsCached(t *testing.T) {
	fake, api := newFakeVercel(t)
	fake.plan = "pro"
	srv := newServerOver(t, fake, api, t.TempDir(), func(srv *Server) { srv.planForceGap = 0 })
	for range 3 {
		vercelAuth(t, srv, "")
	}
	if fake.planReads != 1 {
		t.Fatalf("plan read %d times, want once", fake.planReads)
	}
	vercelAuth(t, srv, "?refresh=1")
	if fake.planReads != 2 {
		t.Fatalf("plan read %d times after a refresh, want twice", fake.planReads)
	}
	// A failed check keeps the plan from the last good one.
	fake.mu.Lock()
	fake.planFail = true
	fake.mu.Unlock()
	if st := vercelAuth(t, srv, "?refresh=1"); st.Plan != "pro" || st.MaxSessionSeconds != 86400 || st.PlanError == "" {
		t.Fatalf("after a failed check: %+v", st)
	}
	// Forced checks are at most one a minute.
	srv.planForceGap = time.Minute
	before := fake.planReads
	vercelAuth(t, srv, "?refresh=1")
	if fake.planReads != before {
		t.Fatal("a forced check right after another one reached Vercel")
	}
}

// TestAccountUpgradeIsNoticed: a Hobby account cannot ask for an hour; once
// it is upgraded to Pro on vercel.com, the next request that asks for more
// than Hobby allows checks the plan again and succeeds, without a restart.
func TestAccountUpgradeIsNoticed(t *testing.T) {
	fake, api := newFakeVercel(t)
	fake.plan = "hobby"
	fake.stdoutFor = fakeRepoSandbox()
	srv := newServerOver(t, fake, api, t.TempDir(), func(srv *Server) {
		withGitHub(t, srv, &fakeGitHubAPI{})
		srv.planForceGap = 0
	})
	h := srv.Handler()
	rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets","timeout_seconds":3600}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Hobby") {
		t.Fatalf("an hour on Hobby: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodPost, "/sandboxes", `{"timeout_seconds":3600}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a plain sandbox of an hour on Hobby: %d", rec.Code)
	}
	fake.setPlan("pro")
	rec = do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets","timeout_seconds":86400,"idle_timeout_seconds":900}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("24 hours on Pro: %d %s", rec.Code, rec.Body)
	}
	var ws idleStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	if ws.SessionSeconds != 86400 || ws.IdleTimeoutSeconds != 900 {
		t.Fatalf("workspace = %+v", ws)
	}
	fake.mu.Lock()
	timeout := fake.sandboxes[fake.created[0]].timeout
	fake.mu.Unlock()
	if timeout != (24 * time.Hour).Milliseconds() {
		t.Fatalf("Vercel was asked for a %d ms session", timeout)
	}
	if st := vercelAuth(t, srv, ""); st.Plan != "pro" || st.MaxSessionSeconds != 86400 {
		t.Fatalf("after the upgrade: %+v", st)
	}
	// Still at most 24 hours, and at least 5 minutes.
	for _, body := range []string{`{"repo":"octo/widgets","timeout_seconds":86401}`, `{"repo":"octo/widgets","timeout_seconds":60}`, `{"repo":"octo/widgets","idle_timeout_seconds":10}`} {
		if rec := do(t, h, http.MethodPost, "/workspaces", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, rec.Code)
		}
	}
}

// TestSessionLengthIsKeptWhenResuming: if Vercel starts a resumed session
// shorter than the workspace's session length, Mosso extends it.
func TestSessionLengthIsKeptWhenResuming(t *testing.T) {
	fake, api := newFakeVercel(t)
	fake.plan = "pro"
	fake.stdoutFor = fakeRepoSandbox()
	h := &persistentHarness{t: t, fake: fake, vercelAPI: api, dataDir: t.TempDir()}
	h.start()
	rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets","timeout_seconds":14400}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &h.ws)
	h.waitStatus("ready")
	fake.mu.Lock()
	fake.resumeTimeout = (5 * time.Minute).Milliseconds()
	fake.mu.Unlock()
	fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	h.finishTask("after the session ended")
	fake.mu.Lock()
	extended := append([]int64(nil), fake.extended...)
	fake.mu.Unlock()
	if len(extended) != 1 || extended[0] != (4*time.Hour-5*time.Minute).Milliseconds() {
		t.Fatalf("extended = %v, want the session brought to 4 hours", extended)
	}
	if left := time.Until(h.idle().ExpiresAt); left < 3*time.Hour {
		t.Fatalf("the resumed session ends in %s, want about 4 hours", left)
	}
}

// TestVercelCapacityIsVercelsToEnforce: Mosso no longer caps how many
// sandboxes run; when Vercel refuses one, it says why with 429.
func TestVercelCapacityIsVercelsToEnforce(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	for i := range 7 {
		if rec := do(t, h, http.MethodPost, "/sandboxes", ""); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	fake.mu.Lock()
	fake.atCapacity = true
	fake.mu.Unlock()
	rec := do(t, h, http.MethodPost, "/sandboxes", "")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "limit of concurrent sandboxes") {
		t.Fatalf("at capacity: %d %s", rec.Code, rec.Body)
	}
}

func TestResumeAtCapacity(t *testing.T) {
	h := newPersistentHarness(t)
	h.fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	h.fake.mu.Lock()
	h.fake.atCapacity = true
	h.fake.mu.Unlock()
	rec := do(t, h.srv.Handler(), http.MethodPost, "/workspaces/"+h.ws.Name+"/connect", `{}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "limit of concurrent sandboxes") {
		t.Fatalf("resume at capacity: %d %s", rec.Code, rec.Body)
	}
	if st := h.idle(); st.Status != "stopped" {
		t.Fatalf("status = %s, want still stopped", st.Status)
	}
}
