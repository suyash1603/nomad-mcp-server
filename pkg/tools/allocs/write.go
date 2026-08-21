// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package allocs

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// RestartAllocation restarts tasks inside a running allocation.
func RestartAllocation(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("restart_allocation",
			mcp.WithDescription(
				"Restart the tasks inside a running allocation, in place on the same node.\n\n"+
					"This restarts the task process; it does not reschedule the allocation elsewhere "+
					"and does not create a new allocation. The allocation ID stays the same, so it is "+
					"the right tool for clearing a wedged process, and the wrong one for moving work "+
					"off a bad node — use stop_allocation for that.\n\n"+
					"For a service job this interrupts whatever the task was serving. Confirm with "+
					"the user first, and prefer restarting one allocation over a whole group unless "+
					"they asked otherwise."),
			utils.MutatingTool(true, true),
			allocIDParam(),
			mcp.WithString("task",
				mcp.Description(
					"Restart only this task. If omitted, every task in the allocation is restarted."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			allocID, namespace, alloc, nomad, region, errMsg := allocContext(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}
			_ = region

			if alloc.ClientStatus != "running" {
				return utils.ErrorResultf(
					"Allocation %s is %q, not running, so there is nothing to restart. A terminal "+
						"allocation cannot be revived — Nomad replaces it with a new one. Check the "+
						"owning job with list_job_allocations.",
					utils.ShortID(allocID), alloc.ClientStatus)
			}

			task := req.GetString("task", "")
			if task != "" {
				if resolved, msg := pickTask(alloc, task); msg != "" {
					return utils.ErrorResult(msg)
				} else {
					task = resolved
				}
			}

			var err error
			if task == "" {
				err = nomad.Allocations().RestartAllTasks(alloc, nil)
			} else {
				err = nomad.Allocations().Restart(alloc, task, nil)
			}
			if err != nil {
				return utils.ErrorResult(fsFailure(err, p, allocID, "restart", namespace))
			}

			scope := "all tasks"
			if task != "" {
				scope = "task " + task
			}
			return utils.JSONResult(map[string]any{
				"alloc_id":  allocID,
				"short_id":  utils.ShortID(allocID),
				"namespace": namespace,
				"restarted": scope,
				"note": "The restart was requested. Nomad restarts tasks asynchronously, so confirm " +
					"with read_allocation rather than assuming it has already happened.",
			})
		},
	}
}

// StopAllocation stops an allocation so Nomad reschedules it.
func StopAllocation(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("stop_allocation",
			mcp.WithDescription(
				"Stop a single allocation. Nomad will then reschedule the work, usually onto a "+
					"different node, creating a NEW allocation with a new ID.\n\n"+
					"This is the tool for moving work off a specific node, or for replacing an "+
					"allocation that is unhealthy in a way a restart will not fix. Use "+
					"restart_allocation instead if you want the task restarted in place.\n\n"+
					"The job's count is unchanged, so the work comes back. To actually reduce how "+
					"much is running, use scale_task_group.\n\n"+
					"This interrupts whatever the allocation was serving, and the replacement takes "+
					"time to become healthy. Confirm with the user before stopping an allocation "+
					"belonging to a live service."),
			utils.MutatingTool(true, false),
			allocIDParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			allocID, namespace, alloc, nomad, _, errMsg := allocContext(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}

			resp, err := nomad.Allocations().Stop(alloc, nil)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "stop allocation " + utils.ShortID(allocID),
					Kind:       "allocation",
					Name:       allocID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "alloc-lifecycle",
					ListTool:   "list_allocations",
				}, p.Redactor()))
			}

			out := map[string]any{
				"alloc_id":  allocID,
				"short_id":  utils.ShortID(allocID),
				"namespace": namespace,
				"note": "The allocation is stopping. Because the job's count is unchanged, Nomad " +
					"will create a replacement allocation with a different ID.",
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
				out["next_step"] = "Call read_evaluation with that eval ID to see where the replacement was placed."
			}
			return utils.JSONResult(out)
		},
	}
}

// SignalAllocation sends a signal to a task.
func SignalAllocation(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("signal_allocation",
			mcp.WithDescription(
				"Send a Unix signal to a task inside a running allocation.\n\n"+
					"Most often used to make a process reload its configuration without restarting, "+
					"typically with SIGHUP. Whether a signal does anything at all depends entirely "+
					"on the program: an application that does not handle the signal will either "+
					"ignore it or die from the default action.\n\n"+
					"Be careful with termination signals. SIGTERM and SIGKILL will stop the task, "+
					"and Nomad will treat that as a task failure and apply the restart policy."),
			utils.MutatingTool(true, false),
			allocIDParam(),
			mcp.WithString("signal",
				mcp.Required(),
				mcp.Description(
					"Signal name, such as SIGHUP, SIGUSR1 or SIGTERM. SIGHUP is the usual choice "+
						"for prompting a configuration reload."),
			),
			mcp.WithString("task",
				mcp.Description("Send to this task only. If omitted, the signal goes to every task in the allocation."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			signal, err := req.RequireString("signal")
			if err != nil {
				return utils.ErrorResult("The 'signal' argument is required, for example \"SIGHUP\".")
			}

			allocID, namespace, alloc, nomad, _, errMsg := allocContext(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}

			if alloc.ClientStatus != "running" {
				return utils.ErrorResultf(
					"Allocation %s is %q, not running, so it cannot be signalled.",
					utils.ShortID(allocID), alloc.ClientStatus)
			}

			task := req.GetString("task", "")
			if task != "" {
				resolved, msg := pickTask(alloc, task)
				if msg != "" {
					return utils.ErrorResult(msg)
				}
				task = resolved
			}

			if err := nomad.Allocations().Signal(alloc, nil, task, signal); err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "send " + signal + " to allocation " + utils.ShortID(allocID),
					Kind:       "allocation",
					Name:       allocID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "alloc-lifecycle",
					ListTool:   "list_allocations",
				}, p.Redactor()))
			}

			out := map[string]any{
				"alloc_id":  allocID,
				"short_id":  utils.ShortID(allocID),
				"namespace": namespace,
				"signal":    signal,
				"task":      taskOrAll(task),
				"note": "The signal was delivered. Whether it had any effect depends on how the " +
					"program handles it; check read_allocation or the task's logs to confirm.",
			}
			if isTerminating(signal) {
				out["warning"] = "This is a terminating signal. The task will most likely stop, and " +
					"Nomad will treat that as a failure and apply the job's restart policy."
			}
			return utils.JSONResult(out)
		},
	}
}

func taskOrAll(task string) string {
	if task == "" {
		return "all tasks"
	}
	return task
}

func isTerminating(signal string) bool {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(signal), "SIG")) {
	case "TERM", "KILL", "INT", "QUIT", "ABRT":
		return true
	}
	return false
}
