// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// nodeStats is the projection returned by get_node_stats.
//
// Nomad's host stats include a per-core CPU breakdown and an entry for every
// mounted filesystem, which on a real machine is dozens of near-identical
// objects. What matters when triaging is the aggregate and the outliers, so
// the per-core detail is summarised and the disks are capped.
type nodeStats struct {
	NodeID   string `json:"node_id"`
	ShortID  string `json:"short_id"`
	NodeName string `json:"node_name,omitempty"`
	Status   string `json:"status,omitempty"`
	Uptime   string `json:"uptime,omitempty"`

	Memory *memoryStats `json:"memory,omitempty"`
	CPU    *cpuStats    `json:"cpu,omitempty"`

	Disks    []diskStats `json:"disks,omitempty"`
	AllocDir *diskStats  `json:"alloc_dir,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

type memoryStats struct {
	TotalMB     uint64  `json:"total_mb"`
	UsedMB      uint64  `json:"used_mb"`
	AvailableMB uint64  `json:"available_mb"`
	UsedPercent float64 `json:"used_percent"`
}

type cpuStats struct {
	Cores       int     `json:"cores"`
	TicksUsed   float64 `json:"ticks_used"`
	IdlePercent float64 `json:"idle_percent"`
	BusyPercent float64 `json:"busy_percent"`
}

type diskStats struct {
	Device      string  `json:"device"`
	Mountpoint  string  `json:"mountpoint"`
	SizeGB      float64 `json:"size_gb"`
	UsedGB      float64 `json:"used_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// maxDisksReported caps the filesystem list. Disks are sorted by fullness
// first, so the cap drops the empty ones rather than the interesting ones.
const maxDisksReported = 10

// GetNodeStats reports live resource usage for one client node.
func GetNodeStats(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_node_stats",
			mcp.WithDescription(
				"Get live CPU, memory and disk usage for one client node, as the node itself "+
					"reports it.\n\n"+
					"This is the machine's actual usage, which is a different question from what "+
					"Nomad has scheduled onto it. A node can be fully reserved by the scheduler and "+
					"nearly idle, or be at 100% memory while Nomad believes it has room — the "+
					"second is what makes tasks get OOM-killed on a node that looks fine in "+
					"list_nodes.\n\n"+
					"Reach for it when tasks on one node are being killed, restarting, or running "+
					"slowly while the same job is healthy elsewhere. A full disk is a common and "+
					"easily missed cause: Nomad does not schedule against disk usage, so a node "+
					"with no space left keeps accepting work that then fails.\n\n"+
					"Use get_allocation_stats for one workload's usage rather than the whole node. "+
					"This requires the node's agent to be reachable, so it fails on a node that is "+
					"down — which is itself an answer."),
			utils.ReadOnlyTool(),
			NodeIDParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodeID, err := req.RequireString("node_id")
			if err != nil {
				return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}
			out := nodeStats{NodeID: nodeID, ShortID: utils.ShortID(nodeID)}

			// The node's own record is fetched first so that a failure to reach
			// the agent can be explained by the node being down, rather than
			// reported as an unexplained connection error.
			var status string
			if node, _, err := nomad.Nodes().Info(nodeID, q); err == nil && node != nil {
				out.NodeName = node.Name
				out.Status = node.Status
				status = node.Status
			}

			stats, err := nomad.Nodes().Stats(nodeID, q)
			if err != nil {
				msg := utils.MapError(err, utils.ErrorContext{
					Op:         "read stats for node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:read",
					ListTool:   "list_nodes",
				}, p.Redactor())
				if status != "" && status != "ready" {
					msg += fmt.Sprintf(
						"\n\nThis node's status is %q. Host statistics come from the node's own "+
							"agent, so they are unavailable whenever the agent is not running — the "+
							"node being %s is the likely explanation rather than a separate fault.",
						status, status)
				}
				return utils.ErrorResult(msg)
			}
			if stats == nil {
				return utils.ErrorResultf(
					"Node %s returned no statistics. Its agent may have only just started.",
					utils.ShortID(nodeID))
			}

			if stats.Uptime > 0 {
				out.Uptime = (time.Duration(stats.Uptime) * time.Second).String()
			}

			if m := stats.Memory; m != nil && m.Total > 0 {
				used := float64(m.Used) / float64(m.Total) * 100
				out.Memory = &memoryStats{
					TotalMB:     m.Total / 1024 / 1024,
					UsedMB:      m.Used / 1024 / 1024,
					AvailableMB: m.Available / 1024 / 1024,
					UsedPercent: round1(used),
				}
				if used >= 90 {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"Memory is %.0f%% used. Tasks on this node are at risk of being OOM-killed, "+
							"which shows up as an allocation restarting with exit code 137.", used))
				}
			}

			if len(stats.CPU) > 0 {
				var idle float64
				for _, c := range stats.CPU {
					if c != nil {
						idle += c.Idle
					}
				}
				idle /= float64(len(stats.CPU))
				out.CPU = &cpuStats{
					Cores:       len(stats.CPU),
					TicksUsed:   round1(stats.CPUTicksConsumed),
					IdlePercent: round1(idle),
					BusyPercent: round1(100 - idle),
				}
				if idle <= 10 {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"CPU is %.0f%% busy across %d cores. Tasks here will be slow, and health "+
							"checks that time out will fail.", 100-idle, len(stats.CPU)))
				}
			}

			out.Disks = projectDisks(stats.DiskStats, &out)
			if d := stats.AllocDirStats; d != nil {
				disk := projectDisk(d)
				out.AllocDir = &disk
				if d.UsedPercent >= 85 {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"The allocation directory is %.0f%% full. This is the filesystem Nomad "+
							"writes task data and logs to, so filling it breaks new placements on "+
							"this node and can stop running tasks writing.", d.UsedPercent))
				}
			}

			out.Note = "These are the host's own figures, not Nomad's scheduling view. A node can " +
				"be fully reserved and idle, or saturated while Nomad thinks it has room; " +
				"read_node shows the scheduling side."
			if len(stats.DiskStats) > maxDisksReported {
				out.Note += fmt.Sprintf(" %d filesystems were reported; the %d fullest are shown.",
					len(stats.DiskStats), maxDisksReported)
			}

			return utils.JSONResult(out)
		},
	}
}

