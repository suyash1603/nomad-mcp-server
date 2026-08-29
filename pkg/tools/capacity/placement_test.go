// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestRejectsReportsStateBeforeSize(t *testing.T) {
	// A node that is down should be reported as down rather than as short of
	// memory: the two need completely different fixes.
	down := node("a", 100, 100, 100, 100)
	down.Status = "down"
	require.Equal(t, "node is down", rejects(down, nil, "", 500, 500))

	draining := node("a", 100, 100, 100, 100)
	draining.Draining = true
	require.Equal(t, "node is draining", rejects(draining, nil, "", 500, 500))

	ineligible := node("a", 100, 100, 100, 100)
	ineligible.Eligible = false
	require.Contains(t, rejects(ineligible, nil, "", 500, 500), "ineligible")
}

func TestRejectsOnDatacenter(t *testing.T) {
	n := node("a", 1000, 1000, 0, 0)
	n.Datacenter = "dc1"

	require.Contains(t, rejects(n, []string{"dc2"}, "", 10, 10), "not in the job's datacenters")
	require.Empty(t, rejects(n, []string{"dc1", "dc2"}, "", 10, 10))
	require.Empty(t, rejects(n, nil, "", 10, 10), "no datacenters means no restriction")
}

func TestRejectsOnNodePool(t *testing.T) {
	n := node("a", 1000, 1000, 0, 0)
	n.Pool = "default"

	require.Contains(t, rejects(n, nil, "gpu", 10, 10), "node pool")
	require.Empty(t, rejects(n, nil, "default", 10, 10))
	// "all" is Nomad's wildcard pool.
	require.Empty(t, rejects(n, nil, "all", 10, 10))
}

func TestRejectsOnResources(t *testing.T) {
	n := node("a", 1000, 1024, 900, 900)

	require.Contains(t, rejects(n, nil, "", 500, 10), "insufficient CPU")
	require.Contains(t, rejects(n, nil, "", 10, 500), "insufficient memory")
	require.Empty(t, rejects(n, nil, "", 50, 50))

	// The message has to carry both numbers, or "not enough" is unactionable.
	msg := rejects(n, nil, "", 500, 10)
	require.Contains(t, msg, "100 MHz free")
	require.Contains(t, msg, "needs 500")
}

func TestMatchesAnySupportsWildcards(t *testing.T) {
	require.True(t, matchesAny("dc1", []string{"*"}))
	require.True(t, matchesAny("dc1", []string{"dc1"}))
	require.True(t, matchesAny("eu-west-1", []string{"eu-west-*"}))
	require.False(t, matchesAny("us-east-1", []string{"eu-west-*"}))
	require.False(t, matchesAny("dc1", []string{"dc2"}))
	require.False(t, matchesAny("dc1", nil))
}

// TestAllocationsThatFitPacksSeveralPerNode is the bug this replaced. Nomad
// packs several allocations of a group onto one node, so counting nodes and
// counting allocations give different — and on a small cluster, opposite —
// answers.
func TestAllocationsThatFitPacksSeveralPerNode(t *testing.T) {
	n := node("a", 1000, 1024, 0, 0)

	require.Equal(t, 10, allocationsThatFit(n, 100, 102, false))
	// Memory is the binding dimension here, not CPU.
	require.Equal(t, 2, allocationsThatFit(n, 100, 512, false))
}

func TestAllocationsThatFitHonoursDistinctHosts(t *testing.T) {
	n := node("a", 10000, 10240, 0, 0)
	require.Equal(t, 1, allocationsThatFit(n, 100, 100, true),
		"distinct_hosts caps a group at one allocation per node")
}

func TestAllocationsThatFitWithNoReservation(t *testing.T) {
	n := node("a", 1000, 1024, 0, 0)
	// No reservation on either dimension means nothing here bounds it, which
	// must not become a division by zero.
	require.Equal(t, unboundedFit, allocationsThatFit(n, 0, 0, false))
	// One dimension unset: the other still binds.
	require.Equal(t, 10, allocationsThatFit(n, 0, 102, false))
	require.Equal(t, 10, allocationsThatFit(n, 100, 0, false))
}

func TestAllocationsThatFitWhenFull(t *testing.T) {
	n := node("a", 100, 100, 100, 100)
	require.Equal(t, 0, allocationsThatFit(n, 50, 50, false))
}

func TestHasDistinctHosts(t *testing.T) {
	job := &api.Job{}
	name := "web"
	tg := &api.TaskGroup{Name: &name}

	require.False(t, hasDistinctHosts(job, tg))

	tg.Constraints = []*api.Constraint{{Operand: "distinct_hosts"}}
	require.True(t, hasDistinctHosts(job, tg), "an empty RTarget means true for this operand")

	tg.Constraints = []*api.Constraint{{Operand: "distinct_hosts", RTarget: "true"}}
	require.True(t, hasDistinctHosts(job, tg))

	tg.Constraints = []*api.Constraint{{Operand: "distinct_hosts", RTarget: "false"}}
	require.False(t, hasDistinctHosts(job, tg))

	// Declared at job level rather than group level.
	tg.Constraints = nil
	job.Constraints = []*api.Constraint{{Operand: "distinct_hosts"}}
	require.True(t, hasDistinctHosts(job, tg))
}

