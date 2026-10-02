package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector prefers the credential whose weekly quota window resets
// soonest, so no allowance is left on the table when a window rolls over.
//
// Ranking, best first:
//  1. credentials with a known, unexhausted weekly window, soonest reset first;
//  2. credentials with no quota signals yet (never used, or provider does not
//     report them);
//  3. credentials whose weekly window is nearly exhausted (they would only hit
//     a 429 and cool down).
//
// Within a tier the higher utilization wins, so an already-drained account is
// finished before a fresh one is touched. Credentials without any reset signal
// (e.g. Codex) fall back to round-robin so they still share load.
//
// ponytail: signals are the raw Anthropic rate-limit headers captured on the
// last response; only Claude emits them today. Extend resetFirstSignals when a
// second provider grows comparable headers.
type ResetFirstSelector struct {
	fallback RoundRobinSelector
}

const (
	resetFirstExhaustedUtilization = 0.97 // weekly window treated as spent
	resetFirstBlockedUtilization   = 0.95 // short window about to reject
)

type resetFirstScore struct {
	known       bool
	exhausted   bool
	reset       time.Time
	utilization float64
}

func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}

	scores := make(map[string]resetFirstScore, len(available))
	anyKnown := false
	for _, a := range available {
		sc := resetFirstSignals(a, now)
		scores[a.ID] = sc
		anyKnown = anyKnown || sc.known
	}
	if !anyKnown {
		return s.fallback.Pick(ctx, provider, model, opts, available)
	}

	sorted := append([]*Auth(nil), available...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return resetFirstLess(scores[sorted[i].ID], scores[sorted[j].ID])
	})
	return sorted[0], nil
}

func resetFirstLess(a, b resetFirstScore) bool {
	if a.exhausted != b.exhausted {
		return !a.exhausted
	}
	if a.known != b.known {
		return a.known
	}
	if a.known && !a.reset.Equal(b.reset) {
		return a.reset.Before(b.reset)
	}
	return a.utilization > b.utilization
}

// resetFirstSignals reads the Anthropic unified rate-limit headers stored on the
// credential. A reset in the past means the window has rolled over since the
// last response: treat it as fresh and due again one week later.
func resetFirstSignals(a *Auth, now time.Time) resetFirstScore {
	if a == nil || len(a.Quota.Signals) == 0 {
		return resetFirstScore{}
	}
	get := func(suffix string) (string, bool) {
		for k, v := range a.Quota.Signals {
			if strings.EqualFold(k, "anthropic-ratelimit-unified-"+suffix) {
				return strings.TrimSpace(v), true
			}
		}
		return "", false
	}
	resetRaw, ok := get("7d-reset")
	if !ok {
		return resetFirstScore{}
	}
	unix, err := strconv.ParseInt(resetRaw, 10, 64)
	if err != nil || unix <= 0 {
		return resetFirstScore{}
	}
	sc := resetFirstScore{known: true, reset: time.Unix(unix, 0)}
	util := func(suffix string) float64 {
		raw, _ := get(suffix)
		f, _ := strconv.ParseFloat(raw, 64)
		return f
	}
	// 7d_oi is the separate weekly window for the top-tier models; whichever is
	// fuller is the one that will reject first.
	sc.utilization = max(util("7d-utilization"), util("7d_oi-utilization"))
	if !sc.reset.After(now) {
		sc.reset = sc.reset.Add(7 * 24 * time.Hour)
		sc.utilization = 0
		return sc
	}
	if sc.utilization >= resetFirstExhaustedUtilization {
		sc.exhausted = true
		return sc
	}
	if status, _ := get("5h-status"); status != "" && !strings.EqualFold(status, "allowed") {
		sc.exhausted = true
		return sc
	}
	if fiveHourReset, ok := get("5h-reset"); ok {
		if u, errParse := strconv.ParseInt(fiveHourReset, 10, 64); errParse == nil && time.Unix(u, 0).After(now) && util("5h-utilization") >= resetFirstBlockedUtilization {
			sc.exhausted = true
		}
	}
	return sc
}
