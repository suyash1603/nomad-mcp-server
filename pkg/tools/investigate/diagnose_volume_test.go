// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// healthyVolume is a volume with nothing wrong, as a base for the cases below.
func healthyVolume() *api.CSIVolume {
	return &api.CSIVolume{
		ID:                  "vol-1",
		PluginID:            "ebs",
		Schedulable:         true,
		NodesHealthy:        3,
		NodesExpected:       3,
		ControllersHealthy:  1,
		ControllersExpected: 1,
		ControllerRequired:  true,
	}
}

// TestStaleClaimFromMissingAllocation is the case this tool exists for. The
// claim map still holds an allocation that is gone entirely, and nothing in
// read_volume makes that visible.
func TestStaleClaimFromMissingAllocation(t *testing.T) {
	vol := healthyVolume()
	vol.WriteAllocs = map[string]*api.Allocation{"dead-alloc-id-1234": nil}
	// Allocations is empty: the allocation no longer exists at all.

	claims, count := collectClaims(vol)

	require.Equal(t, 1, count)
	require.True(t, claims[0].Stale)
	require.Equal(t, "gone", claims[0].Status)

	found := volumeFindings(vol, claims)
	require.Equal(t, "stale-claims", found[0].Category)
	require.Equal(t, sevCritical, found[0].sev)
	require.Contains(t, found[0].Detail, "sit pending forever")
}

// TestStaleClaimFromTerminalAllocation covers the other shape: the allocation
// still exists but has stopped, so it will never release the claim itself.
func TestStaleClaimFromTerminalAllocation(t *testing.T) {
	for _, status := range []string{"complete", "failed", "lost"} {
		t.Run(status, func(t *testing.T) {
			vol := healthyVolume()
			vol.WriteAllocs = map[string]*api.Allocation{"a1": nil}
			vol.Allocations = []*api.AllocationListStub{
				{ID: "a1", ClientStatus: status, JobID: "web", NodeName: "n1"},
			}

			claims, _ := collectClaims(vol)
			require.True(t, claims[0].Stale, "%s is terminal and must read as stale", status)
			require.Equal(t, "web", claims[0].Job)
		})
	}
}

func TestLiveClaimIsNotStale(t *testing.T) {
	vol := healthyVolume()
	vol.WriteAllocs = map[string]*api.Allocation{"a1": nil}
	vol.Allocations = []*api.AllocationListStub{
		{ID: "a1", ClientStatus: "running", JobID: "web", NodeName: "n1"},
	}

	claims, count := collectClaims(vol)

	require.Equal(t, 1, count)
	require.False(t, claims[0].Stale)
	require.Empty(t, volumeFindings(vol, claims), "a healthy volume with a live claim has no findings")
}

func TestClaimsSortStaleFirst(t *testing.T) {
	vol := healthyVolume()
	vol.ReadAllocs = map[string]*api.Allocation{"live": nil}
	vol.WriteAllocs = map[string]*api.Allocation{"dead": nil}
	vol.Allocations = []*api.AllocationListStub{{ID: "live", ClientStatus: "running"}}

	claims, _ := collectClaims(vol)

	require.True(t, claims[0].Stale, "the stale claim is the interesting one, so it comes first")
	require.Equal(t, "write", claims[0].Mode)
	require.Equal(t, "read", claims[1].Mode)
}

func TestVolumeFindingsUnschedulable(t *testing.T) {
	vol := healthyVolume()
	vol.Schedulable = false

	found := volumeFindings(vol, nil)

	require.Equal(t, "not-schedulable", found[0].Category)
	// The point worth making: the resulting placement error says nothing about
	// storage, which is why this is hard to find.
	require.Contains(t, found[0].Detail, "does not mention storage")
}

func TestVolumeFindingsNoNodePlugin(t *testing.T) {
	vol := healthyVolume()
	vol.NodesHealthy, vol.NodesExpected = 0, 0

	found := volumeFindings(vol, nil)

	require.Equal(t, "no-node-plugin", found[0].Category)
	require.Equal(t, sevCritical, found[0].sev)
}

func TestVolumeFindingsDegradedNodes(t *testing.T) {
	vol := healthyVolume()
	vol.NodesHealthy = 1 // of 3

	found := volumeFindings(vol, nil)

	require.Equal(t, "degraded-plugin-nodes", found[0].Category)
	require.Equal(t, 2, found[0].Count, "the count is how many are missing")
	require.Equal(t, sevWarning, found[0].sev)
}

func TestVolumeFindingsNoHealthyController(t *testing.T) {
	vol := healthyVolume()
	vol.ControllersHealthy = 0

	categories := categoriesOf(volumeFindings(vol, nil))
	require.Contains(t, categories, "no-healthy-controller")
}

func TestVolumeFindingsConflictingWriters(t *testing.T) {
	vol := healthyVolume()
	vol.AccessMode = api.CSIVolumeAccessMode("single-node-writer")
	vol.WriteAllocs = map[string]*api.Allocation{"a1": nil, "a2": nil}
	vol.Allocations = []*api.AllocationListStub{
		{ID: "a1", ClientStatus: "running"}, {ID: "a2", ClientStatus: "running"},
	}

	claims, _ := collectClaims(vol)
	categories := categoriesOf(volumeFindings(vol, claims))
	require.Contains(t, categories, "conflicting-claims")
}

func TestVolumeFindingsHealthyVolumeHasNone(t *testing.T) {
	require.Empty(t, volumeFindings(healthyVolume(), nil))
}

func TestVolumeNoteOnAHealthyVolumeRedirects(t *testing.T) {
	note := volumeNote(volumeDiagnosis{ClaimCount: 2}, healthyVolume())

	require.Contains(t, note, "No problem found")
	// Not finding a problem here is itself a useful result, but only if it
	// says where to look instead.
	require.Contains(t, note, "read_evaluation")
}

func TestVolumeNoteNamesTheAccessMode(t *testing.T) {
	vol := healthyVolume()
	vol.AccessMode = api.CSIVolumeAccessMode("single-node-writer")

	note := volumeNote(volumeDiagnosis{Findings: []finding{{Category: "x"}}}, vol)
	require.Contains(t, note, "single-node-writer")
}

func TestIsTerminalAlloc(t *testing.T) {
	for _, s := range []string{"complete", "failed", "lost"} {
		require.True(t, isTerminalAlloc(s), s)
	}
	for _, s := range []string{"running", "pending", ""} {
		require.False(t, isTerminalAlloc(s), s)
	}
}

func TestRatio(t *testing.T) {
	require.Equal(t, "2/3 healthy", ratio(2, 3))
	require.Equal(t, "none registered", ratio(0, 0))
}

func categoriesOf(f []finding) []string {
	out := make([]string, 0, len(f))
	for _, x := range f {
		out = append(out, x.Category)
	}
	return out
}
