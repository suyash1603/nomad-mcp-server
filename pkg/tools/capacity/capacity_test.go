// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func node(name string, cpuTotal, memTotal, cpuAlloc, memAlloc int64) nodeCapacity {
	return nodeCapacity{
		Name: name, Status: "ready", Eligible: true, Pool: "default",
		CPUTotal: cpuTotal, MemTotal: memTotal,
		CPUAllocated: cpuAlloc, MemAllocated: memAlloc,
	}
}

func TestNodeUsable(t *testing.T) {
	ok := node("a", 100, 100, 0, 0)
	require.True(t, ok.usable())

	down := ok
	down.Status = "down"
	require.False(t, down.usable())

	draining := ok
	draining.Draining = true
	require.False(t, draining.usable())

	ineligible := ok
	ineligible.Eligible = false
	require.False(t, ineligible.usable())
}

func TestNodeFreeNeverGoesNegative(t *testing.T) {
	// Oversubscribed memory is possible with memory_max, and a negative free
	// figure would propagate into every total.
	over := node("a", 100, 100, 150, 150)
	require.Equal(t, int64(0), over.cpuFree())
	require.Equal(t, int64(0), over.memFree())
}

func TestSummarise(t *testing.T) {
	got := summarise([]nodeCapacity{
		node("a", 1000, 2048, 250, 512),
		node("b", 1000, 2048, 750, 1536),
	})

	require.Equal(t, int64(2000), got.CPUTotal)
	require.Equal(t, int64(1000), got.CPUAllocated)
	require.Equal(t, int64(1000), got.CPUFree)
	require.Equal(t, 50, got.CPUPercent)

	require.Equal(t, int64(4096), got.MemTotal)
	require.Equal(t, int64(2048), got.MemAllocated)
	require.Equal(t, 50, got.MemPercent)
}

func TestSummariseEmpty(t *testing.T) {
	got := summarise(nil)
	require.Zero(t, got.CPUPercent, "percentages must not divide by zero")
	require.Zero(t, got.MemPercent)
}

// TestLargestFreeIsPerNodeNotClusterWide is the translation the whole package
// exists for. Ten nodes with 1GB free each cannot run one 2GB task, and a
// cluster-wide total would say they can.
func TestLargestFreeIsPerNodeNotClusterWide(t *testing.T) {
	nodes := []nodeCapacity{
		node("a", 1000, 1024, 0, 0),
		node("b", 1000, 1024, 0, 0),
		node("c", 1000, 1024, 0, 0),
	}

	pool := summarise(nodes)
	require.Equal(t, int64(3072), pool.MemFree, "cluster-wide free is the sum")

	_, mem, _, memNode := largestFree(nodes)
	require.Equal(t, int64(1024), mem, "but only one node's worth can actually be used")
	require.NotEmpty(t, memNode)
}

func TestLargestFreeSkipsUnusableNodes(t *testing.T) {
	big := node("big", 9000, 9000, 0, 0)
	big.Draining = true

	nodes := []nodeCapacity{big, node("small", 1000, 1024, 0, 0)}

	cpu, mem, _, memNode := largestFree(nodes)
	require.Equal(t, int64(1000), cpu, "a draining node's capacity is not available")
	require.Equal(t, int64(1024), mem)
	require.Equal(t, "small", memNode)
}

func TestLargestFreeWithNoUsableNodes(t *testing.T) {
	down := node("a", 100, 100, 0, 0)
	down.Status = "down"

	cpu, mem, cpuNode, memNode := largestFree([]nodeCapacity{down})
	require.Zero(t, cpu)
	require.Zero(t, mem)
	require.Empty(t, cpuNode)
	require.Empty(t, memNode)
}

func TestHoldsResources(t *testing.T) {
	// A completed allocation still appears in the list, but its reservation is
	// gone; counting it would invent pressure that does not exist.
	require.True(t, holdsResources("running"))
	require.True(t, holdsResources("pending"))
	require.False(t, holdsResources("complete"))
	require.False(t, holdsResources("failed"))
	require.False(t, holdsResources("lost"))
}

