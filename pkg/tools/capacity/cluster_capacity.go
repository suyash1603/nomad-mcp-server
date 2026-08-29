// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// capacityResult is the tool's output.
type capacityResult struct {
	Nodes    nodeCounts      `json:"nodes"`
	Usable   resourcePool    `json:"usable_capacity"`
	Unusable *resourcePool   `json:"capacity_on_unusable_nodes,omitempty"`
	Largest  largestFit      `json:"largest_placeable_on_one_node"`
	GroupBy  string          `json:"grouped_by,omitempty"`
	Groups   []capacityGroup `json:"groups,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// nodeCounts is the population of the cluster by state.
type nodeCounts struct {
	Total      int `json:"total"`
	Ready      int `json:"ready"`
	Down       int `json:"down,omitempty"`
	Draining   int `json:"draining,omitempty"`
	Ineligible int `json:"ineligible,omitempty"`
}

// largestFit is the biggest task group that could actually be placed.
type largestFit struct {
	CPUMhz   int64  `json:"cpu_mhz"`
	CPUNode  string `json:"cpu_node,omitempty"`
	MemoryMB int64  `json:"memory_mb"`
	MemNode  string `json:"memory_node,omitempty"`
}

// capacityGroup is one node pool, datacenter or class.
type capacityGroup struct {
	Name     string       `json:"name"`
	Nodes    int          `json:"nodes"`
	Usable   int          `json:"usable_nodes"`
	Pool     resourcePool `json:"capacity"`
	Largest  largestFit   `json:"largest_placeable_on_one_node"`
	Pressure string       `json:"pressure,omitempty"`
}

// GetClusterCapacity reports what the cluster has and what is left.
func GetClusterCapacity(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_cluster_capacity",
			mcp.WithDescription(
				"Report the cluster's CPU and memory: how much exists, how much is allocated, how "+
					"much is left, and — the number that actually decides placement — the largest "+
					"amount still free on any SINGLE node.\n\n"+
					"That distinction is the point of this tool. Cluster-wide free capacity is "+
					"nearly meaningless in Nomad, because a task group must fit entirely on one "+
					"node. Ten nodes with 1GB free each cannot run one 2GB task, and a cluster "+
					"reporting 10GB free will still refuse to place it.\n\n"+
					"Use this for \"can this cluster fit X?\", \"do we need more nodes?\", \"how full "+
					"are we?\", and to check headroom before a scale-up. Pair it with "+
					"explain_placement when a specific job will not place.\n\n"+
					"Capacity on nodes that are down, draining or ineligible is reported separately "+
					"and never counted as available, because it is not."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
			groupByParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return clusterCapacity(ctx, req, p)
		},
	}
}

func clusterCapacity(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	nodes, err := loadCluster(ctx, nomad, p, region)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read cluster capacity",
			Address:    p.Address(),
			Capability: "node:read (plus read-job for the allocations)",
		}, p.Redactor()))
	}

	if len(nodes) == 0 {
		return utils.JSONResult(capacityResult{
			Note: "This cluster has no client nodes registered, so it has no capacity at all and " +
				"nothing can be scheduled. Servers do not run workloads; check that at least one " +
				"agent is running in client mode.",
		})
	}

	var usable, unusable []nodeCapacity
	out := capacityResult{}
	for _, n := range nodes {
		out.Nodes.Total++
		switch {
		case n.Status == "down":
			out.Nodes.Down++
		case n.Draining:
			out.Nodes.Draining++
		case !n.Eligible:
			out.Nodes.Ineligible++
		default:
			out.Nodes.Ready++
		}
		if n.usable() {
			usable = append(usable, n)
		} else {
			unusable = append(unusable, n)
		}
	}

	out.Usable = summarise(usable)
	if len(unusable) > 0 {
		stranded := summarise(unusable)
		out.Unusable = &stranded
	}

	cpu, mem, cpuNode, memNode := largestFree(usable)
	out.Largest = largestFit{CPUMhz: cpu, CPUNode: cpuNode, MemoryMB: mem, MemNode: memNode}

	if by := req.GetString("group_by", "node_pool"); by != "none" {
		out.GroupBy = by
		out.Groups = groupCapacity(nodes, by)
	}

	out.Note = capacityNote(out, len(usable), len(unusable))
	return utils.JSONResult(out)
}

// groupCapacity breaks the cluster down along one dimension.
func groupCapacity(nodes []nodeCapacity, by string) []capacityGroup {
	buckets := map[string][]nodeCapacity{}
	for _, n := range nodes {
		key := groupKey(n, by)
		buckets[key] = append(buckets[key], n)
	}

	out := make([]capacityGroup, 0, len(buckets))
	for _, name := range sortedGroupNames(buckets) {
		members := buckets[name]

		var usable []nodeCapacity
		for _, n := range members {
			if n.usable() {
				usable = append(usable, n)
			}
		}

		cpu, mem, cpuNode, memNode := largestFree(usable)
		g := capacityGroup{
			Name:    name,
			Nodes:   len(members),
			Usable:  len(usable),
			Pool:    summarise(usable),
			Largest: largestFit{CPUMhz: cpu, CPUNode: cpuNode, MemoryMB: mem, MemNode: memNode},
		}
		g.Pressure = pressureWord(g)
		out = append(out, g)
	}
	return out
}

// pressureWord names the state of one group in a word or two.
//
// A group with no usable nodes is called out separately from a full one: they
// look identical in the numbers — nothing can be placed either way — and they
// need completely different fixes.
func pressureWord(g capacityGroup) string {
	switch {
	case g.Usable == 0:
		return "no usable nodes: every node here is down, draining or ineligible"
	case g.Pool.MemPercent >= 90 || g.Pool.CPUPercent >= 90:
		return "critical: over 90% allocated"
	case g.Pool.MemPercent >= 75 || g.Pool.CPUPercent >= 75:
		return "tight: over 75% allocated"
	}
	return ""
}

// capacityNote explains the numbers in words.
func capacityNote(r capacityResult, usable, unusable int) string {
	var parts []string

	if usable == 0 {
		return "No node can accept work: all " + fmt.Sprint(r.Nodes.Total) +
			" are down, draining or ineligible. Nothing will be placed anywhere until that " +
			"changes, whatever the totals below say."
	}

	// The headline translation. Free-across-the-cluster and free-on-one-node are
	// different numbers and only the second decides whether a job places — but
	// on a single usable node they are the same number, and drawing a contrast
	// between a figure and itself reads as a mistake.
	if usable == 1 {
		parts = append(parts, fmt.Sprintf(
			"One usable node, so cluster-wide free memory and the largest placeable task group "+
				"are the same figure: %d MB on %s. Adding a second node would change that — a "+
				"task group must fit entirely on one node, so free capacity does not pool.",
			r.Usable.MemFree, orDefault(r.Largest.MemNode, "it")))
	} else {
		parts = append(parts, fmt.Sprintf(
			"The cluster has %d MB of memory free in total, but the largest single task group it "+
				"could actually place must fit within %d MB — the most free on any one node (%s). "+
				"Use the second number to answer \"will this fit?\"; the first only says how much "+
				"is unused overall.",
			r.Usable.MemFree, r.Largest.MemoryMB, orDefault(r.Largest.MemNode, "unknown")))
	}

	if unusable > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d node%s cannot accept work and %s capacity is excluded from every total above.",
			unusable, plural(unusable), pronoun(unusable)))
	}

	switch {
	case r.Usable.MemPercent >= 90 || r.Usable.CPUPercent >= 90:
		parts = append(parts,
			"The cluster is over 90% allocated. Expect placement failures and blocked evaluations.")
	case r.Usable.MemPercent >= 75 || r.Usable.CPUPercent >= 75:
		parts = append(parts,
			"The cluster is over 75% allocated. There is room, but little margin for a node "+
				"failing or a deployment that briefly doubles a job's allocations.")
	}

	parts = append(parts,
		"These are RESERVATIONS, not measured use. A task reserving 2GB and using 200MB counts "+
			"as 2GB here — analyze_job_resources compares the two.")

	return joinNote(parts...)
}

func pronoun(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}

// joinNote combines non-empty fragments.
func joinNote(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	out := ""
	for i, p := range kept {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
