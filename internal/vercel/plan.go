package vercel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Vercel plans, as GET /v2/teams/{teamId} reports them in billing.plan.
const (
	PlanHobby      = "hobby"
	PlanPro        = "pro"
	PlanEnterprise = "enterprise"
)

// HobbyMaxTimeout is the longest sandbox session on the Hobby plan, and the
// limit Mosso assumes when it cannot tell the plan.
const HobbyMaxTimeout = 45 * time.Minute

// Limits are the Sandbox limits of a Vercel plan that Mosso needs.
type Limits struct {
	// MaxSessionDuration is the longest a sandbox session may run before
	// Vercel stops it. A persistent sandbox gets a new session each time it
	// resumes, so this caps a session, not the sandbox.
	MaxSessionDuration time.Duration
}

// LimitsFor returns the documented Sandbox limits of a plan
// (https://vercel.com/docs/sandbox/pricing#runtime-limits). Vercel's API
// does not report them, so they are written down here. An unknown or empty
// plan gets the Hobby limits, which every plan allows.
func LimitsFor(plan string) Limits {
	switch plan {
	case PlanPro, PlanEnterprise:
		return Limits{MaxSessionDuration: MaxTimeout}
	}
	return Limits{MaxSessionDuration: HobbyMaxTimeout}
}

// KnownPlan reports whether plan is one Mosso knows the limits of.
func KnownPlan(plan string) bool {
	return plan == PlanHobby || plan == PlanPro || plan == PlanEnterprise
}

// Plan returns the billing plan of the team the client works for
// ("hobby", "pro", "enterprise", or whatever else Vercel reports, in lower
// case), from GET /v2/teams/{teamId}. It is "" if the team has no billing
// information.
func (c *Client) Plan(ctx context.Context) (string, error) {
	var resp struct {
		Billing *struct {
			Plan string `json:"plan"`
		} `json:"billing"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v2/teams/"+url.PathEscape(c.cfg.TeamID), nil, nil, &resp); err != nil {
		return "", fmt.Errorf("get the Vercel team: %w", err)
	}
	if resp.Billing == nil {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(resp.Billing.Plan)), nil
}
