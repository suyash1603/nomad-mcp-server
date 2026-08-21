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
	return server.ServerTool{
		Tool: mcp.NewTool("promote_deployment", deploymentWriteOptions(
			"Promote the canaries in a deployment, allowing the rollout to continue to the rest "+
				"of the allocations.\n\n"+
				"A canary deployment deliberately stops and waits for a human decision: Nomad "+
				"places a small number of new allocations and holds there until they are promoted. "+
				"Promoting says the new version looks good and the old allocations should now be "+
				"replaced.\n\n"+
				"Check the canaries are actually healthy before promoting. read_deployment shows "+
				"their health, and read_allocation_logs on a canary shows what it is doing. "+
				"Promoting an unhealthy canary rolls a broken version out to everything.\n\n"+
				"Confirm with the user first: this is the decision the deployment was waiting for.",
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

			resp, _, err := nomad.Deployments().PromoteAll(id, w)
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
