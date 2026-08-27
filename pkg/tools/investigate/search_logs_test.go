// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

func TestCompilePattern(t *testing.T) {
	t.Run("plain text works as a pattern", func(t *testing.T) {
		re, msg := compilePattern("connection refused", false)
		require.Empty(t, msg)
		require.True(t, re.MatchString("error: connection refused"))
	})

	t.Run("case-insensitive by default", func(t *testing.T) {
		re, _ := compilePattern("PANIC", false)
		require.True(t, re.MatchString("a panic occurred"))
	})

	t.Run("case_sensitive is honoured", func(t *testing.T) {
		re, _ := compilePattern("PANIC", true)
		require.False(t, re.MatchString("a panic occurred"))
		require.True(t, re.MatchString("a PANIC occurred"))
	})

	t.Run("alternation", func(t *testing.T) {
		re, _ := compilePattern("(timeout|refused)", false)
		require.True(t, re.MatchString("i/o timeout"))
		require.True(t, re.MatchString("connection refused"))
		require.False(t, re.MatchString("everything is fine"))
	})

	t.Run("a bad expression explains itself", func(t *testing.T) {
		re, msg := compilePattern("what[", false)
		require.Nil(t, re)
		require.Contains(t, msg, "not a valid regular expression")
		// The message must tell the model how to recover, not just that it failed.
		require.Contains(t, msg, "backslash")
	})
}

func TestLeadingTimestamp(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"RFC3339", "2026-08-27T10:00:00Z something happened", true},
		{"RFC3339 with nanos", "2026-08-27T10:00:00.123456789Z hello", true},
		{"RFC3339 with offset", "2026-08-27T10:00:00+05:30 hello", true},
		{"space separated", "2026-08-27 10:00:00 hello", true},
		{"slash dates", "2026/08/27 10:00:00 hello", true},
		{"bracketed", "[2026-08-27T10:00:00Z] hello", true},
		{"no timestamp", "plain log line", false},
		{"level prefix first", "INFO 2026-08-27T10:00:00Z hello", false},
		{"too short", "short", false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := leadingTimestamp(tc.line)
			require.Equal(t, tc.want, ok)
		})
	}
}

func TestLeadingTimestampParsesCorrectInstant(t *testing.T) {
	got, ok := leadingTimestamp("2026-08-27T10:00:00Z the thing")
	require.True(t, ok)
	require.Equal(t, time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), got.UTC())
}

func TestWithinWindow(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 8, 27, h, 0, 0, 0, time.UTC) }
	lo, hi := at(10), at(12)

	require.True(t, withinWindow(at(11), &lo, &hi))
	require.True(t, withinWindow(at(10), &lo, &hi), "the boundary is inclusive")
	require.True(t, withinWindow(at(12), &lo, &hi), "the boundary is inclusive")
	require.False(t, withinWindow(at(9), &lo, &hi))
	require.False(t, withinWindow(at(13), &lo, &hi))

	require.True(t, withinWindow(at(1), nil, nil), "no window means everything matches")
	require.True(t, withinWindow(at(99), &lo, nil))
	require.False(t, withinWindow(at(1), &lo, nil))
}

// TestSelectSearchTargetsPrioritisesFailures is what makes the target cap safe.
// When the cap trims the list, the allocations most likely to explain a
// problem must be the ones that survive.
func TestSelectSearchTargetsPrioritisesFailures(t *testing.T) {
	stubs := []*api.AllocationListStub{
		{ID: "a", ClientStatus: "running"},
		{ID: "b", ClientStatus: "complete"},
		{ID: "c", ClientStatus: "failed"},
		{ID: "d", ClientStatus: "running"},
		{ID: "e", ClientStatus: "lost"},
	}

	got := selectSearchTargets(stubs, nil)
	require.Equal(t, []string{"c", "e", "a", "d", "b"}, idsOf(got))
}

