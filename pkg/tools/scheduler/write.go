// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// deploymentArgs resolves the arguments shared by the deployment write tools.
func deploymentArgs(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (string, string, *api.WriteOptions, string) {
	id, err := req.RequireString("deployment_id")
	if err != nil {
		return "", "", nil, "The 'deployment_id' argument is required. Use list_deployments to find one."
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return "", "", nil, err.Error()
	}
	return id, namespace, &api.WriteOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	}, ""
}

func deploymentWriteOptions(description string, destructive, idempotent bool) []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithDescription(description),
		utils.MutatingTool(destructive, idempotent),
		mcp.WithString("deployment_id",
			mcp.Required(),
			mcp.Description("The deployment's full ID, as returned by list_deployments or list_job_deployments."),
		),
		utils.NamespaceParam(),
		utils.RegionParam(),
	}
}

// PromoteDeployment promotes a deployment's canaries.
func PromoteDeployment(p *client.Provider) server.ServerTool {
	opts := deploymentWriteOptions(
		"Promote the canaries in a deployment, allowing the rollout to continue to the rest "+
			"of the allocations.\n\n"+
			"A canary deployment deliberately stops and waits for a human decision: Nomad "+
			"places a small number of new allocations and holds there until they are promoted. "+
			"Promoting says the new version looks good and the old allocations should now be "+
			"replaced.\n\n"+
			"By default every task group is promoted. Pass task_groups to promote only some of "+
			"them, which is what you want when one group's canaries look healthy and another's "+
			"do not — the unpromoted groups stay held.\n\n"+
			"Check the canaries are actually healthy before promoting. read_deployment shows "+
			"their health, and read_allocation_logs on a canary shows what it is doing. "+
			"Promoting an unhealthy canary rolls a broken version out to everything.\n\n"+
			"Confirm with the user first: this is the decision the deployment was waiting for.",
		true, true,
	)
	opts = append(opts, mcp.WithArray("task_groups",
		mcp.Description(
			"Names of the task groups to promote. Omit to promote every group in the "+
				"deployment. read_deployment lists the groups and their canary health."),
		mcp.WithStringItems(),
	))

	return server.ServerTool{
		Tool: mcp.NewTool("promote_deployment", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, namespace, w, errMsg := deploymentArgs(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			groups := utils.StringSlice(req, "task_groups")

			var resp *api.DeploymentUpdateResponse
			if len(groups) > 0 {
				resp, _, err = nomad.Deployments().PromoteGroups(id, groups, w)
			} else {
				resp, _, err = nomad.Deployments().PromoteAll(id, w)
			}
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "promote deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			out := map[string]any{
				"deployment_id": id,
				"namespace":     namespace,
				"promoted":      true,
				"note": "The canaries were promoted. Nomad is now replacing the remaining old " +
					"allocations; watch it finish with read_deployment.",
			}
			if len(groups) > 0 {
				out["task_groups"] = groups
				out["note"] = "The canaries in " + join(groups, ", ") + " were promoted. Any other " +
					"task group in this deployment is still holding at its canaries and needs its " +
					"own promotion before the rollout can finish."
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
			}
			return utils.JSONResult(out)
		},
	}
}

// FailDeployment marks a deployment failed.
func FailDeployment(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("fail_deployment", deploymentWriteOptions(
			"Mark a deployment as failed, stopping the rollout.\n\n"+
				"Use this to abandon a rollout that is not working rather than waiting for it to "+
				"time out. If the job sets auto_revert, failing the deployment triggers an "+
				"automatic rollback to the last stable version — which is usually what you want "+
				"when a bad version is going out.\n\n"+
				"If the job does NOT set auto_revert, this stops the rollout where it is and "+
				"leaves a mix of old and new allocations running. In that case follow up with "+
				"revert_job_version to get back to a known state.\n\n"+
				"Check read_job on the job to see which case applies, and confirm with the user "+
				"before calling this: it ends a rollout that may simply be slow rather than broken.",
			true, true,
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, namespace, w, errMsg := deploymentArgs(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Deployments().Fail(id, w)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "fail deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			out := map[string]any{
				"deployment_id": id,
				"namespace":     namespace,
				"failed":        true,
				"note": "The deployment was marked failed. If the job sets auto_revert, Nomad is " +
					"rolling back to the last stable version now — confirm with list_job_versions. " +
					"If it does not, old and new allocations may both be running; use " +
					"revert_job_version to settle on one.",
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
				out["reverted_job_version"] = resp.RevertedJobVersion
			}
			return utils.JSONResult(out)
		},
	}
}

