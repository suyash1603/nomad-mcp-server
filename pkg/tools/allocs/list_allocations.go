// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package allocs holds the tools that read and act on Nomad allocations.
//
// An allocation is one instance of a task group placed on one client node.
// Everything that actually runs is an allocation, so these are the tools that
// answer "what is happening right now" and "why did it stop".
package allocs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/projection"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// allocIDParam declares the allocation ID argument.
func allocIDParam() mcp.ToolOption {
	return mcp.WithString("alloc_id",
		mcp.Required(),
		mcp.Description(
			"The allocation's full ID, as returned by list_allocations or list_job_allocations. "+
				"Nomad requires the complete UUID here, not the shortened form shown in the CLI — "+
				"if you only have a short ID, resolve it with the search tool first."),
	)
}

// resolveAlloc fetches an allocation, which several tools need in full because
// the Nomad client requires an *api.Allocation rather than an ID.
func resolveAlloc(ctx context.Context, p *client.Provider, nomad *api.Client, allocID, namespace, region string) (*api.Allocation, string) {
	alloc, _, err := nomad.Allocations().Info(allocID, &api.QueryOptions{
		Namespace: namespace,
		Region:    region,
	})
	if err != nil {
		return nil, utils.MapError(err, utils.ErrorContext{
			Op:         "read allocation " + utils.ShortID(allocID),
			Kind:       "allocation",
			Name:       allocID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "read-job",
			ListTool:   "list_allocations",
		}, p.Redactor())
	}
	return alloc, ""
}

// ListAllocations lists allocations in a namespace.
func ListAllocations(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List allocations across a namespace, newest first, with each one's client status, " +
				"the node it is running on, and per-task state including restart counts and the " +
				"last event for any task that failed.\n\n" +
				"Use this for a cluster-wide view of what is running or recently failed. If you " +
				"already know which job you care about, list_job_allocations is more direct.\n\n" +
				"Allocations flagged needs_attention are failed, lost, or have a failed task inside " +
				"them, and are the ones worth investigating with read_allocation_logs."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("allocations"),
		utils.FilterParam(`ClientStatus == "failed"  •  TaskGroup == "web"  •  NodeID == "..."`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_allocations", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			page := utils.PageFrom(req)
			q := page.Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix:    req.GetString("prefix", ""),
				Filter:    req.GetString("filter", ""),
			})

			stubs, meta, err := nomad.Allocations().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list allocations",
					Kind:       "allocation",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
				}, p.Redactor()))
			}

			items := make([]projection.AllocStub, 0, len(stubs))
			unhealthy := 0
			for _, s := range stubs {
				item := projection.Alloc(s)
				if item.Unhealthy {
					unhealthy++
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if result.Note == "" && unhealthy > 0 {
				result.Note = "Some allocations need attention. Use read_allocation_logs with " +
					"log_type \"stderr\" on those to see why they failed."
			}
			return utils.JSONResult(result)
		},
	}
}

// allocDetail is the full view of one allocation.
type allocDetail struct {
	projection.AllocStub
	JobType       string            `json:"job_type,omitempty"`
	Resources     *allocResources   `json:"resources,omitempty"`
	DeployStatus  string            `json:"deployment_health,omitempty"`
	FollowupEval  string            `json:"followup_eval_id,omitempty"`
	NextAlloc     string            `json:"next_allocation_id,omitempty"`
	PreviousAlloc string            `json:"previous_allocation_id,omitempty"`
	PreemptedBy   string            `json:"preempted_by_allocation,omitempty"`
	Reschedules   []rescheduleEvent `json:"reschedule_history,omitempty"`
	Events        []taskEvent       `json:"recent_task_events,omitempty"`
	Diagnosis     string            `json:"diagnosis,omitempty"`
}

type allocResources struct {
	CPUMHz   int `json:"cpu_mhz,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
	DiskMB   int `json:"disk_mb,omitempty"`
}

type rescheduleEvent struct {
	Time      string `json:"time"`
	PrevAlloc string `json:"previous_alloc_id,omitempty"`
	PrevNode  string `json:"previous_node_id,omitempty"`
	Delay     string `json:"delay,omitempty"`
}

type taskEvent struct {
	Task    string `json:"task"`
	Time    string `json:"time"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// ReadAllocation returns one allocation in detail.
func ReadAllocation(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_allocation",
			mcp.WithDescription(
				"Read one allocation in detail: its status, the node it is on, per-task state and "+
					"resources, its reschedule history, and its recent task events.\n\n"+
					"The task events are the important part when something has gone wrong. They are "+
					"Nomad's own account of what happened to each task in order — image pulled, task "+
					"started, exited with code 1, not restarting. Read them before reaching for logs: "+
					"they distinguish a task that crashed from one that was killed, evicted, or never "+
					"started at all.\n\n"+
					"Use read_allocation_logs afterwards to see what the task itself printed."),
			utils.ReadOnlyTool(),
			allocIDParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return readAllocation(ctx, req, p)
		},
	}
}

