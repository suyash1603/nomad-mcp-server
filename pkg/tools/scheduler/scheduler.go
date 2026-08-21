// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package scheduler holds the deployment and evaluation tools.
//
// Deployments and evaluations are two halves of the same question. An
// evaluation is the scheduler deciding what should run; a deployment is the
// rollout of a service job actually happening. When something is not running
// as expected, one of the two explains it.
package scheduler

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/projection"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// ListDeployments lists deployments in a namespace.
func ListDeployments(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List deployments in a namespace. A deployment tracks the rollout of one version of a " +
				"service job: how many allocations were placed, how many became healthy, and whether " +
				"canaries are waiting for promotion.\n\n" +
				"Use this to find rollouts that are stuck. A deployment stuck in \"running\" usually " +
				"means new allocations are failing their health checks; one marked as requiring " +
				"promotion is waiting on a human and will not proceed on its own.\n\n" +
				"Only service jobs create deployments."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("deployments"),
		utils.FilterParam(`Status == "running"`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_deployments", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix:    req.GetString("prefix", ""),
				Filter:    req.GetString("filter", ""),
			})

			deps, meta, err := nomad.Deployments().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list deployments",
					Kind:       "deployment",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
				}, p.Redactor()))
			}

			items := make([]projection.Deploy, 0, len(deps))
			for _, d := range deps {
				items = append(items, projection.Deployment(d))
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if len(items) == 0 && result.Note == "" {
				result.Note = "No deployments in namespace " + namespace +
					". Batch and system jobs do not create them, and a service job that was never updated has none."
			}
			return utils.JSONResult(result)
		},
	}
}

type deploymentDetail struct {
	projection.Deploy
	Allocations []projection.AllocStub `json:"allocations,omitempty"`
	Diagnosis   string                 `json:"diagnosis,omitempty"`
}

// ReadDeployment returns one deployment with its allocations.
func ReadDeployment(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_deployment",
			mcp.WithDescription(
				"Read one deployment in detail, including the allocations it created and their "+
					"health.\n\n"+
					"Use this when a rollout is not completing. The per-group counts show how far it "+
					"got, and the allocations show which specific instances are unhealthy — from "+
					"there, read_allocation_logs on a failing one usually gives the answer.\n\n"+
					"A deployment that requires promotion is waiting on a decision, not on the "+
					"cluster: it will sit indefinitely until promote_deployment is called or it times out."),
			utils.ReadOnlyTool(),
			mcp.WithString("deployment_id",
				mcp.Required(),
				mcp.Description("The deployment's full ID, as returned by list_deployments or list_job_deployments."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("deployment_id")
			if err != nil {
				return utils.ErrorResult("The 'deployment_id' argument is required. Use list_deployments to find one.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			}

			dep, _, err := nomad.Deployments().Info(id, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read deployment " + utils.ShortID(id),
					Kind:       "deployment",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_deployments",
				}, p.Redactor()))
			}

			out := deploymentDetail{Deploy: projection.Deployment(dep)}

			// Allocations are a best-effort extra: a failure here should not
			// lose the deployment itself.
			if stubs, _, err := nomad.Deployments().Allocations(id, q); err == nil {
				for _, s := range stubs {
					out.Allocations = append(out.Allocations, projection.Alloc(s))
				}
			}

			out.Diagnosis = diagnoseDeployment(out)
			return utils.JSONResult(out)
		},
	}
}

func diagnoseDeployment(d deploymentDetail) string {
	switch d.Status {
	case "running":
		for name, g := range d.Groups {
			if g.RequiresPromote {
				return "Task group \"" + name + "\" is waiting for canary promotion. This deployment " +
					"will not progress until the canaries are promoted or it times out."
			}
			if g.Unhealthy > 0 {
				return "Task group \"" + name + "\" has unhealthy allocations, so the deployment " +
					"cannot progress. Check the allocations listed here with read_allocation_logs."
			}
			if g.Placed < g.Desired {
				return "Task group \"" + name + "\" has not placed all of its allocations. " +
					"Check list_job_evaluations for the job: this is usually a placement failure."
			}
		}
		return "This deployment is still in progress."
	case "failed":
		return "This deployment failed. If the job has auto_revert set, Nomad has already rolled " +
			"back to the previous stable version; check list_job_versions to confirm which version is live."
	case "cancelled":
		return "This deployment was cancelled, which normally means a newer version of the job was submitted before it finished."
	}
	return ""
}

// ListEvaluations lists evaluations in a namespace.
func ListEvaluations(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List scheduler evaluations in a namespace. An evaluation is the scheduler's attempt " +
				"to reconcile what jobs ask for with what the cluster can provide.\n\n" +
				"Evaluations with status \"blocked\" are the interesting ones: they mean Nomad " +
				"wanted to place work and could not, and each carries the reason. Filter for them " +
				"when investigating cluster-wide capacity problems.\n\n" +
				"If you already know which job is affected, list_job_evaluations is more direct."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.FilterParam(`Status == "blocked"  •  TriggeredBy == "job-register"`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_evaluations", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
				Filter:    req.GetString("filter", ""),
			})

			evals, meta, err := nomad.Evaluations().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list evaluations",
					Kind:       "evaluation",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
				}, p.Redactor()))
			}

			items := make([]projection.Eval, 0, len(evals))
			blocked := 0
			for _, e := range evals {
				item := projection.Evaluation(e)
				if len(item.PlacementFailed) > 0 {
					blocked++
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if result.Note == "" && blocked > 0 {
				result.Note = "Some evaluations recorded placement failures; read their explanation fields."
			}
			return utils.JSONResult(result)
		},
	}
}

type evalDetail struct {
	projection.Eval
	Allocations []projection.AllocStub `json:"allocations,omitempty"`
}

// ReadEvaluation returns one evaluation with its allocations.
func ReadEvaluation(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_evaluation",
			mcp.WithDescription(
				"Read one evaluation in detail, including any placement failures explained in plain "+
					"language and the allocations it produced.\n\n"+
					"This is where a job's scheduling problem is finally explained. When a job will "+
					"not run, the chain is: list_job_evaluations to find the evaluation, then this to "+
					"read why. The explanation field names the specific constraint, datacenter or "+
					"resource that blocked placement.\n\n"+
					"Also use this after run_job or a scale operation, both of which return an "+
					"evaluation ID: reading it tells you whether the change actually took effect."),
			utils.ReadOnlyTool(),
			mcp.WithString("eval_id",
				mcp.Required(),
				mcp.Description(
					"The evaluation's full ID. Tools that change a job return one of these, and "+
						"list_job_evaluations lists them."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("eval_id")
			if err != nil {
				return utils.ErrorResult("The 'eval_id' argument is required. Use list_job_evaluations to find one.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			}

			eval, _, err := nomad.Evaluations().Info(id, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read evaluation " + utils.ShortID(id),
					Kind:       "evaluation",
					Name:       id,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_evaluations",
				}, p.Redactor()))
			}

			out := evalDetail{Eval: projection.Evaluation(eval)}

			if stubs, _, err := nomad.Evaluations().Allocations(id, q); err == nil {
				for _, s := range stubs {
					out.Allocations = append(out.Allocations, projection.Alloc(s))
				}
			}

			return utils.JSONResult(out)
		},
	}
}