func TestSelectSearchTargetsSkipsPending(t *testing.T) {
	// A pending allocation has no log files at all, so reading it costs a round
	// trip to learn nothing.
	got := selectSearchTargets([]*api.AllocationListStub{
		{ID: "a", ClientStatus: "pending"},
		{ID: "b", ClientStatus: "running"},
	}, nil)
	require.Equal(t, []string{"b"}, idsOf(got))
}

func TestSelectSearchTargetsHonoursStatusFilter(t *testing.T) {
	only := func(s string) bool { return s == "failed" }
	got := selectSearchTargets([]*api.AllocationListStub{
		{ID: "a", ClientStatus: "running"},
		{ID: "b", ClientStatus: "failed"},
	}, only)
	require.Equal(t, []string{"b"}, idsOf(got))
}

// TestTimeFilterNoteIsHonestAboutNoTimestamps is the guard on the claim this
// tool must never make: that a result covers a time range when Nomad gave it
// nothing to filter on.
func TestTimeFilterNoteIsHonestAboutNoTimestamps(t *testing.T) {
	none := timeFilterNote(false)
	require.Contains(t, none, "NO effect")
	require.Contains(t, none, "Do not describe this result as covering a time range")

	some := timeFilterNote(true)
	require.Contains(t, some, "approximate")
}

// TestSearchNoteDoesNotClaimAbsence checks the wording when nothing matched.
// "No match" is not "did not happen", and a model given the first will report
// the second unless told otherwise.
func TestSearchNoteDoesNotClaimAbsence(t *testing.T) {
	note := searchNote(searchResult{AllocsSearched: 5, LinesScanned: 100}, "boom")
	require.Contains(t, note, "not proof")
	require.Contains(t, note, "rotate")
}

func TestSearchNoteFlagsIsolatedAndUniversalFaults(t *testing.T) {
	isolated := searchNote(searchResult{
		TotalMatches: 3, MatchCount: 3, AllocsWithMatch: 1, AllocsSearched: 9,
	}, "boom")
	require.Contains(t, isolated, "the node it is on")

	universal := searchNote(searchResult{
		TotalMatches: 9, MatchCount: 9, AllocsWithMatch: 4, AllocsSearched: 4,
	}, "boom")
	require.Contains(t, universal, "not at any one node")
}

func TestAssembleSearchAggregates(t *testing.T) {
	out := utils.FanOutResult[allocSearch]{
		Visited: 2,
		Items: []allocSearch{
			{matches: []logMatch{{ShortID: "bbb", Line: "x"}}, total: 5, scanned: 100},
			{matches: []logMatch{{ShortID: "aaa", Line: "y"}}, total: 2, scanned: 50},
		},
	}

	got := assembleSearch("web", "default", "boom", 7, nil, nil, out)

	require.Equal(t, 7, got.TotalMatches)
	require.Equal(t, 2, got.MatchCount)
	require.Equal(t, 150, got.LinesScanned)
	require.Equal(t, 2, got.AllocsWithMatch)
	require.Equal(t, 7, got.AllocsTotal)
	require.NotEmpty(t, got.Warning, "log output must always carry the untrusted-content warning")
	// Deterministic ordering between runs.
	require.Equal(t, "aaa", got.Matches[0].ShortID)
}

func TestAssembleSearchOnlyNotesTimeFilterWhenAsked(t *testing.T) {
	out := utils.FanOutResult[allocSearch]{Visited: 1, Items: []allocSearch{{}}}

	require.Empty(t, assembleSearch("w", "default", "p", 1, nil, nil, out).TimeFilterNote)

	now := time.Now()
	require.NotEmpty(t, assembleSearch("w", "default", "p", 1, &now, nil, out).TimeFilterNote)
}

func idsOf(s []*api.AllocationListStub) []string {
	out := make([]string, 0, len(s))
	for _, a := range s {
		out = append(out, a.ID)
	}
	return out
}
