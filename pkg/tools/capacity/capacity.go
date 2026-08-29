// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package capacity holds the tools that answer questions about fit and size:
// what the cluster has, whether a particular job can be placed on it, and
// whether what is already running asked for the right amount.
//
// These are arithmetic tools. Nomad exposes every number they need and joins
// none of them: node capacity lives on the node list, consumption lives on the
// allocation list, and what a task asked for lives in the job specification.
// Answering "is there room for this?" from those three by hand means holding a
// lot of small numbers in your head, which is precisely where a model reading
// many tool results goes wrong.
//
// One idea runs through all three tools, and it is the thing worth
// understanding about Nomad capacity: cluster-wide free capacity is nearly
// meaningless, because a task group must fit on a SINGLE node. Ten nodes with
// 1GB free each cannot run one 2GB task. Every total reported here is therefore
// accompanied by the largest amount actually placeable on one node, and the
// notes say plainly which of the two answers a question.
package capacity

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// listPageSize bounds each underlying list call.
const listPageSize = 500

// resourcePool is the capacity picture for a set of nodes.
type resourcePool struct {
	CPUTotal     int64 `json:"cpu_mhz_total"`
	CPUReserved  int64 `json:"cpu_mhz_reserved,omitempty"`
	CPUAllocated int64 `json:"cpu_mhz_allocated"`
	CPUFree      int64 `json:"cpu_mhz_free"`
	CPUPercent   int   `json:"cpu_percent_allocated"`

	MemTotal     int64 `json:"memory_mb_total"`
	MemReserved  int64 `json:"memory_mb_reserved,omitempty"`
	MemAllocated int64 `json:"memory_mb_allocated"`
	MemFree      int64 `json:"memory_mb_free"`
	MemPercent   int   `json:"memory_percent_allocated"`

	DiskTotal     int64 `json:"disk_mb_total,omitempty"`
	DiskAllocated int64 `json:"disk_mb_allocated,omitempty"`
	DiskFree      int64 `json:"disk_mb_free,omitempty"`
}

// nodeCapacity is one node's contribution.
type nodeCapacity struct {
	ID         string
	Name       string
	Pool       string
	Datacenter string
	Class      string
	Status     string
	Eligible   bool
	Draining   bool

	CPUTotal int64
	MemTotal int64
	DiskFree int64

	CPUAllocated int64
	MemAllocated int64
	DiskAlloc    int64
}

// usable reports whether this node can accept new work at all.
func (n nodeCapacity) usable() bool {
	return n.Status == "ready" && n.Eligible && !n.Draining
}

func (n nodeCapacity) cpuFree() int64 { return max64(n.CPUTotal-n.CPUAllocated, 0) }
func (n nodeCapacity) memFree() int64 { return max64(n.MemTotal-n.MemAllocated, 0) }

