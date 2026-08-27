// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func at(h int) time.Time { return time.Date(2026, 8, 27, h, 0, 0, 0, time.UTC) }

// TestSortEventsOrdersChronologically is the tool's entire reason to exist:
// Nomad scatters these across four object types with no ordering between them,
// and reading them separately makes it easy to get causality backwards.
func TestSortEventsOrdersChronologically(t *testing.T) {
	e := []event{
		{at: at(12), Kind: "task-event"},
		{at: at(10), Kind: "job-version"},
		{at: at(11), Kind: "evaluation"},
	}

	sortEvents(e)

	require.Equal(t, []string{"job-version", "evaluation", "task-event"}, kindsOf(e))
}

// TestSortEventsBreaksTiesByCausalOrder covers the common case where several
// objects share a timestamp to the second. A job version causes an evaluation
// which causes an allocation, so that is the order they should read in.
func TestSortEventsBreaksTiesByCausalOrder(t *testing.T) {
	e := []event{
		{at: at(10), Kind: "task-event", priority: 4},
		{at: at(10), Kind: "allocation", priority: 3},
		{at: at(10), Kind: "job-version", priority: 0},
		{at: at(10), Kind: "evaluation", priority: 1},
	}

	sortEvents(e)

	require.Equal(t, []string{"job-version", "evaluation", "allocation", "task-event"}, kindsOf(e))
}

// TestSortEventsLeavesUntimedEventsWithoutATime guards the fallback for a
// source that reports no timestamp. Nothing produces one today; the point is
// that such an event is never silently placed at the epoch, which would put it
// decades before everything else and read as though it happened first.
func TestSortEventsLeavesUntimedEventsWithoutATime(t *testing.T) {
	e := []event{
		{at: at(10), Kind: "job-version"},
		{Kind: "mystery"},
	}

	sortEvents(e)

	require.Equal(t, "mystery", e[0].Kind, "untimed events sort ahead of timed ones")
	require.Empty(t, e[0].Time, "an event with no real timestamp must not report one")
	require.Equal(t, "2026-08-27T10:00:00Z", e[1].Time)
}

func TestIsTerminalDeployment(t *testing.T) {
	for _, s := range []string{"successful", "failed", "cancelled"} {
		require.True(t, isTerminalDeployment(s), s)
	}
	// A running or paused deployment has not finished, so it gets no closing
	// event — and its absence is what says the rollout is still open.
	for _, s := range []string{"running", "paused", ""} {
		require.False(t, isTerminalDeployment(s), s)
	}
}

func TestEventDetailPrefersTheMostInformativeField(t *testing.T) {
	require.Equal(t, "display", eventDetail(&api.TaskEvent{
		DisplayMessage: "display", Message: "message", DriverError: "driver",
	}))
	require.Equal(t, "driver", eventDetail(&api.TaskEvent{DriverError: "driver"}))
	require.Equal(t, "vault down", eventDetail(&api.TaskEvent{VaultError: "vault down"}))
	require.Equal(t, "exit code 137", eventDetail(&api.TaskEvent{ExitCode: 137}))
	require.Empty(t, eventDetail(&api.TaskEvent{}))
}

func TestDiffFieldsNamesWhatChanged(t *testing.T) {
	d := &api.JobDiff{
		Fields: []*api.FieldDiff{{Name: "Priority"}},
		TaskGroups: []*api.TaskGroupDiff{{
			Tasks: []*api.TaskDiff{{
				Name:   "web",
				Fields: []*api.FieldDiff{{Name: "Image"}},
			}},
		}},
	}

	require.Equal(t, "Priority, web.Image", diffFields(d))
}

func TestDiffFieldsDeduplicates(t *testing.T) {
	d := &api.JobDiff{Fields: []*api.FieldDiff{{Name: "Image"}, {Name: "Image"}}}
	require.Equal(t, "Image", diffFields(d))
}

func TestReverse(t *testing.T) {
	e := []event{{Kind: "a"}, {Kind: "b"}, {Kind: "c"}}
	reverse(e)
	require.Equal(t, []string{"c", "b", "a"}, kindsOf(e))

	// Odd and even lengths, and the empty case.
	reverse([]event{})
	two := []event{{Kind: "a"}, {Kind: "b"}}
	reverse(two)
	require.Equal(t, []string{"b", "a"}, kindsOf(two))
}

func TestDescribeSpanIgnoresUntimedEvents(t *testing.T) {
	span := describeSpan([]event{
		{Kind: "deployment"},
		{at: at(10)},
		{at: at(14)},
	})

	require.Contains(t, span, "2026-08-27T10:00:00Z")
	require.Contains(t, span, "2026-08-27T14:00:00Z")
}

func TestDescribeSpanEmptyWhenNothingIsTimed(t *testing.T) {
	require.Empty(t, describeSpan([]event{{Kind: "deployment"}}))
	require.Empty(t, describeSpan(nil))
}

func TestTimelineNoteReportsMissingSources(t *testing.T) {
	note := timelineNote(timelineResult{
		Missing: []string{"evaluations (permission denied)"},
		Total:   5, Count: 5,
	}, 60)

	require.Contains(t, note, "incomplete")
	require.Contains(t, note, "permission denied")
	require.Contains(t, note, "unknown")
}

// TestTimelineNoteIsSilentOnACleanTimeline — a complete result needs no
// disclosure, and boilerplate on every call trains a reader to skip the notes
// that do matter.
func TestTimelineNoteIsSilentOnACleanTimeline(t *testing.T) {
	require.Empty(t, timelineNote(timelineResult{Total: 5, Count: 5}, 60))
}

func TestTimelineNoteReportsTrimming(t *testing.T) {
	note := timelineNote(timelineResult{Total: 500, Count: 60}, 60)
	require.Contains(t, note, "500 events exist")
	require.Contains(t, note, "most recent")
}

func TestTimelineNoteExplainsAnEmptyResult(t *testing.T) {
	note := timelineNote(timelineResult{Total: 0}, 60)
	require.Contains(t, note, "list_jobs")
}

func TestNodeLabelFallsBack(t *testing.T) {
	require.Equal(t, "web-1", nodeLabel(&api.AllocationListStub{NodeName: "web-1"}))
	require.Equal(t, "abcdefgh", nodeLabel(&api.AllocationListStub{NodeID: "abcdefgh-1234-5678-9abc-def012345678"}))
	require.Equal(t, "an unknown node", nodeLabel(&api.AllocationListStub{}))
}

func TestDerefU64(t *testing.T) {
	v := uint64(7)
	require.Equal(t, uint64(7), derefU64(&v))
	require.Equal(t, uint64(0), derefU64(nil))
}

func kindsOf(e []event) []string {
	out := make([]string, 0, len(e))
	for _, ev := range e {
		out = append(out, ev.Kind)
	}
	return out
}