func readAllocation(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	allocID, err := req.RequireString("alloc_id")
	if err != nil {
		return utils.ErrorResult("The 'alloc_id' argument is required. Use list_allocations or list_job_allocations to find one.")
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	alloc, errMsg := resolveAlloc(ctx, p, nomad, allocID, namespace,
		p.ResolveRegion(ctx, req.GetString("region", "")))
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	out := allocDetail{
		AllocStub:     projection.Alloc(alloc.Stub()),
		JobType:       jobType(alloc),
		FollowupEval:  alloc.FollowupEvalID,
		NextAlloc:     alloc.NextAllocation,
		PreviousAlloc: alloc.PreviousAllocation,
		PreemptedBy:   alloc.PreemptedByAllocation,
	}

	if alloc.Resources != nil {
		out.Resources = &allocResources{
			CPUMHz:   intOr(alloc.Resources.CPU),
			MemoryMB: intOr(alloc.Resources.MemoryMB),
			DiskMB:   intOr(alloc.Resources.DiskMB),
		}
	}

	if alloc.DeploymentStatus != nil && alloc.DeploymentStatus.Healthy != nil {
		if *alloc.DeploymentStatus.Healthy {
			out.DeployStatus = "healthy"
		} else {
			out.DeployStatus = "unhealthy"
		}
	}

	if alloc.RescheduleTracker != nil {
		for _, e := range alloc.RescheduleTracker.Events {
			if e == nil {
				continue
			}
			out.Reschedules = append(out.Reschedules, rescheduleEvent{
				Time:      utils.FormatTime(e.RescheduleTime),
				PrevAlloc: e.PrevAllocID,
				PrevNode:  e.PrevNodeID,
			})
		}
	}

	// Task events, most recent last, capped so a task that has restarted a
	// hundred times does not flood the context.
	for name, ts := range alloc.TaskStates {
		if ts == nil {
			continue
		}
		events := ts.Events
		if len(events) > 10 {
			events = events[len(events)-10:]
		}
		for _, e := range events {
			if e == nil {
				continue
			}
			out.Events = append(out.Events, taskEvent{
				Task:    name,
				Time:    utils.FormatTime(e.Time),
				Type:    e.Type,
				Message: p.Redactor().String(eventMessage(e)),
			})
		}
	}

	out.Diagnosis = diagnose(alloc, out)

	return utils.JSONResult(out)
}

// diagnose turns the allocation's state into a one-line hypothesis.
//
// This is the difference between handing a model a pile of fields and handing
// it a starting point. Each branch corresponds to a failure mode that looks
// different in the data but identical in the summary status.
func diagnose(alloc *api.Allocation, d allocDetail) string {
	switch alloc.ClientStatus {
	case "running":
		if d.DeployStatus == "unhealthy" {
			return "This allocation is running but was marked unhealthy by its deployment, which " +
				"usually means a health check is failing. Check the service's health check definition."
		}
		return ""
	case "pending":
		return "This allocation has been placed on a node but has not started yet. If it stays " +
			"pending, the node may be downloading an image or the task may be waiting on a " +
			"prestart lifecycle hook."
	case "complete":
		return "This allocation finished normally. For a batch job that is success; for a service " +
			"job it means the task exited when it was expected to keep running."
	case "lost":
		return "The node running this allocation stopped heartbeating, so Nomad marked the " +
			"allocation lost and will reschedule it. Check the node with read_node."
	case "failed":
		for _, t := range d.Tasks {
			if !t.Failed {
				continue
			}
			switch {
			case t.OOMKilled:
				return "Task " + t.Name + " was killed for exceeding its memory limit. Raise the " +
					"memory value in the task's resources block, or reduce what the task allocates."
			case t.ExitCode != 0:
				return "Task " + t.Name + " exited with code " + itoa(t.ExitCode) + ". Read its " +
					"stderr with read_allocation_logs to see what it reported before exiting."
			default:
				return "Task " + t.Name + " failed: " + t.LastReason +
					". Read its stderr with read_allocation_logs for detail."
			}
		}
		return "This allocation failed. Read its task events above and then its stderr with read_allocation_logs."
	}
	return ""
}

func jobType(alloc *api.Allocation) string {
	if alloc.Job == nil || alloc.Job.Type == nil {
		return ""
	}
	return *alloc.Job.Type
}

func eventMessage(e *api.TaskEvent) string {
	if e.DisplayMessage != "" {
		return e.DisplayMessage
	}
	return e.Message
}

func intOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
