// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package allocs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

type allocStats struct {
	AllocID   string               `json:"alloc_id"`
	ShortID   string               `json:"short_id"`
	Namespace string               `json:"namespace"`
	Memory    *memStats            `json:"memory,omitempty"`
	CPU       *cpuStats            `json:"cpu,omitempty"`
	Tasks     map[string]taskUsage `json:"tasks,omitempty"`
	Note      string               `json:"note,omitempty"`
}

type memStats struct {
	RSSMB      float64 `json:"rss_mb,omitempty"`
	CacheMB    float64 `json:"cache_mb,omitempty"`
	UsageMB    float64 `json:"usage_mb,omitempty"`
	MaxUsageMB float64 `json:"max_usage_mb,omitempty"`
	AllocatedM int     `json:"allocated_mb,omitempty"`
	PercentOf  float64 `json:"percent_of_allocated,omitempty"`
}

type cpuStats struct {
	Percent      float64 `json:"percent,omitempty"`
	TotalTicks   float64 `json:"total_ticks,omitempty"`
	AllocatedMHz int     `json:"allocated_mhz,omitempty"`
	ThrottledFor uint64  `json:"throttled_time_ns,omitempty"`
	Periods      uint64  `json:"throttled_periods,omitempty"`
}

type taskUsage struct {
	MemoryMB float64 `json:"memory_mb,omitempty"`
	CPUPct   float64 `json:"cpu_percent,omitempty"`
}

// GetAllocationStats reports live resource usage for an allocation.
func GetAllocationStats(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_allocation_stats",
			mcp.WithDescription(
				"Get live CPU and memory usage for a running allocation, per task and in total, "+
					"alongside what it was allocated.\n\n"+
					"Use this to answer whether a task is near its limits. Memory reported as a high "+
					"percentage of allocated is the usual precursor to an out-of-memory kill, and CPU "+
					"throttling shows up here as throttled_time before it shows up as a latency "+
					"complaint.\n\n"+
					"Only works for allocations that are currently running on a reachable client "+
					"node. A stopped or failed allocation has no live stats — use read_allocation "+
					"instead, whose task events record whether it was OOM killed."),
			utils.ReadOnlyTool(),
			allocIDParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			allocID, namespace, alloc, nomad, region, errMsg := allocContext(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}

			if alloc.ClientStatus != "running" {
				return utils.ErrorResultf(
					"Allocation %s is %q, not running, so it has no live resource statistics. "+
						"Use read_allocation to see its final state and task events — those record "+
						"whether it was killed for exceeding memory.",
					utils.ShortID(allocID), alloc.ClientStatus)
			}

			usage, err := nomad.Allocations().Stats(alloc, &api.QueryOptions{
				Namespace: namespace,
				Region:    region,
			})
			if err != nil {
				return utils.ErrorResult(fsFailure(err, p, allocID, "resource statistics", namespace))
			}

			out := allocStats{
				AllocID:   allocID,
				ShortID:   utils.ShortID(allocID),
				Namespace: namespace,
			}

			allocatedMem, allocatedCPU := allocatedResources(alloc)

			if usage.ResourceUsage != nil {
				if m := usage.ResourceUsage.MemoryStats; m != nil {
					out.Memory = &memStats{
						RSSMB:      bytesToMB(m.RSS),
						CacheMB:    bytesToMB(m.Cache),
						UsageMB:    bytesToMB(m.Usage),
						MaxUsageMB: bytesToMB(m.MaxUsage),
						AllocatedM: allocatedMem,
					}
					if allocatedMem > 0 && m.Usage > 0 {
						out.Memory.PercentOf = round1(bytesToMB(m.Usage) / float64(allocatedMem) * 100)
					}
				}
				if c := usage.ResourceUsage.CpuStats; c != nil {
					out.CPU = &cpuStats{
						Percent:      round1(c.Percent),
						TotalTicks:   round1(c.TotalTicks),
						AllocatedMHz: allocatedCPU,
						ThrottledFor: c.ThrottledTime,
						Periods:      c.ThrottledPeriods,
					}
				}
			}

			for name, t := range usage.Tasks {
				if t == nil || t.ResourceUsage == nil {
					continue
				}
				u := taskUsage{}
				if t.ResourceUsage.MemoryStats != nil {
					u.MemoryMB = round1(bytesToMB(t.ResourceUsage.MemoryStats.Usage))
				}
				if t.ResourceUsage.CpuStats != nil {
					u.CPUPct = round1(t.ResourceUsage.CpuStats.Percent)
				}
				if out.Tasks == nil {
					out.Tasks = map[string]taskUsage{}
				}
				out.Tasks[name] = u
			}

			switch {
			case out.Memory != nil && out.Memory.PercentOf >= 90:
				out.Note = "Memory usage is at or above 90% of what this allocation was given. " +
					"It is a strong candidate for an out-of-memory kill; consider raising the " +
					"memory value in the task's resources block."
			case out.CPU != nil && out.CPU.ThrottledFor > 0:
				out.Note = "This allocation is being CPU throttled, meaning it wants more CPU than " +
					"it was allocated. That shows up to users as latency rather than as an error."
			}

			return utils.JSONResult(out)
		},
	}
}

func allocatedResources(alloc *api.Allocation) (memMB, cpuMHz int) {
	if alloc.Resources != nil {
		if alloc.Resources.MemoryMB != nil {
			memMB = *alloc.Resources.MemoryMB
		}
		if alloc.Resources.CPU != nil {
			cpuMHz = *alloc.Resources.CPU
		}
	}
	// AllocatedResources is the authoritative view once the allocation has been
	// placed, and reflects any per-task overrides. Memory and Cpu are structs
	// rather than pointers here, so they are summed rather than nil-checked.
	if alloc.AllocatedResources != nil && len(alloc.AllocatedResources.Tasks) > 0 {
		var taskMem, taskCPU int
		for _, t := range alloc.AllocatedResources.Tasks {
			if t == nil {
				continue
			}
			taskMem += int(t.Memory.MemoryMB)
			taskCPU += int(t.Cpu.CpuShares)
		}
		if taskMem > 0 {
			memMB = taskMem
		}
		if taskCPU > 0 {
			cpuMHz = taskCPU
		}
	}
	return memMB, cpuMHz
}

func bytesToMB(b uint64) float64 {
	return round1(float64(b) / (1024 * 1024))
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