func TestAllocSizeSumsTasksNotMemoryMax(t *testing.T) {
	r := &api.AllocatedResources{
		Tasks: map[string]*api.AllocatedTaskResources{
			"web": {
				Cpu:    api.AllocatedCpuResources{CpuShares: 500},
				Memory: api.AllocatedMemoryResources{MemoryMB: 256, MemoryMaxMB: 2048},
			},
			"sidecar": {
				Cpu:    api.AllocatedCpuResources{CpuShares: 100},
				Memory: api.AllocatedMemoryResources{MemoryMB: 64},
			},
		},
	}

	cpu, mem := allocSize(r)
	require.Equal(t, int64(600), cpu)
	// MemoryMaxMB is an oversubscription ceiling the scheduler does not
	// reserve, so counting it would overstate how full the cluster is.
	require.Equal(t, int64(320), mem)
}

func TestGroupKey(t *testing.T) {
	n := nodeCapacity{Pool: "gpu", Datacenter: "dc1", Class: "compute"}
	require.Equal(t, "gpu", groupKey(n, "node_pool"))
	require.Equal(t, "dc1", groupKey(n, "datacenter"))
	require.Equal(t, "compute", groupKey(n, "node_class"))
	require.Equal(t, "gpu", groupKey(n, "anything else"))

	bare := nodeCapacity{Pool: "default"}
	require.Equal(t, "(none)", groupKey(bare, "datacenter"))
	require.Equal(t, "(no class)", groupKey(bare, "node_class"))
}

func TestPressureWordDistinguishesFullFromUnusable(t *testing.T) {
	// These look identical in the numbers — nothing can be placed either way —
	// and they need completely different fixes.
	unusable := pressureWord(capacityGroup{Usable: 0})
	require.Contains(t, unusable, "no usable nodes")

	full := pressureWord(capacityGroup{Usable: 3, Pool: resourcePool{MemPercent: 95}})
	require.Contains(t, full, "critical")

	tight := pressureWord(capacityGroup{Usable: 3, Pool: resourcePool{CPUPercent: 80}})
	require.Contains(t, tight, "tight")

	require.Empty(t, pressureWord(capacityGroup{Usable: 3, Pool: resourcePool{MemPercent: 10}}))
}

func TestCapacityNoteOnSingleNodeDoesNotContrastANumberWithItself(t *testing.T) {
	r := capacityResult{
		Nodes:   nodeCounts{Total: 1, Ready: 1},
		Usable:  resourcePool{MemFree: 4096},
		Largest: largestFit{MemoryMB: 4096, MemNode: "only-node"},
	}

	note := capacityNote(r, 1, 0)
	require.Contains(t, note, "One usable node")
	require.Contains(t, note, "does not pool")
	require.NotContains(t, note, "but the largest single task group")
}

func TestCapacityNoteContrastsOnAMultiNodeCluster(t *testing.T) {
	r := capacityResult{
		Usable:  resourcePool{MemFree: 8192},
		Largest: largestFit{MemoryMB: 1024, MemNode: "n3"},
	}

	note := capacityNote(r, 4, 0)
	require.Contains(t, note, "8192 MB of memory free in total")
	require.Contains(t, note, "must fit within 1024 MB")
}

func TestCapacityNoteWhenNothingIsUsable(t *testing.T) {
	note := capacityNote(capacityResult{Nodes: nodeCounts{Total: 5}}, 0, 5)
	require.Contains(t, note, "No node can accept work")
	require.Contains(t, note, "whatever the totals below say")
}

func TestCapacityNoteAlwaysSaysReservationsNotUsage(t *testing.T) {
	note := capacityNote(capacityResult{Usable: resourcePool{MemFree: 100}}, 2, 0)
	require.Contains(t, note, "RESERVATIONS, not measured use")
	require.Contains(t, note, "analyze_job_resources")
}

func TestPercentAndMax64(t *testing.T) {
	require.Equal(t, 50, percent(1, 2))
	require.Equal(t, 0, percent(1, 0))
	require.Equal(t, int64(5), max64(5, 3))
	require.Equal(t, int64(5), max64(3, 5))
}
