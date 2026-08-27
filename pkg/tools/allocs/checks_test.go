// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package allocs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckRankPutsFailuresFirst(t *testing.T) {
	require.Less(t, checkRank("failure"), checkRank("pending"))
	require.Less(t, checkRank("pending"), checkRank("success"))
	require.Equal(t, checkRank("success"), checkRank("passing"))
	// Anything unrecognised is treated as a failure rather than assumed fine.
	require.Equal(t, checkRank("failure"), checkRank("something-new"))
}

// TestChecksNoteOnNoChecks is the important one. No checks and no failing
// checks look identical in the data and mean opposite things, so the note has
// to say which it is.
func TestChecksNoteOnNoChecks(t *testing.T) {
	note := checksNote(checksResult{Count: 0})

	require.Contains(t, note, "does NOT mean it")
	require.Contains(t, note, "nothing is checking")
	// The most common real reason, which is invisible from Nomad's side.
	require.Contains(t, note, "consul")
}

func TestChecksNoteOnFailures(t *testing.T) {
	note := checksNote(checksResult{Count: 3, Failing: 2, Passing: 1})

	require.Contains(t, note, "2 of 3 checks are failing")
	// The distinction that makes this tool worth having: running is not healthy.
	require.Contains(t, note, "The allocation is running")
	require.Contains(t, note, "deployment stuck")
}

func TestChecksNoteOnPending(t *testing.T) {
	note := checksNote(checksResult{Count: 2, Pending: 2})
	require.Contains(t, note, "not reported yet")
	require.Contains(t, note, "grace period")
}

func TestChecksNoteOnAllPassing(t *testing.T) {
	require.Equal(t, "All 4 checks are passing.", checksNote(checksResult{Count: 4, Passing: 4}))
}
