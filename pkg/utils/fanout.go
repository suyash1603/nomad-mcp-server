// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fan-out defaults.
//
// These bound a tool that visits many allocations or nodes in one call. All
// three matter on a large cluster for different reasons: concurrency protects
// the Nomad servers from a thundering herd, the target cap protects the model's
// context, and the budget protects the client from a call that never returns
// because forty nodes are unreachable.
const (
	// DefaultFanOutConcurrency is how many requests run at once. Nomad's API
	// servers answer these from Raft state, and a burst of hundreds is a
	// self-inflicted denial of service on the cluster being diagnosed.
	DefaultFanOutConcurrency = 8

	// DefaultFanOutMaxTargets caps how many targets are visited at all. Beyond
	// this the fan-out samples and says so, because a partial answer that
	// admits it is partial is worth more than a complete one that arrives after
	// the client has given up.
	DefaultFanOutMaxTargets = 200

	// DefaultFanOutBudget bounds the whole fan-out in wall-clock time.
	DefaultFanOutBudget = 30 * time.Second

	// maxReportedErrors caps the distinct error messages returned. Failures
	// during a fan-out are usually the same failure repeated, and returning
	// three hundred copies of "connection refused" is how a diagnostic tool
	// exhausts the context it was supposed to save.
	maxReportedErrors = 5
)

// FanOutLimits bounds one fan-out. A zero value means the defaults above.
type FanOutLimits struct {
	// Concurrency is how many targets are visited at once.
	Concurrency int

	// MaxTargets is how many targets are visited at all. Targets beyond it are
	// not attempted, and the result records that it sampled.
	MaxTargets int

	// Budget is the wall-clock limit for the whole fan-out.
	Budget time.Duration
}

// withDefaults fills in the zero fields.
func (l FanOutLimits) withDefaults() FanOutLimits {
	if l.Concurrency <= 0 {
		l.Concurrency = DefaultFanOutConcurrency
	}
	if l.MaxTargets <= 0 {
		l.MaxTargets = DefaultFanOutMaxTargets
	}
	if l.Budget <= 0 {
		l.Budget = DefaultFanOutBudget
	}
	return l
}

// FanOutResult is the outcome of a fan-out.
//
// It reports what was not done as prominently as what was, because every field
// here that goes unreported is a way for a model to state a partial finding as
// a complete one — "no allocation is failing" when sixty were never checked.
type FanOutResult[R any] struct {
	// Items holds one entry per target that succeeded, in the order the
	// targets were given rather than the order they finished.
	Items []R `json:"items"`

	// Requested is how many targets were passed in.
	Requested int `json:"requested"`

	// Visited is how many were actually attempted. It is less than Requested
	// when the target cap sampled, or when the budget expired part-way.
	Visited int `json:"visited"`

	// Failed is how many attempted targets returned an error.
	Failed int `json:"failed,omitempty"`

	// Errors holds the distinct failures, most frequent first, capped and each
	// carrying the number of targets it affected.
	Errors []FanOutError `json:"errors,omitempty"`

	// Sampled is true when the target cap stopped this short of every target.
	Sampled bool `json:"sampled,omitempty"`

	// TimedOut is true when the budget expired before every attempted target
	// finished.
	TimedOut bool `json:"timed_out,omitempty"`

	// Note states any of the above in words. Empty when the fan-out was
	// complete and everything succeeded.
	Note string `json:"note,omitempty"`
}

// FanOutError is one distinct failure and how many targets hit it.
type FanOutError struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// FanOut applies fn to each target, bounded by limits, and collects the
// results.
//
// fn is called concurrently and must be safe to call from several goroutines.
// A target whose fn returns an error contributes no item; the error is counted
// and folded into Errors rather than aborting the fan-out, because on a large
// cluster some fraction of nodes is always unreachable and that must not cost
// the caller every other answer.
//
// The returned context error is not propagated: a fan-out that runs out of
// budget has partial results worth returning, and TimedOut says so.
func FanOut[T, R any](
	ctx context.Context,
	targets []T,
	limits FanOutLimits,
	fn func(context.Context, T) (R, error),
) FanOutResult[R] {
	limits = limits.withDefaults()

	res := FanOutResult[R]{Requested: len(targets)}
	if len(targets) == 0 {
		return res
	}

	visit := targets
	if len(visit) > limits.MaxTargets {
		visit = visit[:limits.MaxTargets]
		res.Sampled = true
	}

	ctx, cancel := context.WithTimeout(ctx, limits.Budget)
	defer cancel()

	// Results are written into a slice indexed by target so the output order
	// follows the input rather than whichever request happened to finish first.
	// A tool whose output reorders itself between identical calls is much
	// harder to reason about, for a person and for a model comparing runs.
	type slot struct {
		item R
		ok   bool
		err  string
	}
	slots := make([]slot, len(visit))

	sem := make(chan struct{}, limits.Concurrency)
	var wg sync.WaitGroup

	for i, target := range visit {
		// Stop dispatching once the budget has gone. Work already in flight is
		// still waited for: its context is cancelled, so it returns promptly.
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(i int, target T) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			item, err := fn(ctx, target)
			if err != nil {
				slots[i] = slot{err: err.Error()}
				return
			}
			slots[i] = slot{item: item, ok: true}
		}(i, target)
	}

	wg.Wait()

	counts := map[string]int{}
	for _, s := range slots {
		switch {
		case s.ok:
			res.Items = append(res.Items, s.item)
			res.Visited++
		case s.err != "":
			res.Failed++
			res.Visited++
			counts[s.err]++
		}
	}

	res.TimedOut = ctx.Err() != nil
	res.Errors = topErrors(counts)
	res.Note = res.describe()
	return res
}

// topErrors returns the most frequent distinct errors, capped.
func topErrors(counts map[string]int) []FanOutError {
	if len(counts) == 0 {
		return nil
	}

	out := make([]FanOutError, 0, len(counts))
	for msg, n := range counts {
		out = append(out, FanOutError{Message: msg, Count: n})
	}

	// Most frequent first, then alphabetically so the output is deterministic
	// when two errors tie.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Message < out[j].Message
	})

	if len(out) > maxReportedErrors {
		out = out[:maxReportedErrors]
	}
	return out
}

// describe states an incomplete fan-out in words.
//
// The structured fields say the same thing, but a model reading a result with
// items in it tends to answer from the items and skip the metadata. Saying
// "this is not the whole picture" in a sentence is what stops a sampled scan
// being reported as an exhaustive one.
func (r FanOutResult[R]) describe() string {
	var parts []string

	if r.Sampled {
		parts = append(parts,
			"Only "+itoa(r.Visited)+" of "+itoa(r.Requested)+
				" targets were checked, because the fan-out limit stopped it there. "+
				"This result is a sample: do not describe it as covering everything.")
	}
	if r.TimedOut {
		parts = append(parts,
			"The time budget expired before every target was checked, so results may be incomplete.")
	}
	if r.Failed > 0 {
		parts = append(parts,
			itoa(r.Failed)+" of "+itoa(r.Visited)+
				" targets could not be reached; see the errors field. Those targets are unknown, not healthy.")
	}

	return strings.Join(parts, " ")
}
