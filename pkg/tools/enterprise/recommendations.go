// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package enterprise

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// recommendation is the projection returned by list_recommendations.
//
// Nomad returns the raw current and suggested values; the delta and the
// direction are computed here because those are what a person decides on, and
// making the model do the arithmetic is how it gets done wrong.
type recommendation struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	JobID     string `json:"job_id"`
	Group     string `json:"group"`
	Task      string `json:"task"`

	Resource string `json:"resource"`
	Current  int    `json:"current"`
	Proposed int    `json:"proposed"`
	Change   string `json:"change"`

	SubmittedAt string `json:"submitted_at,omitempty"`
}

// ListRecommendations lists Dynamic Application Sizing recommendations.
func ListRecommendations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_recommendations",
			mcp.WithDescription(
				"List the resource recommendations Nomad has produced for jobs in this cluster.\n\n"+
					"Dynamic Application Sizing watches what tasks actually consume and proposes "+
					"CPU and memory values based on it. Each recommendation names one resource on "+
					"one task, its current reservation and the proposed one.\n\n"+
					"This is the tool for \"is anything over-provisioned\" and for sizing questions "+
					"generally, because it answers them from observed usage rather than from "+
					"guesswork. Recommendations that lower a value reclaim cluster capacity; ones "+
					"that raise it usually mean a task has been running close to its limit, which "+
					"is worth knowing before it starts being OOM-killed.\n\n"+
					"Nothing here changes anything. Applying a recommendation is a separate call "+
					"to apply_recommendations, which resubmits the job and replaces its "+
					"allocations.\n\n"+
					"Requires Nomad Enterprise, and Dynamic Application Sizing must be enabled and "+
					"have gathered enough data to have an opinion."),
			utils.ReadOnlyTool(),
			utils.EnterpriseTool(),
			utils.NamespaceParam(),
			mcp.WithString("job_id",
				mcp.Description("Return only recommendations for this job."),
			),
			mcp.WithString("resource",
				mcp.Enum("CPU", "MemoryMB"),
				mcp.Description("Return only recommendations for this resource."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			recs, _, err := nomad.Recommendations().List(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list recommendations",
					Kind:       "recommendation",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job with recommendation reads",
				}, p.Redactor()))
			}

			jobFilter := req.GetString("job_id", "")
			resourceFilter := req.GetString("resource", "")

			items := make([]recommendation, 0, len(recs))
			for _, r := range recs {
				if r == nil {
					continue
				}
				if jobFilter != "" && r.JobID != jobFilter {
					continue
				}
				if resourceFilter != "" && r.Resource != resourceFilter {
					continue
				}
				// A namespace was requested, so anything the API returned from
				// another one is filtered rather than shown: the allowlist is
				// enforced on what this server hands back, not only on what it
				// asks for.
				if !p.Config().NamespaceAllowed(r.Namespace) {
					continue
				}
				items = append(items, recommendation{
					ID:          r.ID,
					Namespace:   r.Namespace,
					JobID:       r.JobID,
					Group:       r.Group,
					Task:        r.Task,
					Resource:    r.Resource,
					Current:     r.Current,
					Proposed:    r.Value,
					Change:      describeChange(r.Current, r.Value),
					SubmittedAt: utils.FormatTime(r.SubmitTime),
				})
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			switch {
			case len(items) == 0:
				result.Note = "No recommendations. Either Dynamic Application Sizing is not enabled, " +
					"or it has not gathered enough data yet, or the current reservations already " +
					"match observed usage. This is not an error."
			default:
				result.Note = "These are proposals only; nothing has changed. Applying one " +
					"resubmits the job, which replaces its allocations — see apply_recommendations."
			}
			return utils.JSONResult(result)
		},
	}
}

// describeChange states the direction and size of a proposed change in words.
func describeChange(current, proposed int) string {
	switch {
	case current == proposed:
		return "no change"
	case current == 0:
		return "set to " + itoa(proposed)
	case proposed > current:
		return "increase by " + itoa(proposed-current) + " (" +
			itoa((proposed-current)*100/current) + "% more)"
	default:
		return "decrease by " + itoa(current-proposed) + " (" +
			itoa((current-proposed)*100/current) + "% less)"
	}
}