// projectDisks converts and caps the filesystem list, fullest first, and warns
// about any that are nearly full.
func projectDisks(in []*api.HostDiskStats, out *nodeStats) []diskStats {
	disks := make([]diskStats, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		disks = append(disks, projectDisk(d))
		if d.UsedPercent >= 90 {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"Filesystem %s at %s is %.0f%% full. Nomad does not schedule against disk usage, "+
					"so it will keep placing work here that then fails to write.",
				d.Device, d.Mountpoint, d.UsedPercent))
		}
	}

	// Insertion sort, descending by fullness: a handful of entries at most
	// after the cap, and it keeps the sort import out of the package.
	for i := 1; i < len(disks); i++ {
		for j := i; j > 0 && disks[j].UsedPercent > disks[j-1].UsedPercent; j-- {
			disks[j], disks[j-1] = disks[j-1], disks[j]
		}
	}
	if len(disks) > maxDisksReported {
		disks = disks[:maxDisksReported]
	}
	return disks
}

func projectDisk(d *api.HostDiskStats) diskStats {
	const gb = 1024 * 1024 * 1024
	return diskStats{
		Device:      d.Device,
		Mountpoint:  d.Mountpoint,
		SizeGB:      round1(float64(d.Size) / gb),
		UsedGB:      round1(float64(d.Used) / gb),
		UsedPercent: round1(d.UsedPercent),
	}
}

// round1 trims a float to one decimal place. Host stats arrive with far more
// precision than anyone needs, and every extra digit is context spent.
func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
