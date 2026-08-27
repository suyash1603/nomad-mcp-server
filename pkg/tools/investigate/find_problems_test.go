// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSortFindingsRanksBySeverityThenSize is the property the whole tool rests
// on: the first finding should be the most likely answer, so a model that reads
// only the top of the list still reads the right thing.
func TestSortFindingsRanksBySeverityThenSize(t *testing.T) {
	f := []finding{
		{sev: sevWarning, Category: "nodes-draining", Count: 50},
		{sev: sevCritical, Category: "failed-allocations", Count: 2},
		{sev: sevCritical, Category: "blocked-evaluations", Count: 9},
		{sev: sevInfo, Category: "whatever", Count: 100},
	}

	sortFindings(f)

	require.Equal(t, "blocked-evaluations", f[0].Category, "critical and largest comes first")
	require.Equal(t, "failed-allocations", f[1].Category)
	require.Equal(t, "nodes-draining", f[2].Category, "a warning outranks info regardless of count")
	require.Equal(t, "whatever", f[3].Category)
}

func TestSortFindingsIsStableForEqualRank(t *testing.T) {
	build := func() []finding {
		return []finding{
			{sev: sevCritical, Category: "zebra", Count: 3},
			{sev: sevCritical, Category: "alpha", Count: 3},
		}
	}

	first, second := build(), build()
	sortFindings(first)
	sortFindings(second)

	// Ties break on category, so two identical scans cannot disagree about
	// order — which would otherwise make two runs look like different findings.
	require.Equal(t, "alpha", first[0].Category)
	require.Equal(t, first, second)
}

func TestSortFindingsPopulatesSeverityString(t *testing.T) {
	f := []finding{{sev: sevCritical}, {sev: sevWarning}, {sev: sevInfo}}
	sortFindings(f)
	require.Equal(t, "critical", f[0].Severity)
	require.Equal(t, "warning", f[1].Severity)
	require.Equal(t, "info", f[2].Severity)
}

// TestProblemsNoteNeverCallsAFailedScanHealthy is the important negative case.
// A check that could not run leaves its area unknown, and reporting that as
// healthy is the worst thing this tool could do.
func TestProblemsNoteNeverCallsAFailedScanHealthy(t *testing.T) {
	note := problemsNote(problemsResult{ChecksRun: 5, ChecksFailed: 2}, "default")
	require.Contains(t, note, "UNKNOWN, not healthy")
	require.NotContains(t, note, "No problems found")
}

func TestProblemsNoteOnCleanScan(t *testing.T) {
	note := problemsNote(problemsResult{ChecksRun: 5, Count: 0}, "*")
	require.Contains(t, note, "No problems found in every namespace")
	// A clean scan is current state, not history — say so, or "nothing is
	// wrong" gets read as "nothing went wrong".
	require.Contains(t, note, "current state only")
	require.Contains(t, note, "build_job_timeline")
}

func TestProblemsNoteExplainsRanking(t *testing.T) {
	note := problemsNote(problemsResult{ChecksRun: 5, Count: 3}, "prod")
	require.Contains(t, note, "namespace prod")
	require.Contains(t, note, "consequences of the first")
}

func TestPluralAndHas(t *testing.T) {
	require.Equal(t, "", plural(1))
	require.Equal(t, "s", plural(0))
	require.Equal(t, "s", plural(2))

	// "1 job has" / "2 jobs have"
	require.Equal(t, "s", has(1))
	require.Equal(t, "ve", has(2))
}

func TestFirstN(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, firstN([]string{"a", "b", "c"}, 2))
	require.Equal(t, []string{"a"}, firstN([]string{"a"}, 5))
	require.Empty(t, firstN(nil, 3))
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]bool{"c": true, "a": true, "b": true}, 2)
	require.Equal(t, []string{"a", "b"}, got, "deterministic and capped")
}

func TestSeverityString(t *testing.T) {
	require.Equal(t, "critical", sevCritical.String())
	require.Equal(t, "warning", sevWarning.String())
	require.Equal(t, "info", sevInfo.String())
}