// ApplyRecommendations applies recommendations, resubmitting the jobs.
func ApplyRecommendations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("apply_recommendations",
			mcp.WithDescription(
				"Apply resource recommendations, updating the jobs they belong to.\n\n"+
					"This is not a settings change. Applying a recommendation rewrites the job's "+
					"resource block and resubmits the job, which starts a deployment and REPLACES "+
					"its running allocations — the same disruption as any other job update. "+
					"Several recommendations against one job are applied together as a single "+
					"resubmission.\n\n"+
					"Lowering a reservation is the risky direction. The recommendation is based on "+
					"observed usage, which does not include the peak that has not happened yet; a "+
					"task cut to its steady-state memory will be OOM-killed by its next spike. "+
					"Raising a reservation is safer but needs the capacity to exist, or the job "+
					"will not place at all.\n\n"+
					"Read the recommendations with list_recommendations, show the user exactly "+
					"which jobs will be resubmitted and how each value changes, and get explicit "+
					"confirmation before calling this. Apply one job at a time on anything "+
					"production.\n\n"+
					"Requires Nomad Enterprise."),
			// Destructive: it replaces running allocations.
			utils.MutatingTool(true, false),
			utils.EnterpriseTool(),
			mcp.WithArray("recommendation_ids",
				mcp.Required(),
				mcp.Description(
					"The IDs of the recommendations to apply, from list_recommendations."),
				mcp.WithStringItems(),
			),
			mcp.WithBoolean("policy_override",
				mcp.DefaultBool(false),
				mcp.Description(
					"Override a soft-mandatory Sentinel policy that would otherwise reject the "+
						"resubmission. Requires the sentinel-override capability. Leave false unless "+
						"the user has explicitly asked to override policy."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ids := utils.StringSlice(req, "recommendation_ids")
			if len(ids) == 0 {
				return utils.ErrorResult(
					"The 'recommendation_ids' argument is required and must contain at least one ID. " +
						"Use list_recommendations to find them.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Recommendations().Apply(ids, req.GetBool("policy_override", false))
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "apply recommendations",
					Kind:       "recommendation",
					Address:    p.Address(),
					Capability: "submit-job",
				}, p.Redactor()))
			}
			if resp == nil {
				return utils.ErrorResult(
					"Nomad accepted the request but returned no result, so it is unclear which jobs " +
						"were updated. Check list_job_deployments on the affected jobs before retrying.")
			}

			updated := make([]map[string]any, 0, len(resp.UpdatedJobs))
			for _, u := range resp.UpdatedJobs {
				if u == nil {
					continue
				}
				entry := map[string]any{
					"namespace":       u.Namespace,
					"job_id":          u.JobID,
					"eval_id":         u.EvalID,
					"recommendations": u.Recommendations,
				}
				if u.Warnings != "" {
					entry["warnings"] = u.Warnings
				}
				updated = append(updated, entry)
			}

			failures := make([]map[string]any, 0, len(resp.Errors))
			for _, e := range resp.Errors {
				if e == nil {
					continue
				}
				failures = append(failures, map[string]any{
					"namespace":       e.Namespace,
					"job_id":          e.JobID,
					"recommendations": e.Recommendations,
					"error":           p.Redactor().String(e.Error),
				})
			}

			note := "The jobs above were resubmitted with the new resource values. Each is now " +
				"running a deployment that replaces its allocations — watch them with " +
				"list_job_deployments, and read_allocation_logs if one does not come back healthy."
			switch {
			case len(updated) == 0 && len(failures) > 0:
				note = "No job was updated; every application failed. See the failures below."
			case len(failures) > 0:
				note = "Some jobs were updated and some were not. The updated ones are mid-deployment " +
					"now; the failures below were left unchanged."
			}

			return utils.JSONResult(map[string]any{
				"applied_count": len(updated),
				"updated_jobs":  updated,
				"failures":      failures,
				"note":          note,
			})
		},
	}
}

// DismissRecommendations discards recommendations without applying them.
func DismissRecommendations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("dismiss_recommendations",
			mcp.WithDescription(
				"Dismiss resource recommendations without applying them.\n\n"+
					"Use this for a proposal that has been considered and rejected — a task whose "+
					"headroom is deliberate, or a job whose sizing is set by something other than "+
					"observed usage. Dismissing clears it from list_recommendations so the "+
					"remaining ones are the ones still worth deciding on.\n\n"+
					"No job is changed and no allocation is touched: this only discards the "+
					"proposal. Nomad may produce a fresh recommendation for the same task later if "+
					"usage keeps diverging from the reservation.\n\n"+
					"Dismissed recommendations cannot be recovered, so read them with "+
					"list_recommendations first. Requires Nomad Enterprise."),
			// Not destructive: no cluster state or running work is affected.
			utils.MutatingTool(false, true),
			utils.EnterpriseTool(),
			mcp.WithArray("recommendation_ids",
				mcp.Required(),
				mcp.Description("The IDs of the recommendations to dismiss, from list_recommendations."),
				mcp.WithStringItems(),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ids := utils.StringSlice(req, "recommendation_ids")
			if len(ids) == 0 {
				return utils.ErrorResult(
					"The 'recommendation_ids' argument is required and must contain at least one ID. " +
						"Use list_recommendations to find them.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.Recommendations().Delete(ids, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "dismiss recommendations",
					Kind:       "recommendation",
					Address:    p.Address(),
					Capability: "submit-job",
				}, p.Redactor()))
			}

			return utils.JSONResult(map[string]any{
				"dismissed_count": len(ids),
				"dismissed_ids":   ids,
				"note": "The recommendations were dismissed. No job was changed and nothing was " +
					"restarted. Nomad may produce new recommendations for the same tasks if their " +
					"usage keeps diverging from what they reserve.",
			})
		},
	}
}
