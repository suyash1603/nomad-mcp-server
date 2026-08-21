// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// evalHint is appended to every tool that produces an evaluation.
//
// Nomad's write endpoints are asynchronous: they return once the request is
// accepted, not once the work is running. Without this, a model reports
// "stopped the job" the instant the call returns, which is frequently untrue —
// the evaluation may be blocked, and nothing may have happened at all.
const evalHint = "This returned an evaluation ID. The change has been accepted, NOT necessarily " +
	"applied. Call read_evaluation with that ID to find out what actually happened; a blocked " +
	"evaluation means the work could not be placed. Do not report success until you have checked."

// writeOpts builds the write options for a namespaced job operation.
func writeOpts(ctx context.Context, req mcp.CallToolRequest, p *client.Provider, namespace string) *api.WriteOptions {
	return &api.WriteOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	}
}

// RunJob submits a job.
func RunJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("run_job",
			mcp.WithDescription(
				"Submit a job to Nomad, creating it or updating it if it already exists. Accepts "+
					"HCL2 or Nomad's JSON job format.\n\n"+
					"ALWAYS run plan_job with the same specification first, and show the user what it "+
					"reports. plan_job says whether the job would actually schedule and how many "+
					"running allocations would be replaced; this tool does not tell you either of "+
					"those things before acting.\n\n"+
					"Submitting a job that already exists is an update, and for a service job that "+
					"means a rolling replacement of its running allocations. There is no separate "+
					"create-versus-update distinction to protect you.\n\n"+
					evalHint),
			utils.MutatingTool(true, false),
			jobspecParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spec, err := req.RequireString("jobspec")
			if err != nil {
				return utils.ErrorResult("The 'jobspec' argument is required.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			job, err := parseJobSpec(nomad, spec, namespace)
			if err != nil {
				return utils.ErrorResult(parseFailure(err, p))
			}

			// Whether this is a create or an update changes what the operator
			// needs to be told afterwards, so establish it before writing.
			existing, _, infoErr := nomad.Jobs().Info(deref(job.ID), &api.QueryOptions{Namespace: namespace})
			isUpdate := infoErr == nil && existing != nil

			resp, _, err := nomad.Jobs().Register(job, writeOpts(ctx, req, p, namespace))
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "submit job " + deref(job.ID),
					Kind:       "job",
					Name:       deref(job.ID),
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
				}, p.Redactor()))
			}

			action := "created"
			if isUpdate {
				action = "updated"
			}

			out := map[string]any{
				"job_id":           deref(job.ID),
				"namespace":        namespace,
				"action":           action,
				"eval_id":          resp.EvalID,
				"job_modify_index": resp.JobModifyIndex,
				"next_step":        evalHint,
			}
			if resp.Warnings != "" {
				out["warnings"] = resp.Warnings
			}
			if isUpdate {
				out["note"] = "This replaced an existing job. For a service job, its running " +
					"allocations are being rolled over now; watch progress with list_job_deployments."
			}
			return utils.JSONResult(out)
		},
	}
}

// StopJob stops or purges a job.
func StopJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("stop_job", jobWriteOptions(
			"Stop a job, which stops all of its allocations.\n\n"+
				"By default the job is stopped but kept, so its history and versions remain and it "+
				"can be started again by submitting it. With purge=true the job and all of its "+
				"history are deleted permanently and cannot be recovered.\n\n"+
				"Confirm with the user before calling this, and be explicit about which of the two "+
				"you are doing. Stopping a service job takes it out of service immediately.\n\n"+
				evalHint,
			true, false,
			mcp.WithBoolean("purge",
				mcp.DefaultBool(false),
				mcp.Description(
					"If true, delete the job and its entire history permanently rather than just "+
						"stopping it. This is irreversible. Leave false unless the user has clearly "+
						"asked for the job to be removed entirely."),
			),
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, err := jobWriteArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			purge := req.GetBool("purge", false)
			evalID, _, err := nomad.Jobs().Deregister(jobID, purge, writeOpts(ctx, req, p, namespace))
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "stop job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			out := map[string]any{
				"job_id":    jobID,
				"namespace": namespace,
				"purged":    purge,
				"eval_id":   evalID,
				"next_step": evalHint,
			}
			if purge {
				out["note"] = "The job and all of its history have been permanently deleted. This cannot be undone."
			} else {
				out["note"] = "The job is stopped but retained. Its versions remain, and submitting " +
					"it again with run_job will start it back up."
			}
			return utils.JSONResult(out)
		},
	}
}

