package vercel

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestPlanDetection(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		max        time.Duration
	}{
		{`{"id":"team_1","billing":{"plan":"hobby"}}`, PlanHobby, 45 * time.Minute},
		{`{"id":"team_1","billing":{"plan":"pro","period":{}}}`, PlanPro, 24 * time.Hour},
		{`{"id":"team_1","billing":{"plan":"Enterprise"}}`, PlanEnterprise, 24 * time.Hour},
		// A plan Mosso does not know gets the conservative Hobby limits.
		{`{"id":"team_1","billing":{"plan":"startup-2027"}}`, "startup-2027", 45 * time.Minute},
		{`{"id":"team_1","billing":null}`, "", 45 * time.Minute},
	} {
		f, c := newFakeAPI(t)
		f.handle("GET /v2/teams/team_1", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) })
		plan, err := c.Plan(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if plan != tc.want {
			t.Errorf("%s: plan = %q, want %q", tc.body, plan, tc.want)
		}
		if got := LimitsFor(plan).MaxSessionDuration; got != tc.max {
			t.Errorf("%s: max session = %s, want %s", tc.body, got, tc.max)
		}
		if KnownPlan(plan) != (tc.want == PlanHobby || tc.want == PlanPro || tc.want == PlanEnterprise) {
			t.Errorf("%s: KnownPlan(%q) wrong", tc.body, plan)
		}
	}
}

func TestPlanDetectionFailure(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("GET /v2/teams/team_1", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"forbidden","message":"Not authorized"}}`, http.StatusForbidden)
	})
	if _, err := c.Plan(context.Background()); err == nil {
		t.Fatal("want an error for a 403")
	}
}

func TestExtendTimeout(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/extend-timeout", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"session":{}}`)
	})
	if err := c.ExtendTimeout(context.Background(), "sbx_1", 90*time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	body := f.requests[0].Body
	f.mu.Unlock()
	if body["duration"] != float64((90 * time.Minute).Milliseconds()) {
		t.Fatalf("body = %v", body)
	}
}

func TestOverCapacity(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"too_many_requests","message":"The concurrency limit has been exceeded."}}`, http.StatusTooManyRequests)
	})
	f.handle("DELETE /v2/sandboxes/mosso-abc", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, `{}`, http.StatusNotFound) })
	_, err := c.Create(context.Background(), "mosso-abc", CreateOptions{})
	if !OverCapacity(err) {
		t.Fatalf("err = %v, want OverCapacity", err)
	}
	if OverCapacity(&APIError{StatusCode: http.StatusBadGateway}) {
		t.Fatal("a 502 is not over capacity")
	}
}