func TestVerdictCountsAllocationsNotNodes(t *testing.T) {
	// One node with room for many is not "only one allocation".
	got := verdict(groupPlacement{Fitting: 1, Capacity: 218, Count: 2})
	require.Contains(t, got, "room for 218 allocations across 1 node")
	require.NotContains(t, got, "queued")
}

func TestVerdictWhenNothingFits(t *testing.T) {
	require.Contains(t, verdict(groupPlacement{Fitting: 0}), "NO node can take this")
}

func TestVerdictWhenShortOfCapacity(t *testing.T) {
	got := verdict(groupPlacement{Fitting: 2, Capacity: 3, Count: 5})
	require.Contains(t, got, "but count is 5")
	require.Contains(t, got, "2 would stay queued")
}

func TestVerdictExplainsDistinctHostsShortfall(t *testing.T) {
	got := verdict(groupPlacement{Fitting: 2, Capacity: 2, Count: 5, OnePerNode: true})
	require.Contains(t, got, "distinct_hosts allows only one allocation per node")
}

func TestRankReasonsOrdersByNodeCount(t *testing.T) {
	got := rankReasons(map[string]int{"rare": 1, "common": 9, "middling": 4})

	require.Equal(t, "common", got[0].Reason)
	require.Equal(t, 9, got[0].Nodes)
	require.Equal(t, "middling", got[1].Reason)
	require.Equal(t, "rare", got[2].Reason)
}

func TestRankReasonsIsStableOnTies(t *testing.T) {
	a := rankReasons(map[string]int{"zebra": 2, "alpha": 2})
	b := rankReasons(map[string]int{"alpha": 2, "zebra": 2})
	require.Equal(t, a, b)
	require.Equal(t, "alpha", a[0].Reason)
}

func TestDescribeConstraint(t *testing.T) {
	got := describeConstraint("group web", &api.Constraint{
		LTarget: "${node.class}", Operand: "=", RTarget: "gpu",
	})
	require.Equal(t, "group web: ${node.class} = gpu", got)

	// A missing operand means equality in Nomad.
	bare := describeConstraint("job", &api.Constraint{LTarget: "${meta.x}", RTarget: "y"})
	require.Contains(t, bare, "= y")
}

func TestGroupNeedsSumsEveryTask(t *testing.T) {
	cpu1, mem1, cpu2, mem2 := 100, 256, 50, 64
	name := "web"
	tg := &api.TaskGroup{
		Name: &name,
		Tasks: []*api.Task{
			{Name: "app", Resources: &api.Resources{CPU: &cpu1, MemoryMB: &mem1}},
			{Name: "sidecar", Resources: &api.Resources{CPU: &cpu2, MemoryMB: &mem2}},
		},
	}

	cpu, mem := groupNeeds(tg)
	require.Equal(t, int64(150), cpu)
	require.Equal(t, int64(320), mem)
}

// TestPlacementNoteAlwaysWarnsAboutUnevaluatedConstraints is the caveat that
// keeps the tool honest: a clean result is not a scheduler verdict.
func TestPlacementNoteAlwaysWarnsAboutUnevaluatedConstraints(t *testing.T) {
	note := placementNote(placementResult{
		Groups:      []groupPlacement{{Fitting: 3}},
		Unevaluated: []string{"group web: ${node.class} = gpu"},
	})

	require.Contains(t, note, "was NOT evaluated", "verb must agree with a count of one")
	require.Contains(t, note, "plan_job")
}

func TestPlacementNoteVerbAgreementForSeveral(t *testing.T) {
	note := placementNote(placementResult{
		Groups:      []groupPlacement{{Fitting: 1}},
		Unevaluated: []string{"a", "b"},
	})
	require.Contains(t, note, "2 requirements were NOT evaluated")
}

func TestPlacementNoteWhenNothingIsUnevaluated(t *testing.T) {
	note := placementNote(placementResult{Groups: []groupPlacement{{Fitting: 2}}})
	require.Contains(t, note, "no constraints, affinities or volumes")
	require.Contains(t, note, "should match the scheduler")
}

func TestPlacementNoteFlagsBlockedGroups(t *testing.T) {
	note := placementNote(placementResult{
		Groups: []groupPlacement{{Fitting: 0}, {Fitting: 4}},
	})
	require.Contains(t, note, "1 of 2 task groups cannot be placed")
	require.Contains(t, note, "rejection_summary")
}

func TestWasWere(t *testing.T) {
	require.Equal(t, "was", wasWere(1))
	require.Equal(t, "were", wasWere(0))
	require.Equal(t, "were", wasWere(2))
}