// PauseDeployment pauses or resumes a deployment's rollout.
func PauseDeployment(p *client.Provider) server.ServerTool {
	opts := deploymentWriteOptions(
		"Pause a deployment in place, or resume one that is paused.\n\n"+
			"Pausing stops Nomad making further progress on a rollout: no new allocations are "+
			"placed and no old ones are replaced. Everything currently running keeps running, "+
			"untouched. It is the tool for \"stop, I want to look at this before it goes "+
			"further\" — a rollout that is halfway through and behaving oddly, where failing it "+
			"outright would be premature.\n\n"+
			"This interrupts an in-flight rollout, so it leaves the job in a mixed state with "+
			"some allocations on the new version and some on the old. That is safe but it is not "+
			"a resting place: either resume with pause=false, or settle the job with "+
			"fail_deployment or revert_job_version. Confirm with the user before pausing "+
			"a production rollout.\n\n"+
			"A deployment that has already finished, failed or been cancelled cannot be paused.",
		true, true,
	)
	opts = append(opts, mcp.WithBoolean("pause",
		mcp.DefaultBool(true),
		mcp.Description(
			"True pauses the rollout. False resumes a paused one, and Nomad picks up where it "+
				"left off."),
	))

	return server.ServerTool{
		Tool: mcp.NewTool("pause_deployment", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, namespace, w, errMsg := deploymentArgs(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			pause := req.GetBool("pause", true)

			resp, _, err := nomad.Deployments().Pause(id, pause, w)
			if err != nil {
				verb := "pause"
				if !pause {
					verb = "resume"
				}
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         verb + " deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			note := "The rollout is paused. Nothing further will be placed or replaced until it " +
				"is resumed with pause=false, failed with fail_deployment, or the job is changed. " +
				"Allocations already running are unaffected."
			if !pause {
				note = "The rollout has resumed from where it stopped. Watch it with read_deployment."
			}

			out := map[string]any{
				"deployment_id": id,
				"namespace":     namespace,
				"paused":        pause,
				"note":          note,
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
			}
			return utils.JSONResult(out)
		},
	}
}

// UnblockDeployment unblocks a deployment blocked on its own health checks.
func UnblockDeployment(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("unblock_deployment", deploymentWriteOptions(
			"Force a blocked deployment to be considered successful, unblocking it.\n\n"+
				"A deployment blocks when its allocations have not reported healthy within "+
				"progress_deadline — typically because a service health check is failing, or "+
				"because the job's check definition is wrong rather than the workload being "+
				"broken. This declares the rollout successful anyway and lets it complete.\n\n"+
				"That is a real override, not a retry: you are overruling the health signal Nomad "+
				"was waiting on. If the allocations genuinely are unhealthy, this promotes a "+
				"broken version to being the job's stable one, and later reverts will roll back "+
				"TO it.\n\n"+
				"Read read_deployment and read_allocation_logs on a blocked allocation first, and "+
				"confirm with the user that the health check is at fault rather than the "+
				"workload. If the workload is at fault, fail_deployment is the right tool.",
			true, true,
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, namespace, w, errMsg := deploymentArgs(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Deployments().Unblock(id, w)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "unblock deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			out := map[string]any{
				"deployment_id": id,
				"namespace":     namespace,
				"unblocked":     true,
				"note": "The deployment was unblocked and counts as successful. This job version " +
					"is now the stable one, which is what a future auto-revert would roll back to — " +
					"so if the allocations really were unhealthy, fix the job rather than leaving " +
					"this version as the fallback.",
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
			}
			return utils.JSONResult(out)
		},
	}
}

// SetDeploymentAllocHealth marks individual allocations healthy or unhealthy.
func SetDeploymentAllocHealth(p *client.Provider) server.ServerTool {
	opts := deploymentWriteOptions(
		"Manually mark specific allocations in a deployment as healthy or unhealthy.\n\n"+
			"This is for a job whose update block sets health_check = \"manual\", where Nomad "+
			"deliberately does not decide health for itself and waits to be told. It also works "+
			"as an override on other health-check modes.\n\n"+
			"Marking allocations healthy lets the rollout proceed. Marking them unhealthy fails "+
			"them, which triggers auto_revert if the job sets it, and otherwise stops the "+
			"rollout — so the unhealthy path is disruptive and can take a running version down.\n\n"+
			"Get the allocation IDs from read_deployment or list_job_allocations, check them with "+
			"read_allocation_logs, and confirm with the user before marking anything unhealthy.",
		true, true,
	)
	opts = append(opts,
		mcp.WithArray("healthy_allocation_ids",
			mcp.Description(
				"Full allocation IDs to mark healthy. At least one of healthy_allocation_ids or "+
					"unhealthy_allocation_ids must be given."),
			mcp.WithStringItems(),
		),
		mcp.WithArray("unhealthy_allocation_ids",
			mcp.Description(
				"Full allocation IDs to mark unhealthy. This fails those allocations and may "+
					"trigger a rollback."),
			mcp.WithStringItems(),
		),
	)

	return server.ServerTool{
		Tool: mcp.NewTool("set_deployment_alloc_health", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, namespace, w, errMsg := deploymentArgs(ctx, req, p)
			if errMsg != "" {
				return utils.ErrorResult(errMsg)
			}

			healthy := utils.StringSlice(req, "healthy_allocation_ids")
			unhealthy := utils.StringSlice(req, "unhealthy_allocation_ids")
			if len(healthy) == 0 && len(unhealthy) == 0 {
				return utils.ErrorResult(
					"Nothing to do: give at least one allocation ID in 'healthy_allocation_ids' or " +
						"'unhealthy_allocation_ids'. read_deployment lists the allocations in this " +
						"deployment.")
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Deployments().SetAllocHealth(id, healthy, unhealthy, w)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "set allocation health on deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			note := "Allocation health was recorded. Watch what Nomad does next with read_deployment."
			if len(unhealthy) > 0 {
				note = "Allocations were marked unhealthy. If the job sets auto_revert, Nomad is " +
					"rolling back now — confirm with list_job_versions. If it does not, the rollout " +
					"has stopped and the job may be left on a mix of versions."
			}

			out := map[string]any{
				"deployment_id":    id,
				"namespace":        namespace,
				"marked_healthy":   healthy,
				"marked_unhealthy": unhealthy,
				"note":             note,
			}
			if resp != nil {
				out["eval_id"] = resp.EvalID
				if resp.RevertedJobVersion != nil {
					out["reverted_job_version"] = resp.RevertedJobVersion
				}
			}
			return utils.JSONResult(out)
		},
	}
}

// join concatenates s with sep. The deployment tools name a handful of task
// groups at most, so pulling in strings for this is not worth the import.
func join(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}