// ScaleTaskGroup changes a task group's count.
func ScaleTaskGroup(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("scale_task_group", jobWriteOptions(
			"Change the number of allocations running for one task group.\n\n"+
				"Check get_job_scale_status first: it reports the current count and any scaling "+
				"policy minimum and maximum, and scaling outside those bounds is rejected.\n\n"+
				"Scaling down stops running allocations immediately, so confirm with the user "+
				"before reducing a count on anything that serves traffic. Scaling up may not place "+
				"anything at all if the cluster lacks capacity, which is why the resulting "+
				"evaluation matters.\n\n"+
				evalHint,
			true, true,
			mcp.WithString("task_group",
				mcp.Required(),
				mcp.Description("Name of the task group to scale, as shown by read_job or get_job_scale_status."),
			),
			mcp.WithNumber("count",
				mcp.Required(),
				mcp.Description("The desired number of allocations. Zero stops the group entirely without stopping the job."),
			),
			mcp.WithString("message",
				mcp.Description(
					"A short reason for the change, recorded in the job's scaling history. Worth "+
						"setting: it is what someone reading the history later will see."),
			),
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, err := jobWriteArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			group, err := req.RequireString("task_group")
			if err != nil {
				return utils.ErrorResult("The 'task_group' argument is required. Use get_job_scale_status to see the groups.")
			}
			count := req.GetInt("count", -1)
			if count < 0 {
				return utils.ErrorResult("The 'count' argument is required and must be zero or greater.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			message := req.GetString("message", "scaled via nomad-mcp-server")
			resp, _, err := nomad.Jobs().Scale(jobID, group, &count, message, false, nil,
				writeOpts(ctx, req, p, namespace))
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "scale task group " + group + " of job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "scale-job (or submit-job)",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			return utils.JSONResult(map[string]any{
				"job_id":     jobID,
				"task_group": group,
				"namespace":  namespace,
				"new_count":  count,
				"eval_id":    resp.EvalID,
				"next_step":  evalHint,
			})
		},
	}
}

// RevertJobVersion rolls a job back to an earlier version.
func RevertJobVersion(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("revert_job_version", jobWriteOptions(
			"Roll a job back to one of its earlier versions.\n\n"+
				"Use list_job_versions first to see what exists and what changed. Versions marked "+
				"stable completed a deployment successfully and are the safest target.\n\n"+
				"Reverting creates a NEW version whose content matches the old one; it does not "+
				"delete anything. For a service job it triggers a rolling replacement of the "+
				"running allocations, which is disruptive to whatever they are serving — confirm "+
				"with the user before reverting a live service.\n\n"+
				evalHint,
			true, false,
			mcp.WithNumber("version",
				mcp.Required(),
				mcp.Description("The job version number to revert to, as shown by list_job_versions."),
			),
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, err := jobWriteArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			version := req.GetInt("version", -1)
			if version < 0 {
				return utils.ErrorResult("The 'version' argument is required. Use list_job_versions to see the available versions.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Jobs().Revert(jobID, uint64(version), nil,
				writeOpts(ctx, req, p, namespace), "", "")
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "revert job " + jobID + " to version " + itoa(version),
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			return utils.JSONResult(map[string]any{
				"job_id":       jobID,
				"namespace":    namespace,
				"reverted_to":  version,
				"eval_id":      resp.EvalID,
				"modify_index": resp.JobModifyIndex,
				"note":         "A new job version was created with the contents of version " + itoa(version) + ". Nothing was deleted.",
				"next_step":    evalHint,
			})
		},
	}
}

// DispatchParameterizedJob dispatches an instance of a parameterized job.
func DispatchParameterizedJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("dispatch_parameterized_job", jobWriteOptions(
			"Dispatch an instance of a parameterized job, creating a child job that actually runs.\n\n"+
				"A parameterized job is a template and never runs on its own; read_job reports it "+
				"as parameterized and lists which meta keys are required. Dispatching creates a new "+
				"child job with its own ID, returned here, which is what you then track with "+
				"read_job_summary or list_job_allocations.\n\n"+
				"This is additive: it starts new work rather than changing existing work.\n\n"+
				evalHint,
			false, false,
			mcp.WithObject("meta",
				mcp.Description(
					"Meta values for this dispatch, as a flat object of string keys to string "+
						"values. read_job on the parameterized job lists which keys are required "+
						"and which are optional."),
			),
			mcp.WithString("payload",
				mcp.Description(
					"Optional payload passed to the dispatched job, which the task reads from its "+
						"dispatch payload file. Plain text; it is base64-encoded for transport."),
			),
			mcp.WithString("id_prefix",
				mcp.Description("Optional prefix for the dispatched child job's ID, to make it recognisable later."),
			),
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, err := jobWriteArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			meta := map[string]string{}
			if raw, ok := req.GetArguments()["meta"].(map[string]any); ok {
				for k, v := range raw {
					if s, ok := v.(string); ok {
						meta[k] = s
					}
				}
			}

			var payload []byte
			if s := req.GetString("payload", ""); s != "" {
				payload = []byte(s)
			}

			resp, _, err := nomad.Jobs().Dispatch(jobID, meta, payload,
				req.GetString("id_prefix", ""), writeOpts(ctx, req, p, namespace))
			if err != nil {
				return utils.ErrorResult(dispatchFailure(err, p, jobID, namespace))
			}

			return utils.JSONResult(map[string]any{
				"parameterized_job_id": jobID,
				"dispatched_job_id":    resp.DispatchedJobID,
				"namespace":            namespace,
				"eval_id":              resp.EvalID,
				"note": "A child job " + resp.DispatchedJobID + " was created. Track that ID, not " +
					"the parameterized job's, to see whether the work ran.",
				"next_step": evalHint,
			})
		},
	}
}

