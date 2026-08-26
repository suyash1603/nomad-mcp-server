// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/projection"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// jobArgs resolves the arguments shared by every per-job tool.
func jobArgs(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (jobID, namespace string, q *api.QueryOptions, err error) {
	jobID, err = req.RequireString("job_id")
	if err != nil {
		return "", "", nil, errRequiredJobID
	}

	namespace, err = p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return "", "", nil, err
	}

	return jobID, namespace, &api.QueryOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	}, nil
}

// errRequiredJobID is returned when job_id is missing, phrased for the model.
var errRequiredJobID = &argError{"The 'job_id' argument is required. Use list_jobs to see what exists."}

type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

// jobToolOptions builds the option list common to the per-job tools.
func jobToolOptions(description string, extra ...mcp.ToolOption) []mcp.ToolOption {
	opts := []mcp.ToolOption{
		mcp.WithDescription(description),
		utils.ReadOnlyTool(),
		mcp.WithString("job_id",
			mcp.Required(),
			mcp.Description("The job's ID, exactly as returned by list_jobs."),
		),
		utils.NamespaceParam(),
		utils.RegionParam(),
	}
	return append(opts, extra...)
}

// ListJobAllocations lists the allocations belonging to a job.
func ListJobAllocations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_job_allocations", jobToolOptions(
			"List the allocations Nomad has created for a job. An allocation is one instance of a "+
				"task group placed on one client node — this is what actually runs.\n\n"+
				"This is the main tool for finding out what a job is really doing, as opposed to what "+
				"it was asked to do. Each allocation reports its client status, which node it landed on, "+
				"per-task state, restart counts and the last event for any task that failed.\n\n"+
				"Use it when a job is unhealthy, to find the allocation ID you need for "+
				"read_allocation_logs or read_allocation. If a job has no allocations at all, nothing "+
				"was ever placed — check list_job_evaluations for the reason.",
			mcp.WithBoolean("all",
				mcp.DefaultBool(false),
				mcp.Description(
					"Include allocations from older versions of the job and from previous deployments. "+
						"Off by default, which shows only the current ones. Turn it on to investigate "+
						"what happened before the most recent change."),
			),
			utils.AllocStatusParam(),
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

			stubs, _, err := nomad.Jobs().Allocations(jobID, req.GetBool("all", false), q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list allocations for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			// Nomad's per-job allocation endpoint returns every allocation in
			// one response, so the status filter is applied here. On a job with
			// hundreds of allocations that is the difference between an answer
			// and a context window full of healthy replicas.
			wanted := utils.AllocStatusFilter(req)
			items := make([]projection.AllocStub, 0, len(stubs))
			var filtered int
			for _, s := range stubs {
				if s == nil {
					continue
				}
				if wanted != nil && !wanted(s.ClientStatus) {
					filtered++
					continue
				}
				items = append(items, projection.Alloc(s))
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			switch {
			case filtered > 0:
				result.Note = utils.FilteredOutNote(len(items), filtered, req.GetString("status", ""))
			case len(items) == 0:
				result.Note = "This job has no allocations. Nothing has been placed for it. " +
					"Run list_job_evaluations against this job: a blocked or failed evaluation will say why."
			}
			return utils.JSONResult(result)
		},
	}
}

// ListJobDeployments lists a job's deployments.
func ListJobDeployments(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_job_deployments", jobToolOptions(
			"List the deployments for a job. A deployment tracks the rollout of one job version — "+
				"how many allocations were placed, how many became healthy, and whether canaries are "+
				"waiting to be promoted.\n\n"+
				"Use this when a job update seems stuck or has not taken effect. A deployment in "+
				"\"running\" state that is not progressing usually means new allocations are failing "+
				"their health checks, or that canaries are waiting for promotion.\n\n"+
				"Only service jobs have deployments. Batch and system jobs will return nothing.",
			mcp.WithBoolean("all",
				mcp.DefaultBool(false),
				mcp.Description("Include deployments from all versions of the job rather than only the latest."),
			),
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

			deps, _, err := nomad.Jobs().Deployments(jobID, req.GetBool("all", false), q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list deployments for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			items := make([]projection.Deploy, 0, len(deps))
			for _, d := range deps {
				items = append(items, projection.Deployment(d))
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if len(items) == 0 {
				result.Note = "This job has no deployments. That is normal for batch and system jobs, " +
					"which do not use them, and for a service job that has never been updated."
			}
			return utils.JSONResult(result)
		},
	}
}

// ListJobEvaluations lists a job's evaluations.
func ListJobEvaluations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_job_evaluations", jobToolOptions(
			"List the evaluations Nomad has run for a job. An evaluation is the scheduler's attempt "+
				"to reconcile what a job asks for with what the cluster can offer.\n\n"+
				"This is the tool that answers \"why is my job not running?\". When placement fails, "+
				"the reason lives in the evaluation and nowhere else — not in the job, not in the job's "+
				"status. Each evaluation here reports its placement failures in plain language: "+
				"constraints that filtered every node out, datacenters that matched nothing, or "+
				"resources the cluster does not have.\n\n"+
				"An evaluation with status \"blocked\" means Nomad could not place the work and is "+
				"waiting for capacity to appear. Read the explanation field first.",
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

			evals, _, err := nomad.Jobs().Evaluations(jobID, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list evaluations for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			items := make([]projection.Eval, 0, len(evals))
			blocked := 0
			failures := 0
			for _, e := range evals {
				item := projection.Evaluation(e)
				if item.Status == "blocked" {
					blocked++
				}
				if len(item.PlacementFailed) > 0 {
					failures++
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			switch {
			case len(items) == 0:
				result.Note = "This job has no evaluations, which is unusual — it may have just been submitted."
			case failures > 0:
				result.Note = "Some evaluations recorded placement failures. Read their explanation fields: " +
					"that is where Nomad says why the work could not be scheduled."
			case blocked > 0:
				result.Note = "This job has blocked evaluations. Nomad is waiting for cluster capacity " +
					"to appear before it can place the work."
			}
			return utils.JSONResult(result)
		},
	}
}
