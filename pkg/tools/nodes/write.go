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

// RestartNodeAllocations restarts every allocation running on one client node.
//
// This is the closest thing Nomad has to "restart a client". Nomad exposes no
// endpoint that restarts a client agent — the agent is a process under the
// node's own init system, and only something on that machine can restart it.
// What people almost always mean by the request is "make everything on this
// node start again", and that is what this does, without the node leaving the
// cluster or its allocations being rescheduled elsewhere.
func RestartNodeAllocations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("restart_node_allocations",
			mcp.WithDescription(
				"Restart the tasks of every allocation running on one client node, in place.\n\n"+
					"This is what to reach for when asked to \"restart a client\" or \"restart a "+
					"node\". Nomad has no API that restarts a client agent — the agent is a process "+
					"managed by the node's own init system, so only something running on that "+
					"machine can restart it. Restarting the work on the node is the operation Nomad "+
					"does offer, and is usually what was actually wanted. Say which one you did.\n\n"+
					"Every task is restarted where it is. Nothing is rescheduled and nothing moves "+
					"to another node, so unlike drain_node this does not need spare capacity "+
					"elsewhere. It does interrupt every workload on the node at once, which on a "+
					"busy node means a lot of simultaneous downtime.\n\n"+
					"This is disruptive and cannot be undone. Run list_node_allocations first so you "+
					"and the user both know what is about to be interrupted, and get explicit "+
					"confirmation naming the node. To restart a single workload instead, use "+
					"restart_allocation.\n\n"+
					"System jobs are included unless you exclude them; on a node running log or "+
					"metrics agents you usually want to leave those alone."),
			// Destructive: it interrupts running work. Idempotent in the sense
			// that a second call restarts the same set again with the same
			// effect, but there is nothing accumulating.
			utils.MutatingTool(true, true),
			NodeIDParam(),
			mcp.WithBoolean("include_system_jobs",
				mcp.DefaultBool(false),
				mcp.Description(
					"Include allocations belonging to system jobs. Off by default: these are "+
						"usually the log shippers and metrics agents you want still running while "+
						"everything else comes back."),
			),
			mcp.WithString("task",
				mcp.Description(
					"Restart only this named task within each allocation, instead of all tasks. "+
						"Allocations that have no task by this name are skipped."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodeID, err := req.RequireString("node_id")
			if err != nil {
				return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := &api.QueryOptions{Namespace: namespace, Region: region}

			allocs, _, err := nomad.Nodes().Allocations(nodeID, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list allocations on node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "node:read",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			includeSystem := req.GetBool("include_system_jobs", false)
			task := req.GetString("task", "")

			var (
				restarted []map[string]string
				skipped   []map[string]string
				failed    []map[string]string
			)

			for _, alloc := range allocs {
				if alloc == nil {
					continue
				}
				entry := map[string]string{
					"alloc_id":   utils.ShortID(alloc.ID),
					"job_id":     alloc.JobID,
					"task_group": alloc.TaskGroup,
				}

				// Only a running allocation has tasks to restart. A complete or
				// failed one would return an error that says nothing useful.
				if alloc.ClientStatus != "running" {
					entry["reason"] = "not running (client status: " + alloc.ClientStatus + ")"
					skipped = append(skipped, entry)
					continue
				}
				if !includeSystem && alloc.Job != nil && alloc.Job.Type != nil &&
					*alloc.Job.Type == "system" {
					entry["reason"] = "belongs to a system job; pass include_system_jobs=true to restart it too"
					skipped = append(skipped, entry)
					continue
				}

				var restartErr error
				if task != "" {
					restartErr = nomad.Allocations().Restart(alloc, task, q)
				} else {
					restartErr = nomad.Allocations().RestartAllTasks(alloc, q)
				}
				if restartErr != nil {
					// One allocation failing must not abandon the rest: a
					// half-restarted node with no record of which half is worse
					// than either outcome.
					entry["error"] = utils.MapError(restartErr, utils.ErrorContext{
						Op:         "restart allocation " + utils.ShortID(alloc.ID),
						Kind:       "allocation",
						Name:       alloc.ID,
						Namespace:  namespace,
						Address:    p.Address(),
						Capability: "alloc-lifecycle",
					}, p.Redactor())
					failed = append(failed, entry)
					continue
				}
				restarted = append(restarted, entry)
			}

			note := "Tasks were asked to restart. A restart is not instant: watch them come back " +
				"with list_node_allocations on this node, and read_allocation_logs if one does not."
			switch {
			case len(allocs) == 0:
				note = "This node has no allocations, so nothing was restarted. That may itself be " +
					"the problem — check read_node to see whether the node is down, draining or " +
					"ineligible."
			case len(restarted) == 0:
				note = "Nothing was restarted: every allocation on this node was skipped or failed. " +
					"See the skipped and failed lists for why."
			case len(failed) > 0:
				note = "Some allocations restarted and some did not. The node is now in a mixed " +
					"state — see the failed list, and re-run for those specific allocations with " +
					"restart_allocation once the cause is fixed."
			}

			return utils.JSONResult(map[string]any{
				"node_id":         nodeID,
				"short_id":        utils.ShortID(nodeID),
				"namespace":       namespace,
				"total_on_node":   len(allocs),
				"restarted_count": len(restarted),
				"restarted":       restarted,
				"skipped":         skipped,
				"failed":          failed,
				"note":            note,
				"caveat": "This restarted the work on the node, not the Nomad client agent itself. " +
					"Nomad has no API for restarting an agent; that needs access to the machine.",
			})
		},
	}
}

// ForceEvaluateNode forces the scheduler to re-evaluate a node.
func ForceEvaluateNode(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("force_evaluate_node",
			mcp.WithDescription(
				"Force the scheduler to re-evaluate one client node.\n\n"+
					"Nomad creates evaluations by itself whenever something changes, so this is "+
					"rarely needed. Its use is nudging a node whose state looks correct but which "+
					"is not being given work — after a node came back up, after its drivers "+
					"finished fingerprinting, or after an eligibility change that does not appear "+
					"to have taken effect.\n\n"+
					"It queues an evaluation and nothing more: no allocation is moved, stopped or "+
					"started as a direct result. If the node still gets no work afterwards, the "+
					"reason is a real constraint and read_evaluation on the returned evaluation "+
					"will say which."),
			utils.MutatingTool(false, true),
			NodeIDParam(),
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

			evalID, _, err := nomad.Nodes().ForceEvaluate(nodeID, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "force evaluation of node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:write",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			return utils.JSONResult(map[string]any{
				"node_id":  nodeID,
				"short_id": utils.ShortID(nodeID),
				"eval_id":  evalID,
				"note": "An evaluation was queued. Read it with read_evaluation: if the node is " +
					"still not being given work, that evaluation names the constraint responsible.",
			})
		},
	}
}

// PurgeNode removes a node from Nomad's state entirely.
func PurgeNode(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("purge_node",
			mcp.WithDescription(
				"Remove a client node from Nomad's state permanently.\n\n"+
					"This is for a node that is gone for good — a terminated EC2 instance, a "+
					"decommissioned machine, a container that will not come back. Nomad keeps a "+
					"down node in its state indefinitely so that it can rejoin, and purging says "+
					"it will not.\n\n"+
					"This is irreversible and it is not a way to fix a node that is merely "+
					"unhealthy. Purging a node that is actually still alive is worse than doing "+
					"nothing: the agent re-registers on its next heartbeat, so you get the "+
					"disruption without the cleanup.\n\n"+
					"Any allocation still recorded against the node is lost from Nomad's view. If "+
					"the machine is somehow still running them, they become orphans that Nomad no "+
					"longer tracks or stops.\n\n"+
					"Before calling this: confirm with read_node that the node's status is \"down\", "+
					"drain it first if it is not, and get explicit confirmation from the user "+
					"naming the node. Never call it as speculative cleanup."),
			utils.MutatingTool(true, true),
			NodeIDParam(),
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

			q := &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}

			// Refusing to purge a live node is a guard Nomad does not provide,
			// and the failure mode without it is silent: the agent simply
			// re-registers and the operator believes the node was removed.
			var status, warning string
			if node, _, err := nomad.Nodes().Info(nodeID, q); err == nil && node != nil {
				status = node.Status
				if node.Status == "ready" {
					return utils.ErrorResultf(
						"Refused: node %s (%s) is \"ready\", which means its agent is alive and "+
							"heartbeating right now.\n\n"+
							"Purging a live node accomplishes nothing — the agent re-registers on its "+
							"next heartbeat, so the node comes back while its allocations do not. "+
							"Purge is for a node that is gone for good.\n\n"+
							"If the intent was to take this node out of service, drain_node migrates "+
							"its work away first. If the intent was to stop it taking new work, "+
							"set_node_eligibility does that without disturbing what is running. If the "+
							"machine really is being destroyed, stop the agent first and purge once "+
							"the node reports \"down\".",
						utils.ShortID(nodeID), node.Name)
				}
				if node.Status != "down" {
					warning = "The node's status was \"" + node.Status + "\" rather than \"down\" " +
						"when it was purged."
				}
			}

			resp, _, err := nomad.Nodes().Purge(nodeID, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "purge node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:write",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			out := map[string]any{
				"node_id":            nodeID,
				"short_id":           utils.ShortID(nodeID),
				"purged":             true,
				"status_when_purged": status,
				"note": "The node was removed from Nomad's state permanently. If its agent is ever " +
					"started again it will register as a new node with a new ID.",
			}
			if warning != "" {
				out["warning"] = warning
			}
			if resp != nil {
				out["eval_ids"] = resp.EvalIDs
			}
			return utils.JSONResult(out)
		},
	}
}

// SetNodeMeta sets or removes dynamic metadata on a client node.
func SetNodeMeta(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("set_node_meta",
			mcp.WithDescription(
				"Set or remove dynamic metadata on a client node.\n\n"+
					"Node metadata is what job constraints match against, as "+
					"${meta.key}. Setting it is how you make a node eligible for work that "+
					"targets a particular label — tagging a node as having a GPU, marking one as "+
					"reserved for a team, or flagging one for a canary.\n\n"+
					"Dynamic metadata is set through this API and survives an agent restart. It "+
					"is separate from static metadata, which comes from the node's own client "+
					"configuration file and cannot be changed from here; where a key exists in "+
					"both, the dynamic value wins. read_node shows the merged result.\n\n"+
					"Changing metadata changes which jobs can be placed on this node. It does not "+
					"move anything already running, but it can make queued work suddenly place "+
					"here, and it can make this node stop being a candidate for work that was "+
					"relying on the old value. Check what constrains on the key before changing it.\n\n"+
					"Pass a key with a null value in 'remove' to delete it.",
			),
			utils.MutatingTool(false, true),
			NodeIDParam(),
			mcp.WithObject("meta",
				mcp.Description(
					"Metadata keys to set, as a flat object of strings. Keys already present are "+
						"overwritten; keys not mentioned are left alone."),
			),
			mcp.WithArray("remove",
				mcp.Description("Metadata keys to delete from the node."),
				mcp.WithStringItems(),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodeID, err := req.RequireString("node_id")
			if err != nil {
				return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
			}

			set := utils.StringMap(req, "meta")
			remove := utils.StringSlice(req, "remove")
			if len(set) == 0 && len(remove) == 0 {
				return utils.ErrorResult(
					"Nothing to do: give at least one key in 'meta' to set, or one key in 'remove' " +
						"to delete. read_node shows the node's current metadata.")
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			// Nomad's dynamic-metadata API encodes deletion as a null value,
			// which is why the map is of pointers.
			meta := make(map[string]*string, len(set)+len(remove))
			for k, v := range set {
				value := v
				meta[k] = &value
			}
			for _, k := range remove {
				meta[k] = nil
			}

			resp, err := nomad.Nodes().Meta().Apply(&api.NodeMetaApplyRequest{
				NodeID: nodeID,
				Meta:   meta,
			}, &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "set metadata on node " + utils.ShortID(nodeID),
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
				"set":      set,
				"removed":  remove,
				"note": "Metadata was applied. It takes effect on the next fingerprint, which is " +
					"quick but not instantaneous, and it changes which jobs can be placed here. " +
					"Nothing already running was moved. Use force_evaluate_node if queued work " +
					"should now fit and has not been reconsidered.",
			}
			if resp != nil {
				out["meta_after"] = resp.Meta
				out["static_meta"] = resp.Static
			}
			return utils.JSONResult(out)
		},
	}
}
