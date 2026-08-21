// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package nodes

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// DrainNode starts or stops draining a client node.
func DrainNode(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("drain_node",
			mcp.WithDescription(
				"Drain a client node: mark it ineligible and migrate its allocations to other "+
					"nodes, or cancel a drain already in progress.\n\n"+
					"This is a disruptive cluster operation. Every allocation on the node is "+
					"rescheduled, and if the rest of the cluster lacks capacity to take them, that "+
					"work simply stops running. Check list_node_allocations to see what is on the "+
					"node, and list_nodes to confirm somewhere else can take it, before draining.\n\n"+
					"Always confirm with the user first. This is the sort of operation that empties "+
					"a production node.\n\n"+
					"Set enable=false to cancel a drain and make the node eligible again."),
			// Not idempotent: `deadline` is relative to when the drain is
			// issued, so re-draining an already-draining node pushes its forced
			// eviction out by another full deadline. A client that skips
			// re-confirmation on an "idempotent" retry would be moving a
			// production node's eviction clock without asking.
			utils.MutatingTool(true, false),
			NodeIDParam(),
			mcp.WithBoolean("enable",
				mcp.DefaultBool(true),
				mcp.Description(
					"True starts the drain. False cancels a drain in progress and marks the node "+
						"eligible for new work again."),
			),
			mcp.WithString("deadline",
				mcp.DefaultString("1h"),
				mcp.Description(
					"How long to allow for graceful migration, as a Go duration such as \"1h\" or "+
						"\"15m\". When it expires, remaining allocations are stopped whether or not "+
						"replacements are healthy. Use \"0s\" to force them out immediately, which "+
						"is abrupt."),
			),
			mcp.WithBoolean("ignore_system_jobs",
				mcp.DefaultBool(false),
				mcp.Description(
					"If true, leave system jobs running on the node while everything else is "+
						"migrated. Useful when the system jobs are log or metrics agents you want "+
						"running until the node actually goes away."),
			),
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

			w := &api.WriteOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}
			enable := req.GetBool("enable", true)

			opts := &api.DrainOptions{}
			if enable {
				deadlineStr := req.GetString("deadline", "1h")
				deadline, err := time.ParseDuration(deadlineStr)
				if err != nil {
					return utils.ErrorResultf(
						"Invalid deadline %q: use a Go duration such as \"1h\", \"30m\" or \"0s\".",
						deadlineStr)
				}
				opts.DrainSpec = &api.DrainSpec{
					Deadline:         deadline,
					IgnoreSystemJobs: req.GetBool("ignore_system_jobs", false),
				}
			} else {
				// A nil DrainSpec cancels the drain; MarkEligible decides
				// whether the node can take work again afterwards.
				opts.MarkEligible = true
			}

			resp, err := nomad.Nodes().UpdateDrainOpts(nodeID, opts, w)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "drain node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:write",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			out := map[string]any{
				"node_id":  nodeID,
				"short_id": utils.ShortID(nodeID),
				"draining": enable,
			}
			if resp != nil {
				out["eval_ids"] = resp.EvalIDs
			}
			if enable {
				out["note"] = "The drain has started. Allocations are being migrated now, which " +
					"takes time. Watch progress with list_node_allocations on this node, and check " +
					"that the work landed somewhere with list_job_allocations for the affected jobs."
			} else {
				out["note"] = "The drain was cancelled and the node is eligible for new work again. " +
					"Allocations already migrated away do not come back on their own."
			}
			return utils.JSONResult(out)
		},
	}
}

// SetNodeEligibility toggles whether a node accepts new work.
func SetNodeEligibility(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("set_node_eligibility",
			mcp.WithDescription(
				"Mark a client node eligible or ineligible for new allocations.\n\n"+
					"Marking a node ineligible stops the scheduler placing NEW work on it while "+
					"leaving everything already running exactly where it is. That makes it the "+
					"gentle option: use it to quarantine a suspect node, or to stop work landing on "+
					"a node you are about to drain.\n\n"+
					"This is not a drain. Nothing is migrated and nothing is interrupted. Use "+
					"drain_node when you need the node actually emptied."),
			utils.MutatingTool(false, true),
			NodeIDParam(),
			mcp.WithBoolean("eligible",
				mcp.Required(),
				mcp.Description(
					"True to allow new allocations on this node, false to stop the scheduler using it."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodeID, err := req.RequireString("node_id")
			if err != nil {
				return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
			}
			eligible, err := req.RequireBool("eligible")
			if err != nil {
				return utils.ErrorResult("The 'eligible' argument is required: true to allow new work, false to stop it.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.Nodes().ToggleEligibility(nodeID, eligible, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "set eligibility on node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:write",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			note := "The node is ineligible. Existing allocations keep running untouched; only new " +
				"placements are blocked."
			if eligible {
				note = "The node is eligible again and the scheduler may place new work on it."
			}

			return utils.JSONResult(map[string]any{
				"node_id":  nodeID,
				"short_id": utils.ShortID(nodeID),
				"eligible": eligible,
				"note":     note,
			})
		},
	}
}
