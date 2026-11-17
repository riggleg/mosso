package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/riggleg/mosso/internal/vercel"
)

// Mosso finds out the Vercel plan with the credentials it already has
// (GET /v2/teams/{teamId}), and derives the longest sandbox session it may
// request from it: Vercel's API does not report Sandbox limits, so they are
// the documented ones (vercel.LimitsFor). The plan is cached and checked
// again after planTTL, and at once when a request asks for more than the
// cached plan allows, so an upgraded account is noticed without a restart
// or any configuration. Until a check succeeds, or for a plan Mosso does
// not know, it assumes the Hobby limits, which every plan allows.

const (
	// planTTL is how long a detected plan is trusted.
	planTTL = time.Hour
	// planRetry is how long Mosso waits to ask again after a failure.
	planRetry = 5 * time.Minute
	// planTimeout bounds one check.
	planTimeout = 10 * time.Second
)

// planInfo is what Mosso knows about the Vercel plan, as GET /vercel/auth
// reports it.
type planInfo struct {
	// Plan is "hobby", "pro", "enterprise", another plan Vercel reported,
	// or "" while it is unknown.
	Plan string `json:"plan"`
	// Known says Mosso knows Plan's limits; otherwise it uses Hobby's.
	Known bool `json:"plan_known"`
	// MaxSessionSeconds is the longest sandbox session Mosso lets
	// workspaces request.
	MaxSessionSeconds int `json:"max_session_seconds"`
	// DefaultSessionSeconds and DefaultIdleSeconds are what a new
	// workspace gets unless it asks for something else.
	DefaultSessionSeconds int `json:"default_session_seconds"`
	DefaultIdleSeconds    int `json:"default_idle_timeout_seconds"`
	// CheckedAt is when the plan was last read from Vercel; PlanError is
	// why the last check failed, if it did (the plan from an earlier
	// successful check is kept).
	CheckedAt time.Time `json:"plan_checked_at,omitzero"`
	PlanError string    `json:"plan_error,omitempty"`

	tried time.Time // the last attempt, successful or not
}

// maxSession is the longest session the plan allows.
func (p planInfo) maxSession() time.Duration { return vercel.LimitsFor(p.Plan).MaxSessionDuration }

// vercelPlan returns the plan, from the cache unless it is stale or force
// is set. A failed check keeps the plan from the last successful one, or
// Hobby's limits if there was none. Checks run one at a time.
func (s *Server) vercelPlan(ctx context.Context, force bool) planInfo {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	s.mu.Lock()
	p := s.plan
	s.mu.Unlock()
	fresh := !p.tried.IsZero() && time.Since(p.tried) < planRetry ||
		p.PlanError == "" && !p.CheckedAt.IsZero() && time.Since(p.CheckedAt) < planTTL
	if s.vercel == nil || fresh && !force {
		return p.withLimits()
	}
	ctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	plan, err := s.vercel.Plan(ctx)
	p.tried = time.Now()
	if err != nil {
		p.PlanError = "could not read the Vercel plan (" + err.Error() + "); using the limits of " + planLabel(p.Plan)
		log.Printf("vercel: %s", p.PlanError)
	} else {
		if plan != p.Plan {
			if p.Plan == "" {
				log.Printf("vercel: the team's plan is %s; sandbox sessions may last up to %s", planLabel(plan), vercel.LimitsFor(plan).MaxSessionDuration)
			} else {
				log.Printf("vercel: the team's plan changed from %s to %s; sandbox sessions may last up to %s", planLabel(p.Plan), planLabel(plan), vercel.LimitsFor(plan).MaxSessionDuration)
			}
		}
		p.Plan, p.CheckedAt, p.PlanError = plan, p.tried, ""
	}
	s.mu.Lock()
	s.plan = p
	s.mu.Unlock()
	return p.withLimits()
}

func (p planInfo) withLimits() planInfo {
	p.Known = vercel.KnownPlan(p.Plan)
	p.MaxSessionSeconds = int(p.maxSession().Seconds())
	p.DefaultSessionSeconds = int(defaultWorkspaceTimeout.Seconds())
	p.DefaultIdleSeconds = int(defaultIdleTimeout.Seconds())
	return p
}

// planLabel names a plan for messages.
func planLabel(plan string) string {
	if plan == "" || !vercel.KnownPlan(plan) {
		if plan == "" {
			return "the Hobby plan (the plan is unknown)"
		}
		return "the Hobby plan (Mosso does not know the " + plan + " plan)"
	}
	return "the " + map[string]string{vercel.PlanHobby: "Hobby", vercel.PlanPro: "Pro", vercel.PlanEnterprise: "Enterprise"}[plan] + " plan"
}

// maxSessionFor returns the longest session Mosso may request, checking
// the plan again first if want is longer than the cached plan allows, so
// that an account upgraded since the last check can use its new limit.
func (s *Server) maxSessionFor(ctx context.Context, want time.Duration) planInfo {
	p := s.vercelPlan(ctx, false)
	if want > p.maxSession() {
		p = s.vercelPlan(ctx, true)
	}
	return p
}

// vercelAuthStatus reports whether Vercel Sandbox credentials are
// configured, so the web UI can say why workspaces are read-only, and the
// detected plan with the session lengths it allows. It never returns the
// credentials. ?refresh=1 checks the plan again.
func (s *Server) vercelAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.vercel == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	p := s.vercelPlan(r.Context(), r.URL.Query().Get("refresh") != "")
	writeJSON(w, http.StatusOK, struct {
		Configured bool `json:"configured"`
		planInfo
	}{true, p})
}
