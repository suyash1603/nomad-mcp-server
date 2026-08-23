// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// nodePoolDetail is the projection returned by read_node_pool. It is deeper
// than the list projection because the question being asked is different: a
// list answers "what pools exist", and this answers "why will nothing place in
// this one".
type nodePoolDetail struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`

	SchedulerAlgorithm     string `json:"scheduler_algorithm,omitempty"`
	MemoryOversubscription *bool  `json:"memory_oversubscription_enabled,omitempty"`

	Nodes    nodePoolNodes `json:"nodes"`
	JobCount int           `json:"job_count"`

	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// nodePoolNodes counts the pool's nodes by the states that decide whether work
// can land in it.
type nodePoolNodes struct {
	Total      int            `json:"total"`
	Ready      int            `json:"ready"`
	ByStatus   map[string]int `json:"by_status,omitempty"`
	Draining   int            `json:"draining,omitempty"`
	Ineligible int            `json:"ineligible,omitempty"`
	Names      []string       `json:"names,omitempty"`
}

// ReadNodePool reads one node pool, with the state of the nodes in it.
func ReadNodePool(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_node_pool",
			mcp.WithDescription(
				"Read one node pool: its description, metadata, scheduler configuration, how many "+
					"client nodes are in it and what state they are in, and how many jobs target it.\n\n"+
					"This is the tool for \"the job says node_pool = gpu and nothing is placing\". A "+
					"pool with no nodes, or whose nodes are all down, draining or ineligible, "+
					"explains that immediately — and the answer is reported here as a warning rather "+
					"than left for you to infer from counts.\n\n"+
					"scheduler_algorithm and memory_oversubscription_enabled are per-pool overrides "+
					"and only appear on Nomad Enterprise; their absence on Community Edition is "+
					"normal, not a fault."),
			utils.ReadOnlyTool(),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The node pool's name. Use list_node_pools to see what exists."),
			),
			mcp.WithBoolean("include_node_names",
				mcp.DefaultBool(false),
				mcp.Description(
					"Include the name of every node in the pool. Off by default because a large "+
						"pool would fill the context; the counts are usually enough."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult(
					"The 'name' argument is required. Use list_node_pools to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}

			pool, _, err := nomad.NodePools().Info(name, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read node pool " + name,
					Kind:       "node pool",
					Name:       name,
					Address:    p.Address(),
					Capability: "node:read",
					ListTool:   "list_node_pools",
				}, p.Redactor()))
			}
			if pool == nil {
				return utils.ErrorResultf(
					"No node pool named %q exists. Use list_node_pools to see what does.", name)
			}

			out := nodePoolDetail{
				Name:        pool.Name,
				Description: pool.Description,
				Meta:        pool.Meta,
				Nodes:       nodePoolNodes{ByStatus: map[string]int{}},
			}
			if pool.SchedulerConfiguration != nil {
				out.SchedulerAlgorithm = string(pool.SchedulerConfiguration.SchedulerAlgorithm)
				out.MemoryOversubscription = pool.SchedulerConfiguration.MemoryOversubscriptionEnabled
			}

			// Membership and job count are best-effort: a token that can read
			// the pool may not be able to list its nodes, and a partial answer
			// beats failing the whole call.
			includeNames := req.GetBool("include_node_names", false)
			if nodes, _, err := nomad.NodePools().ListNodes(name, q); err == nil {
				for _, n := range nodes {
					if n == nil {
						continue
					}
					out.Nodes.Total++
					out.Nodes.ByStatus[n.Status]++
					if n.Drain {
						out.Nodes.Draining++
					}
					if n.SchedulingEligibility == "ineligible" {
						out.Nodes.Ineligible++
					}
					if n.Status == "ready" && !n.Drain && n.SchedulingEligibility == "eligible" {
						out.Nodes.Ready++
					}
					if includeNames {
						out.Nodes.Names = append(out.Nodes.Names, n.Name)
					}
				}
			} else {
				out.Warnings = append(out.Warnings, bestEffortNote("the pool's nodes", err, p))
			}

			if jobs, _, err := nomad.NodePools().ListJobs(name, q); err == nil {
				out.JobCount = len(jobs)
			}

			out.Warnings = append(out.Warnings, nodePoolWarnings(name, out.Nodes, out.JobCount)...)
			out.Note = "Nodes are counted as ready only when they are up, not draining and " +
				"eligible. Use list_nodes with a filter on NodePool for the individual nodes."

			return utils.JSONResult(out)
		},
	}
}

// nodePoolWarnings turns the counts into the sentence a person actually wants.
//
// The counts alone are enough to diagnose an unplaceable job, but only if
// someone does the arithmetic. Saying it outright is the difference between the
// model reporting "the pool has 3 nodes" and reporting why nothing is running.
func nodePoolWarnings(name string, n nodePoolNodes, jobs int) []string {
	var w []string

	switch {
	case n.Total == 0:
		w = append(w, "This node pool contains no client nodes, so nothing targeting it can "+
			"ever be placed. Nodes join a pool through the node_pool setting in their client "+
			"configuration, which means fixing this is a change on the nodes, not here.")

	case n.Ready == 0:
		reasons := make([]string, 0, 3)
		if down := n.ByStatus["down"]; down > 0 {
			reasons = append(reasons, plural(down, "is down", "are down"))
		}
		if n.Draining > 0 {
			reasons = append(reasons, plural(n.Draining, "is draining", "are draining"))
		}
		if n.Ineligible > 0 {
			reasons = append(reasons, plural(n.Ineligible, "is ineligible", "are ineligible"))
		}
		msg := "No node in this pool can accept new work"
		if len(reasons) > 0 {
			msg += ": " + join(reasons, ", ")
		}
		w = append(w, msg+". Jobs targeting this pool will stay queued until that changes.")

	case n.Ready < n.Total:
		w = append(w, plural(n.Total-n.Ready, "node in this pool cannot accept new work",
			"nodes in this pool cannot accept new work")+
			" because they are down, draining or ineligible.")
	}

	if jobs > 0 && n.Ready == 0 {
		w = append(w, plural(jobs, "job targets this pool and cannot place",
			"jobs target this pool and cannot place")+
			". Use list_jobs and list_job_evaluations to confirm.")
	}
	if name == "all" || name == "default" {
		w = append(w, "\""+name+"\" is one of Nomad's built-in pools and cannot be deleted "+
			"or renamed.")
	}
	return w
}

// builtinNodePools are the two pools Nomad creates itself. Neither can be
// deleted, and "all" cannot be written to at all.
var builtinNodePools = map[string]bool{"all": true, "default": true}

// CreateNodePool creates or updates a node pool.
func CreateNodePool(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("create_node_pool",
			mcp.WithDescription(
				"Create a node pool, or update an existing one's description, metadata and "+
					"scheduler configuration.\n\n"+
					"A node pool is a named group of client nodes that jobs target with "+
					"node_pool = \"name\". Creating one is additive and safe: it does not move any "+
					"workload and does not change where anything currently runs.\n\n"+
					"Creating the pool does NOT put any nodes in it. Nodes join a pool through the "+
					"node_pool setting in their own client configuration, which is a change on the "+
					"node, not something any API can do. Expect read_node_pool to report zero nodes "+
					"immediately after this, and say so rather than treating it as a failure.\n\n"+
					"This is an upsert: calling it with an existing pool's name REPLACES that "+
					"pool's configuration rather than failing. Read the current state with "+
					"read_node_pool first if you are modifying rather than creating.\n\n"+
					"scheduler_algorithm and memory_oversubscription are Nomad Enterprise features. "+
					"Setting either against Community Edition is refused by Nomad."),
			// Not destructive: it neither discards state nor interrupts work.
			// Idempotent: the same call twice leaves the same pool.
			utils.MutatingTool(false, true),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description(
					"The pool's name. Lowercase alphanumerics, dashes and underscores, up to 128 "+
						"characters. Cannot be \"all\", which Nomad reserves."),
			),
			mcp.WithString("description",
				mcp.Description("A human-readable description of what this pool is for."),
			),
			mcp.WithObject("meta",
				mcp.Description("Arbitrary key/value metadata, as a flat object of strings."),
			),
			mcp.WithString("scheduler_algorithm",
				mcp.Enum("binpack", "spread"),
				mcp.Description(
					"ENTERPRISE ONLY. Override the cluster scheduler algorithm for this pool. "+
						"\"binpack\" packs work onto as few nodes as possible; \"spread\" distributes "+
						"it. Omit to inherit the cluster setting."),
			),
			mcp.WithBoolean("memory_oversubscription",
				mcp.Description(
					"ENTERPRISE ONLY. Allow tasks in this pool to exceed their memory reservation "+
						"up to memory_max. Omit to inherit the cluster setting."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return utils.ErrorResult("The 'name' argument cannot be empty.")
			}
			if name == "all" {
				return utils.ErrorResult(
					"\"all\" is a built-in node pool that Nomad reserves to mean every node, and it " +
						"cannot be created or modified. Choose a different name.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			existing, _, infoErr := nomad.NodePools().Info(name, &api.QueryOptions{Region: region})
			isUpdate := infoErr == nil && existing != nil

			pool := &api.NodePool{
				Name:        name,
				Description: req.GetString("description", ""),
			}
			if raw, ok := req.GetArguments()["meta"].(map[string]any); ok {
				pool.Meta = map[string]string{}
				for k, v := range raw {
					if s, ok := v.(string); ok {
						pool.Meta[k] = s
					}
				}
			}

			// The scheduler block is only sent when the caller asked for
			// something. An empty block would otherwise be rejected on
			// Community Edition for a setting nobody requested.
			algorithm := strings.TrimSpace(req.GetString("scheduler_algorithm", ""))
			_, hasOversubscribe := req.GetArguments()["memory_oversubscription"]
			if algorithm != "" || hasOversubscribe {
				sc := &api.NodePoolSchedulerConfiguration{}
				if algorithm != "" {
					switch algorithm {
					case "binpack", "spread":
						sc.SchedulerAlgorithm = api.SchedulerAlgorithm(algorithm)
					default:
						return utils.ErrorResultf(
							"Invalid scheduler_algorithm %q: it must be \"binpack\" or \"spread\".",
							algorithm)
					}
				}
				if hasOversubscribe {
					sc.MemoryOversubscriptionEnabled = utils.BoolPtr(
						req.GetBool("memory_oversubscription", false))
				}
				pool.SchedulerConfiguration = sc
			}

			if _, err := nomad.NodePools().Register(pool, &api.WriteOptions{Region: region}); err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "create node pool " + name,
					Kind:       "node pool",
					Name:       name,
					Address:    p.Address(),
					Capability: "node:write",
				}, p.Redactor()))
			}

			action, note := "created", "The node pool exists but is empty. Nodes join it through "+
				"the node_pool setting in their client configuration; until one does, nothing "+
				"targeting this pool can be placed."
			if isUpdate {
				action = "updated"
				note = "A pool with this name already existed, so its configuration was replaced. " +
					"Nodes already in the pool stay in it and nothing was rescheduled."
			}

			return utils.JSONResult(map[string]any{
				"name":   name,
				"action": action,
				"note":   note,
			})
		},
	}
}

// DeleteNodePool deletes an empty node pool.
func DeleteNodePool(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("delete_node_pool",
			mcp.WithDescription(
				"Delete a node pool permanently.\n\n"+
					"This is irreversible. Nomad refuses to delete a pool that still has nodes or "+
					"jobs in it, which is a real safeguard — but it does not check job "+
					"specifications that merely name the pool. A job whose node_pool points at a "+
					"deleted pool stops being placeable, and the failure shows up as a queued "+
					"evaluation rather than an error on this call.\n\n"+
					"Before calling this: run read_node_pool to see what is in it, and get explicit "+
					"confirmation from the user naming the pool. Do not call it as cleanup you "+
					"inferred was wanted.\n\n"+
					"The built-in \"default\" and \"all\" pools cannot be deleted."),
			utils.MutatingTool(true, true),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The node pool to delete. Must contain no nodes and no jobs."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			name = strings.TrimSpace(name)
			if builtinNodePools[name] {
				return utils.ErrorResultf(
					"The %q node pool is built into Nomad and cannot be deleted.", name)
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.NodePools().Delete(name, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				msg := utils.MapError(err, utils.ErrorContext{
					Op:         "delete node pool " + name,
					Kind:       "node pool",
					Name:       name,
					Address:    p.Address(),
					Capability: "node:write",
					ListTool:   "list_node_pools",
				}, p.Redactor())
				if lower := strings.ToLower(msg); strings.Contains(lower, "has nodes") ||
					strings.Contains(lower, "has jobs") || strings.Contains(lower, "non-terminal") {
					msg += "\n\nNomad will not delete a node pool that still holds nodes or jobs. " +
						"read_node_pool shows both. Nodes leave a pool only by changing their own " +
						"client configuration."
				}
				return utils.ErrorResult(msg)
			}

			return utils.JSONResult(map[string]any{
				"name":    name,
				"deleted": true,
				"note": "The node pool was deleted permanently. Any job specification still naming " +
					"it will now fail to place; check list_jobs if that is a possibility.",
			})
		},
	}
}
