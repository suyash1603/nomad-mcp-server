// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// ListRegions lists the federated regions known to the cluster.
func ListRegions(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_regions",
			mcp.WithDescription(
				"List the Nomad regions this cluster knows about.\n\n"+
					"Most clusters have exactly one region and you will not need this. "+
					"Reach for it when a job or node cannot be found and the cluster is federated, "+
					"since every other tool operates on one region at a time and defaults to the "+
					"region of the agent this server talks to."),
			utils.ReadOnlyTool(),
		),
		Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			regions, err := nomad.Regions().List()
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:      "list regions",
					Address: p.Address(),
				}, p.Redactor()))
			}

			return utils.JSONResult(utils.List{
				Count: len(regions),
				Items: regions,
			})
		},
	}
}

// nodePool is the projection returned by list_node_pools.
type nodePool struct {
	Name               string            `json:"name"`
	Description        string            `json:"description,omitempty"`
	Meta               map[string]string `json:"meta,omitempty"`
	SchedulerAlgorithm string            `json:"scheduler_algorithm,omitempty"`
}

// ListNodePools lists node pools.
func ListNodePools(p *client.Provider) server.ServerTool {
	tool := []mcp.ToolOption{
		mcp.WithDescription(
			"List the node pools defined in the cluster. A node pool is a named group of client " +
				"nodes that jobs can be targeted at, used to keep workloads on particular hardware " +
				"or to separate tenants.\n\n" +
				"Useful when a job will not place and you need to check whether its node_pool exists " +
				"and has eligible nodes in it, or when writing a job specification that targets one. " +
				"Use list_nodes to see the nodes inside a pool."),
		utils.ReadOnlyTool(),
		utils.RegionParam(),
		utils.FilterParam(`Name != "default"`),
	}
	tool = append(tool, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_node_pools", tool...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			page := utils.PageFrom(req)
			q := page.Apply(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
				Filter: req.GetString("filter", ""),
			})

			pools, meta, err := nomad.NodePools().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list node pools",
					Kind:       "node pool",
					Address:    p.Address(),
					Capability: "node:read",
				}, p.Redactor()))
			}

			items := make([]nodePool, 0, len(pools))
			for _, pool := range pools {
				if pool == nil {
					continue
				}
				out := nodePool{
					Name:        pool.Name,
					Description: pool.Description,
					Meta:        pool.Meta,
				}
				if pool.SchedulerConfiguration != nil {
					out.SchedulerAlgorithm = string(pool.SchedulerConfiguration.SchedulerAlgorithm)
				}
				items = append(items, out)
			}

			result := utils.List{Count: len(items), Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			return utils.JSONResult(result)
		},
	}
}
