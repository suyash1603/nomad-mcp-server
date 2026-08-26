// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanOutCollectsInInputOrder(t *testing.T) {
	// Later targets finish first, so a result that followed completion order
	// would come back reversed. Stable ordering is what lets two runs of the
	// same tool be compared.
	targets := []int{1, 2, 3, 4, 5}

	res := FanOut(context.Background(), targets, FanOutLimits{Concurrency: 5},
		func(_ context.Context, n int) (int, error) {
			time.Sleep(time.Duration(len(targets)-n) * 10 * time.Millisecond)
			return n * 10, nil
		})

	require.Equal(t, []int{10, 20, 30, 40, 50}, res.Items)
	require.Equal(t, 5, res.Requested)
	require.Equal(t, 5, res.Visited)
	require.Zero(t, res.Failed)
	require.False(t, res.Sampled)
	require.False(t, res.TimedOut)
	require.Empty(t, res.Note, "a complete, clean fan-out needs no disclosure")
}

func TestFanOutEmptyTargets(t *testing.T) {
	res := FanOut(context.Background(), nil, FanOutLimits{},
		func(_ context.Context, n int) (int, error) { return n, nil })

	require.Empty(t, res.Items)
	require.Zero(t, res.Requested)
	require.Zero(t, res.Visited)
	require.Empty(t, res.Note)
}

func TestFanOutRespectsConcurrency(t *testing.T) {
	var mu sync.Mutex
	var inFlight, peak int

	targets := make([]int, 50)
	res := FanOut(context.Background(), targets, FanOutLimits{Concurrency: 4},
		func(_ context.Context, _ int) (int, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return 0, nil
		})

	require.Len(t, res.Items, 50)
	require.LessOrEqual(t, peak, 4, "more requests ran at once than the limit allows")
}

func TestFanOutSamplesBeyondMaxTargets(t *testing.T) {
	var called atomic.Int32

	targets := make([]int, 100)
	res := FanOut(context.Background(), targets, FanOutLimits{MaxTargets: 10},
		func(_ context.Context, _ int) (int, error) {
			called.Add(1)
			return 1, nil
		})

	require.Equal(t, int32(10), called.Load(), "targets beyond the cap must not be attempted")
	require.Equal(t, 100, res.Requested)
	require.Equal(t, 10, res.Visited)
	require.True(t, res.Sampled)
	require.Contains(t, res.Note, "sample")
	require.Contains(t, res.Note, "10 of 100")
}

func TestFanOutPartialFailuresDoNotAbort(t *testing.T) {
	res := FanOut(context.Background(), []int{1, 2, 3, 4}, FanOutLimits{},
		func(_ context.Context, n int) (string, error) {
			if n%2 == 0 {
				return "", errors.New("connection refused")
			}
			return fmt.Sprintf("ok-%d", n), nil
		})

	require.Equal(t, []string{"ok-1", "ok-3"}, res.Items)
	require.Equal(t, 4, res.Visited)
	require.Equal(t, 2, res.Failed)
	require.Len(t, res.Errors, 1, "identical failures collapse into one entry")
	require.Equal(t, "connection refused", res.Errors[0].Message)
	require.Equal(t, 2, res.Errors[0].Count)
	require.Contains(t, res.Note, "could not be reached")
	require.Contains(t, res.Note, "unknown, not healthy")
}

func TestFanOutCapsDistinctErrors(t *testing.T) {
	targets := make([]int, 40)
	for i := range targets {
		targets[i] = i
	}

	res := FanOut(context.Background(), targets, FanOutLimits{},
		func(_ context.Context, n int) (int, error) {
			return 0, fmt.Errorf("failure number %d", n)
		})

	require.Equal(t, 40, res.Failed)
	require.Len(t, res.Errors, maxReportedErrors,
		"forty distinct errors must not all land in the model's context")
}

func TestFanOutErrorsRankedByFrequency(t *testing.T) {
	res := FanOut(context.Background(), []int{1, 2, 3, 4, 5}, FanOutLimits{},
		func(_ context.Context, n int) (int, error) {
			if n <= 3 {
				return 0, errors.New("common")
			}
			return 0, errors.New("rare")
		})

	require.Len(t, res.Errors, 2)
	require.Equal(t, "common", res.Errors[0].Message)
	require.Equal(t, 3, res.Errors[0].Count)
	require.Equal(t, "rare", res.Errors[1].Message)
}

func TestFanOutBudgetStopsAndReports(t *testing.T) {
	targets := make([]int, 40)

	start := time.Now()
	res := FanOut(context.Background(), targets,
		FanOutLimits{Concurrency: 2, Budget: 80 * time.Millisecond},
		func(ctx context.Context, _ int) (int, error) {
			select {
			case <-time.After(2 * time.Second):
				return 1, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		})

	require.True(t, res.TimedOut)
	require.Less(t, time.Since(start), 2*time.Second, "the budget must actually cut the work short")
	require.Contains(t, res.Note, "budget expired")
	require.Less(t, res.Visited, res.Requested)
}

func TestFanOutHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := FanOut(ctx, make([]int, 20), FanOutLimits{},
		func(_ context.Context, _ int) (int, error) { return 1, nil })

	require.True(t, res.TimedOut)
	require.Empty(t, res.Items)
}

func TestFanOutLimitDefaults(t *testing.T) {
	got := FanOutLimits{}.withDefaults()
	require.Equal(t, DefaultFanOutConcurrency, got.Concurrency)
	require.Equal(t, DefaultFanOutMaxTargets, got.MaxTargets)
	require.Equal(t, DefaultFanOutBudget, got.Budget)

	// Explicit values survive.
	got = FanOutLimits{Concurrency: 2, MaxTargets: 3, Budget: time.Second}.withDefaults()
	require.Equal(t, 2, got.Concurrency)
	require.Equal(t, 3, got.MaxTargets)
	require.Equal(t, time.Second, got.Budget)
}
