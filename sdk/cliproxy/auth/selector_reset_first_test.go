package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetFirstAuth(id string, resetIn time.Duration, weekly, fiveHour float64) *Auth {
	now := time.Now()
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(resetIn).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weekly, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(fiveHour, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
	}}}
}

func TestResetFirstSelector(t *testing.T) {
	s := &ResetFirstSelector{}
	pick := func(auths ...*Auth) string {
		a, err := s.Pick(context.Background(), "claude", "claude-fable-5-1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	soon := resetFirstAuth("soon", 24*time.Hour, 0.45, 0.10)
	later := resetFirstAuth("later", 6*24*time.Hour, 0.80, 0.10)
	if got := pick(later, soon); got != "soon" {
		t.Fatalf("soonest reset should win, got %s", got)
	}
	spent := resetFirstAuth("spent", 2*time.Hour, 0.99, 0.10)
	if got := pick(spent, later); got != "later" {
		t.Fatalf("exhausted weekly window should lose, got %s", got)
	}
	hot := resetFirstAuth("hot", 24*time.Hour, 0.20, 0.96)
	if got := pick(hot, later); got != "later" {
		t.Fatalf("5h window about to reject should lose, got %s", got)
	}
	rolled := resetFirstAuth("rolled", -time.Hour, 0.99, 0.10)
	if sc := resetFirstSignals(rolled, time.Now()); sc.exhausted || sc.utilization != 0 {
		t.Fatalf("past reset should be treated as fresh: %+v", sc)
	}
	fresh := &Auth{ID: "fresh", Provider: "claude"}
	if got := pick(fresh, soon); got != "soon" {
		t.Fatalf("known window should beat unknown, got %s", got)
	}
	a, b := &Auth{ID: "a", Provider: "codex"}, &Auth{ID: "b", Provider: "codex"}
	first := pick(a, b)
	if second := pick(a, b); first == second {
		t.Fatalf("no-signal providers should round-robin, got %s twice", first)
	}
}
