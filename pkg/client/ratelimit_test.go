// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// callLimited runs one tool call through the limiter.
func callLimited(t *testing.T, r *RateLimiter, ctx context.Context) (*mcp.CallToolResult, bool) {
	t.Helper()

	reached := false
	handler := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reached = true
		return mcp.NewToolResultText("ok"), nil
	}

	var req mcp.CallToolRequest
	req.Params.Name = "list_jobs"

	res, err := r.Middleware()(handler)(ctx, req)
	require.NoError(t, err, "throttling must be reported as a tool result, not a Go error")
	return res, reached
}

func TestParseRateLimits(t *testing.T) {
	cfg := &config.Config{RateLimitGlobal: "25:50", RateLimitSession: "7:14"}
	got := ParseRateLimits(cfg, quietLogger())

	require.Equal(t, rate.Limit(25), got.GlobalLimit)
	require.Equal(t, 50, got.GlobalBurst)
	require.Equal(t, rate.Limit(7), got.PerSessionLimit)
	require.Equal(t, 14, got.PerSessionBurst)
}

func TestParseRateLimitsFallsBackOnGarbage(t *testing.T) {
	cfg := &config.Config{RateLimitGlobal: "nonsense", RateLimitSession: "also:bad"}
	got := ParseRateLimits(cfg, quietLogger())

	require.Equal(t, DefaultRateLimitConfig(), got,
		"an unparseable limit must fall back to the default rather than disabling limiting")
}

func TestParseRateLimitsEmptyUsesDefaults(t *testing.T) {
	require.Equal(t, DefaultRateLimitConfig(), ParseRateLimits(&config.Config{}, quietLogger()))
}

func TestDefaultsMatchDocumentedValues(t *testing.T) {
	d := DefaultRateLimitConfig()
	require.Equal(t, rate.Limit(10), d.GlobalLimit, "documented default is 10:20")
	require.Equal(t, 20, d.GlobalBurst)
	require.Equal(t, rate.Limit(5), d.PerSessionLimit, "documented default is 5:10")
	require.Equal(t, 10, d.PerSessionBurst)
}

// TestGlobalLimitBlocksBeyondBurst drives the limiter past its burst with a
// zero refill rate, so the outcome does not depend on wall-clock timing.
func TestGlobalLimitBlocksBeyondBurst(t *testing.T) {
	r := NewRateLimiter(RateLimitConfig{
		GlobalLimit:     0, // never refills
		GlobalBurst:     2,
		PerSessionLimit: 1000,
		PerSessionBurst: 1000,
	}, quietLogger())

	for i := 0; i < 2; i++ {
		_, reached := callLimited(t, r, context.Background())
		require.True(t, reached, "call %d should be within the burst", i+1)
	}

	res, reached := callLimited(t, r, context.Background())
	require.False(t, reached, "the third call must be throttled")
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "Rate limit exceeded")
}

// TestThrottleMessageDiscouragesTightRetries: a model that retries immediately
// is exactly what tripped the limit.
func TestThrottleMessageDiscouragesTightRetries(t *testing.T) {
	r := NewRateLimiter(RateLimitConfig{
		GlobalLimit: 0, GlobalBurst: 1,
		PerSessionLimit: 1000, PerSessionBurst: 1000,
	}, quietLogger())

	_, _ = callLimited(t, r, context.Background())
	res, _ := callLimited(t, r, context.Background())

	msg := resultText(t, res)
	require.Contains(t, msg, "tight loop")
	require.Contains(t, msg, "Wait a moment")
}

func TestSessionLimiterIsPerSession(t *testing.T) {
	r := NewRateLimiter(RateLimitConfig{
		GlobalLimit: 1000, GlobalBurst: 1000,
		PerSessionLimit: 0, PerSessionBurst: 1,
	}, quietLogger())

	a := r.sessionLimiter("session-a")
	b := r.sessionLimiter("session-b")
	require.NotSame(t, a, b, "each session gets its own budget")

	require.Same(t, a, r.sessionLimiter("session-a"), "the limiter must be stable per session")

	require.True(t, a.Allow())
	require.False(t, a.Allow(), "session-a has spent its burst")
	require.True(t, b.Allow(), "session-b must be unaffected by session-a")
}

func TestEndSessionDropsLimiter(t *testing.T) {
	r := NewRateLimiter(DefaultRateLimitConfig(), quietLogger())

	first := r.sessionLimiter("session-a")
	r.EndSession("session-a")
	second := r.sessionLimiter("session-a")

	require.NotSame(t, first, second, "a finished session must not leak its limiter")
	require.NotPanics(t, func() { r.EndSession("never-existed") })
}

func TestNoSessionStillAppliesGlobalLimit(t *testing.T) {
	r := NewRateLimiter(RateLimitConfig{
		GlobalLimit: 0, GlobalBurst: 1,
		PerSessionLimit: 1000, PerSessionBurst: 1000,
	}, quietLogger())

	_, reached := callLimited(t, r, context.Background())
	require.True(t, reached)

	_, reached = callLimited(t, r, context.Background())
	require.False(t, reached, "the global limit applies even without a session")
}

func TestSessionLimiterIsConcurrencySafe(t *testing.T) {
	r := NewRateLimiter(DefaultRateLimitConfig(), quietLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			r.sessionLimiter("shared")
		}
	}()
	for i := 0; i < 500; i++ {
		r.sessionLimiter("shared")
	}
	<-done
}
