// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// TestMemoryMBRespectsWhatTheDriverMeasured is the guard against reporting
// every task as using no memory. Which fields a driver populates varies by
// platform: cgroups fill Usage and MaxUsage, while several drivers report only
// RSS and leave the rest at zero.
func TestMemoryMBRespectsWhatTheDriverMeasured(t *testing.T) {
	t.Run("cgroups style, Usage measured", func(t *testing.T) {
		mb, ok := memoryMB(&api.MemoryStats{
			Usage: 512 * 1024 * 1024, RSS: 400 * 1024 * 1024,
			Measured: []string{"Usage", "RSS", "MaxUsage"},
		})
		require.True(t, ok)
		require.Equal(t, int64(512), mb)
	})

	t.Run("RSS only, Usage left at zero", func(t *testing.T) {
		// This is the real shape observed on a driver reporting only RSS.
		// Reading Usage unconditionally would report 0 MB.
		mb, ok := memoryMB(&api.MemoryStats{
			RSS: 3 * 1024 * 1024, Usage: 0, Measured: []string{"RSS", "Swap"},
		})
		require.True(t, ok)
		require.Equal(t, int64(3), mb)
	})

	t.Run("nothing measured at all", func(t *testing.T) {
		mb, ok := memoryMB(&api.MemoryStats{})
		require.False(t, ok, "unmeasured must be distinguishable from zero")
		require.Zero(t, mb)
	})

	t.Run("no Measured list falls back to Usage", func(t *testing.T) {
		mb, ok := memoryMB(&api.MemoryStats{Usage: 256 * 1024 * 1024})
		require.True(t, ok)
		require.Equal(t, int64(256), mb)
	})
}

func TestIsOOM(t *testing.T) {
	require.True(t, isOOM(&api.TaskEvent{DisplayMessage: "Task exceeded memory limit: Out of Memory"}))
	require.True(t, isOOM(&api.TaskEvent{Details: map[string]string{"oom_killed": "true"}}))
	// 137 is SIGKILL, which is what the kernel's OOM killer sends.
	require.True(t, isOOM(&api.TaskEvent{ExitCode: 137}))

	require.False(t, isOOM(&api.TaskEvent{DisplayMessage: "Task started by client"}))
	require.False(t, isOOM(&api.TaskEvent{ExitCode: 1}))
	require.False(t, isOOM(&api.TaskEvent{Details: map[string]string{"oom_killed": "false"}}))
}

func TestCountOOMKillsAcrossDeadAllocations(t *testing.T) {
	// The point of scanning terminal allocations: a task that was OOM-killed
	// and rescheduled looks perfectly healthy in its current allocation.
	stubs := []*api.AllocationListStub{
		{
			TaskGroup: "web", ClientStatus: "failed",
			TaskStates: map[string]*api.TaskState{
				"app": {Events: []*api.TaskEvent{{ExitCode: 137}, {Type: "Started"}}},
			},
		},
		{
			TaskGroup: "web", ClientStatus: "running",
			TaskStates: map[string]*api.TaskState{
				"app": {Events: []*api.TaskEvent{{Type: "Started"}}},
			},
		},
	}

	got := countOOMKills(stubs, "")
	require.Equal(t, 1, got[requestedKey{"web", "app"}])
}

func TestCountOOMKillsHonoursGroupFilter(t *testing.T) {
	stubs := []*api.AllocationListStub{
		{TaskGroup: "web", TaskStates: map[string]*api.TaskState{
			"app": {Events: []*api.TaskEvent{{ExitCode: 137}}},
		}},
		{TaskGroup: "batch", TaskStates: map[string]*api.TaskState{
			"job": {Events: []*api.TaskEvent{{ExitCode: 137}}},
		}},
	}

	got := countOOMKills(stubs, "web")
	require.Len(t, got, 1)
	require.Equal(t, 1, got[requestedKey{"web", "app"}])
}

// TestJudgeOOMOutranksEverything is the important ordering. A task killed for
// memory is under-provisioned however comfortable its current sample looks —
// which is precisely the case a single sample would otherwise miss.
func TestJudgeOOMOutranksEverything(t *testing.T) {
	verdict, advice := judge(taskSizing{
		Samples: 3, MemRequested: 512, MemObserved: 10, OOMKills: 2,
	})

	require.Equal(t, "under-provisioned", verdict)
	require.Contains(t, advice, "2 out-of-memory kills")
	require.Contains(t, advice, "whatever the current sample shows")
}

func TestJudgeTight(t *testing.T) {
	verdict, advice := judge(taskSizing{Samples: 1, MemRequested: 512, MemObserved: 500})
	require.Equal(t, "tight", verdict)
	require.Contains(t, advice, "out-of-memory kill on any spike")
}

