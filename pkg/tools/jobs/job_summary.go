// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

type jobSummary struct {
	JobID     string                 `json:"job_id"`
	Namespace string                 `json:"namespace"`
	Groups    map[string]groupCounts `json:"task_groups"`
	Totals    counts                 `json:"totals"`
	Children  *childCounts           `json:"children,omitempty"`
	Healthy   bool                   `json:"healthy"`
	Note      string                 `json:"note,omitempty"`
}

type groupCounts struct {
	Running  int `json:"running"`
	Starting int `json:"starting,omitempty"`
	Queued   int `json:"queued,omitempty"`
	Complete int `json:"complete,omitempty"`
	Failed   int `json:"failed,omitempty"`
	Lost     int `json:"lost,omitempty"`
	Unknown  int `json:"unknown,omitempty"`
}

type childCounts struct {
	Pending int64 `json:"pending"`
	Running int64 `json:"running"`
	Dead    int64 `json:"dead"`
}

// ReadJobSummary returns per-task-group allocation counts.
func ReadJobSummary(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_job_summary", jobToolOptions(
			"Get a job's allocation counts broken down by task group: how many are running, "+
				"starting, queued, complete, failed or lost.\n\n"+
				"This is the fastest way to answer \"is this job healthy?\" without fetching every "+
				"allocation. Queued allocations mean Nomad wants to place work and cannot; failed "+
				"allocations mean it placed work that did not survive. Either is a reason to look "+
				"further with list_job_allocations or list_job_evaluations.\n\n"+
				"For a periodic or parameterized job, this also reports how many child jobs are "+
				"pending, running and dead.",
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, q, err := jobArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			summary, _, err := nomad.Jobs().Summary(jobID, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read summary for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			out := jobSummary{
				JobID:     summary.JobID,
				Namespace: orDefault(summary.Namespace, namespace),
				Groups:    map[string]groupCounts{},
			}

			for name, tg := range summary.Summary {
				out.Groups[name] = groupCounts{
					Running:  tg.Running,
					Starting: tg.Starting,
					Queued:   tg.Queued,
					Complete: tg.Complete,
					Failed:   tg.Failed,
					Lost:     tg.Lost,
					Unknown:  tg.Unknown,
				}
				out.Totals.Running += tg.Running
				out.Totals.Starting += tg.Starting
				out.Totals.Queued += tg.Queued
				out.Totals.Complete += tg.Complete
				out.Totals.Failed += tg.Failed
				out.Totals.Lost += tg.Lost
				out.Totals.Unknown += tg.Unknown
			}

			if summary.Children != nil {
				out.Children = &childCounts{
					Pending: summary.Children.Pending,
					Running: summary.Children.Running,
					Dead:    summary.Children.Dead,
				}
			}

			out.Healthy = out.Totals.Failed == 0 && out.Totals.Queued == 0 && out.Totals.Lost == 0

			switch {
			case out.Totals.Queued > 0:
				out.Note = "This job has queued allocations: Nomad wants to place work but cannot. " +
					"Run list_job_evaluations to see which constraint or resource is blocking it."
			case out.Totals.Failed > 0:
				out.Note = "This job has failed allocations. Run list_job_allocations to find them, " +
					"then read_allocation_logs on the failing task's stderr to see why."
			}

			return utils.JSONResult(out)
		},
	}
}

type scaleStatus struct {
	JobID     string                 `json:"job_id"`
	Namespace string                 `json:"namespace"`
	Stopped   bool                   `json:"job_stopped,omitempty"`
	Groups    map[string]scaleGroup  `json:"task_groups"`
	Note      string                 `json:"note,omitempty"`
	Events    map[string][]scaleEvnt `json:"recent_scaling_events,omitempty"`
}

type scaleGroup struct {
	Desired   int  `json:"desired"`
	Placed    int  `json:"placed"`
	Running   int  `json:"running"`
	Healthy   int  `json:"healthy"`
	Unhealthy int  `json:"unhealthy"`
	Min       *int `json:"policy_min,omitempty"`
	Max       *int `json:"policy_max,omitempty"`
	Enabled   bool `json:"policy_enabled,omitempty"`
}

type scaleEvnt struct {
	Time     string `json:"time"`
	Count    *int64 `json:"count,omitempty"`
	Previous int64  `json:"previous_count"`
	Message  string `json:"message,omitempty"`
	Error    bool   `json:"error,omitempty"`
}

// GetJobScaleStatus reports current and desired counts plus scaling policy.
func GetJobScaleStatus(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_job_scale_status", jobToolOptions(
			"Get a job's current scale: the desired and running count for each task group, any "+
				"minimum and maximum from a scaling policy, and recent scaling events.\n\n"+
				"Use this before scaling a job, to see the current count and whether a scaling policy "+
				"constrains it. Also useful for understanding why a group is not at the count you "+
				"expect: the recent scaling events record who changed it and why, including failed "+
				"attempts.",
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, q, err := jobArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			status, _, err := nomad.Jobs().ScaleStatus(jobID, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read scale status for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job (or read-job-scaling)",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			out := scaleStatus{
				JobID:     status.JobID,
				Namespace: orDefault(status.Namespace, namespace),
				Stopped:   status.JobStopped,
				Groups:    map[string]scaleGroup{},
			}

			for name, tg := range status.TaskGroups {
				g := scaleGroup{
					Desired:   tg.Desired,
					Placed:    tg.Placed,
					Running:   tg.Running,
					Healthy:   tg.Healthy,
					Unhealthy: tg.Unhealthy,
				}
				out.Groups[name] = g

				for _, e := range tg.Events {
					ev := scaleEvnt{
						Time:     utils.FormatTime(int64(e.Time)),
						Count:    countPtr(e.Count),
						Previous: int64(e.PreviousCount),
						Message:  e.Message,
						Error:    e.Error,
					}
					if out.Events == nil {
						out.Events = map[string][]scaleEvnt{}
					}
					out.Events[name] = append(out.Events[name], ev)
				}
			}

			if status.JobStopped {
				out.Note = "This job is stopped, so its groups are scaled to zero regardless of the counts above."
			}

			return utils.JSONResult(out)
		},
	}
}

func countPtr(c *int64) *int64 { return c }

// compile-time guard that api types used above still exist.
var _ = api.QueryOptions{}