// loadCluster reads every node and allocation once and joins them.
//
// Two calls, whatever the size of the cluster: both list endpoints return the
// resource fields when asked, so there is no need to walk nodes individually.
// On a cluster with hundreds of nodes that is the difference between a tool
// that answers and one that times out.
func loadCluster(ctx context.Context, nomad *api.Client, p *client.Provider, region string) ([]nodeCapacity, error) {
	nodes, _, err := nomad.Nodes().List(&api.QueryOptions{
		Region:  region,
		PerPage: listPageSize,
		Params:  map[string]string{"resources": "true"},
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	byID := make(map[string]*nodeCapacity, len(nodes))
	out := make([]nodeCapacity, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		nc := nodeCapacity{
			ID:         n.ID,
			Name:       n.Name,
			Pool:       orDefault(n.NodePool, "default"),
			Datacenter: n.Datacenter,
			Class:      n.NodeClass,
			Status:     n.Status,
			Eligible:   n.SchedulingEligibility != "ineligible",
			Draining:   n.Drain,
		}
		if r := n.NodeResources; r != nil {
			nc.CPUTotal = r.Cpu.CpuShares
			nc.MemTotal = r.Memory.MemoryMB
			nc.DiskFree = r.Disk.DiskMB
		}
		// Reserved resources belong to the host, not to Nomad, so they are
		// removed from the total rather than counted as allocated. Reporting
		// them as available would overstate what can actually be scheduled.
		if r := n.ReservedResources; r != nil {
			nc.CPUTotal = max64(nc.CPUTotal-int64(r.Cpu.CpuShares), 0)
			nc.MemTotal = max64(nc.MemTotal-int64(r.Memory.MemoryMB), 0)
		}
		out = append(out, nc)
	}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}

	allocs, _, err := nomad.Allocations().List(&api.QueryOptions{
		Region:    region,
		Namespace: "*",
		PerPage:   listPageSize,
		Params:    map[string]string{"resources": "true"},
	})
	if err != nil {
		return nil, fmt.Errorf("listing allocations: %w", err)
	}

	for _, a := range allocs {
		if a == nil || a.AllocatedResources == nil {
			continue
		}
		// Only allocations that are actually holding resources count. A
		// completed or failed allocation still appears in the list but its
		// reservation is long gone, and counting it would invent pressure that
		// does not exist.
		if !holdsResources(a.ClientStatus) {
			continue
		}
		node := byID[a.NodeID]
		if node == nil {
			continue
		}
		cpu, mem := allocSize(a.AllocatedResources)
		node.CPUAllocated += cpu
		node.MemAllocated += mem
		node.DiskAlloc += a.AllocatedResources.Shared.DiskMB
	}

	return out, nil
}

// holdsResources reports whether an allocation still occupies its reservation.
func holdsResources(clientStatus string) bool {
	switch clientStatus {
	case "running", "pending":
		return true
	}
	return false
}

// allocSize totals one allocation's task reservations.
func allocSize(r *api.AllocatedResources) (cpu, mem int64) {
	for _, t := range r.Tasks {
		if t == nil {
			continue
		}
		cpu += t.Cpu.CpuShares
		// MemoryMB is the reservation the scheduler places against; MemoryMaxMB
		// is an oversubscription ceiling the scheduler does not reserve, so
		// counting it here would overstate how full the cluster is.
		mem += t.Memory.MemoryMB
	}
	return cpu, mem
}

// summarise totals a set of nodes into one pool.
func summarise(nodes []nodeCapacity) resourcePool {
	var out resourcePool
	for _, n := range nodes {
		out.CPUTotal += n.CPUTotal
		out.MemTotal += n.MemTotal
		out.CPUAllocated += n.CPUAllocated
		out.MemAllocated += n.MemAllocated
		out.DiskTotal += n.DiskFree
		out.DiskAllocated += n.DiskAlloc
	}
	out.CPUFree = max64(out.CPUTotal-out.CPUAllocated, 0)
	out.MemFree = max64(out.MemTotal-out.MemAllocated, 0)
	out.DiskFree = max64(out.DiskTotal-out.DiskAllocated, 0)
	out.CPUPercent = percent(out.CPUAllocated, out.CPUTotal)
	out.MemPercent = percent(out.MemAllocated, out.MemTotal)
	return out
}

// largestFree returns the biggest CPU and memory still free on any single
// usable node, and which node that is.
//
// This is the number that decides whether a task group can be placed, and it is
// the one Nomad never reports. Cluster-wide free capacity answers a different
// and much less useful question.
func largestFree(nodes []nodeCapacity) (cpu, mem int64, cpuNode, memNode string) {
	for _, n := range nodes {
		if !n.usable() {
			continue
		}
		if f := n.cpuFree(); f > cpu {
			cpu, cpuNode = f, n.displayName()
		}
		if f := n.memFree(); f > mem {
			mem, memNode = f, n.displayName()
		}
	}
	return cpu, mem, cpuNode, memNode
}

func (n nodeCapacity) displayName() string {
	if n.Name != "" {
		return n.Name
	}
	return utils.ShortID(n.ID)
}

// groupKey picks the grouping dimension.
func groupKey(n nodeCapacity, by string) string {
	switch by {
	case "datacenter":
		return orDefault(n.Datacenter, "(none)")
	case "node_class":
		return orDefault(n.Class, "(no class)")
	default:
		return n.Pool
	}
}

// groupByParam declares the shared grouping argument.
func groupByParam() mcp.ToolOption {
	return mcp.WithString("group_by",
		mcp.DefaultString("node_pool"),
		mcp.Enum("node_pool", "datacenter", "node_class", "none"),
		mcp.Description(
			"How to break the totals down. Node pools and datacenters are the boundaries a job "+
				"actually targets, so a cluster that looks half empty overall can still be full "+
				"in the pool a particular job is confined to."),
	)
}

func percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// sortedGroupNames returns map keys in a stable order.
func sortedGroupNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