// dispatchFailure explains the common mistake of dispatching a normal job.
func dispatchFailure(err error, p *client.Provider, jobID, namespace string) string {
	msg := utils.MapError(err, utils.ErrorContext{
		Op:         "dispatch job " + jobID,
		Kind:       "job",
		Name:       jobID,
		Namespace:  namespace,
		Address:    p.Address(),
		Capability: "dispatch-job (or submit-job)",
		ListTool:   "list_jobs",
	}, p.Redactor())

	if strings.Contains(strings.ToLower(msg), "not a parameterized") {
		msg += "\n\nOnly a job with a parameterized block can be dispatched. Use read_job to check: " +
			"if it does not report a parameterized section, submit it with run_job instead."
	}
	return msg
}

// ForcePeriodicJob triggers a periodic job immediately.
func ForcePeriodicJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("force_periodic_job", jobWriteOptions(
			"Force a periodic job to run now, without waiting for its next scheduled time.\n\n"+
				"This creates a child job immediately, in addition to the normal schedule; it does "+
				"not change or skip the schedule itself. The returned child job ID is what to "+
				"track, since the periodic job itself never has allocations of its own.\n\n"+
				"If the job sets prohibit_overlap and an instance is still running, this will be "+
				"refused.\n\n"+
				evalHint,
			false, false,
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, err := jobWriteArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			childID, _, err := nomad.Jobs().PeriodicForce(jobID, writeOpts(ctx, req, p, namespace))
			if err != nil {
				msg := utils.MapError(err, utils.ErrorContext{
					Op:         "force periodic job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_jobs",
				}, p.Redactor())
				if strings.Contains(strings.ToLower(msg), "not periodic") {
					msg += "\n\nOnly a job with a periodic block can be forced. Use read_job to check."
				}
				return utils.ErrorResult(msg)
			}

			return utils.JSONResult(map[string]any{
				"periodic_job_id": jobID,
				"child_job_id":    childID,
				"namespace":       namespace,
				"note": "Child job " + childID + " was created and is running now. The normal " +
					"schedule is unchanged.",
				"next_step": "Track the child job with read_job_summary or list_job_allocations.",
			})
		},
	}
}

// jobWriteOptions builds the option list for a per-job write tool.
func jobWriteOptions(description string, destructive, idempotent bool, extra ...mcp.ToolOption) []mcp.ToolOption {
	opts := []mcp.ToolOption{
		mcp.WithDescription(description),
		utils.MutatingTool(destructive, idempotent),
		mcp.WithString("job_id",
			mcp.Required(),
			mcp.Description("The job's ID, exactly as returned by list_jobs."),
		),
		utils.NamespaceParam(),
		utils.RegionParam(),
	}
	return append(opts, extra...)
}

// jobWriteArgs resolves job_id and namespace for a write tool.
func jobWriteArgs(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (string, string, error) {
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return "", "", errRequiredJobID
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return "", "", err
	}
	return jobID, namespace, nil
}