func TestJudgeOverProvisioned(t *testing.T) {
	verdict, advice := judge(taskSizing{Samples: 2, MemRequested: 2048, MemObserved: 150})
	require.Equal(t, "possibly over-provisioned", verdict)
	// It must hedge: a single sample cannot see a spiky workload's peaks.
	require.Contains(t, advice, "Confirm against a real distribution")
}

func TestJudgeReasonable(t *testing.T) {
	verdict, advice := judge(taskSizing{Samples: 2, MemRequested: 512, MemObserved: 300})
	require.Equal(t, "reasonable", verdict)
	require.Empty(t, advice)
}

func TestJudgeNotMeasured(t *testing.T) {
	t.Run("no running allocation", func(t *testing.T) {
		verdict, advice := judge(taskSizing{Samples: 0, MemRequested: 512})
		require.Equal(t, "not measured", verdict)
		require.Contains(t, advice, "find_problems")
	})

	t.Run("driver reported nothing", func(t *testing.T) {
		// Must not be read as "using zero memory".
		verdict, advice := judge(taskSizing{Samples: 3, MemRequested: 512, MemObserved: 0})
		require.Equal(t, "not measured", verdict)
		require.Contains(t, advice, "did not report memory usage")
	})
}

func TestRequestedResourcesReadsTheSpec(t *testing.T) {
	cpu, mem := 250, 512
	name := "web"
	job := &api.Job{TaskGroups: []*api.TaskGroup{{
		Name:  &name,
		Tasks: []*api.Task{{Name: "app", Resources: &api.Resources{CPU: &cpu, MemoryMB: &mem}}},
	}}}

	got := requestedResources(job, "")
	require.Equal(t, int64(250), got[requestedKey{"web", "app"}].cpuMhz)
	require.Equal(t, int64(512), got[requestedKey{"web", "app"}].memMB)

	require.Empty(t, requestedResources(job, "other"), "the group filter is honoured")
}

func TestCombineTakesTheMaximumAcrossAllocations(t *testing.T) {
	requested := map[requestedKey]taskSample{{"web", "app"}: {cpuMhz: 500, memMB: 1024}}
	samples := []sample{
		{group: "web", tasks: map[string]taskSample{"app": {cpuMhz: 100, memMB: 200, measured: true}}},
		{group: "web", tasks: map[string]taskSample{"app": {cpuMhz: 300, memMB: 900, measured: true}}},
	}

	got := combine(requested, samples, nil)

	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Samples)
	// The worst case across allocations is what matters for sizing, not the
	// average: one hot replica is the one that gets killed.
	require.Equal(t, int64(300), got[0].CPUObserved)
	require.Equal(t, int64(900), got[0].MemObserved)
}

func TestCombineIncludesTasksWithNoRunningAllocation(t *testing.T) {
	// Silently omitting these would read as nothing to report.
	requested := map[requestedKey]taskSample{{"web", "app"}: {memMB: 512}}

	got := combine(requested, nil, nil)

	require.Len(t, got, 1)
	require.Equal(t, "not measured", got[0].Verdict)
	require.Zero(t, got[0].Samples)
}

func TestCombineIncludesOOMOnlyTasks(t *testing.T) {
	// A task with no spec entry and no live sample, but a history of kills,
	// still has to appear.
	got := combine(nil, nil, map[requestedKey]int{{"web", "gone"}: 3})

	require.Len(t, got, 1)
	require.Equal(t, 3, got[0].OOMKills)
	require.Equal(t, "under-provisioned", got[0].Verdict)
}

func TestSizingNoteLeadsWithOOM(t *testing.T) {
	note := sizingNote(sizingResult{Running: 2, OOMKills: 4})
	require.Contains(t, note, "4 out-of-memory kills")
	require.Contains(t, note, "outrank every other reading")
}

func TestSizingNoteWithNoRunningAllocations(t *testing.T) {
	note := sizingNote(sizingResult{Running: 0})
	require.Contains(t, note, "no running allocations")
}

func TestSizingNoteCountsVerdicts(t *testing.T) {
	note := sizingNote(sizingResult{
		Running: 3,
		Tasks: []taskSizing{
			{Verdict: "possibly over-provisioned"},
			{Verdict: "tight"},
			{Verdict: "reasonable"},
		},
	})
	require.Contains(t, note, "1 task sitting close to its memory limit")
	require.Contains(t, note, "1 task may be over-provisioned")
	require.Contains(t, note, "get_cluster_capacity")
}

func TestSizingNoteWhenNothingIsWrong(t *testing.T) {
	note := sizingNote(sizingResult{Running: 2, Tasks: []taskSizing{{Verdict: "reasonable"}}})
	require.Contains(t, note, "Nothing looks obviously mis-sized")
}
