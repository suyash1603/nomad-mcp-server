// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/api/contexts"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// searchContexts maps the values this tool accepts to Nomad's search contexts.
// Enterprise-only contexts (quotas, recommendations) are excluded: offering
// them would only ever yield a 501 on Community Edition.
var searchContexts = map[string]contexts.Context{
	"jobs":        contexts.Jobs,
	"allocs":      contexts.Allocs,
	"nodes":       contexts.Nodes,
	"deployments": contexts.Deployments,
	"evals":       contexts.Evals,
	"namespaces":  contexts.Namespaces,
	"node_pools":  contexts.NodePools,
	"plugins":     contexts.Plugins,
	"volumes":     contexts.Volumes,
	"vars":        contexts.Variables,
	"all":         contexts.All,
}

type searchResult struct {
	Prefix    string              `json:"prefix"`
	Namespace string              `json:"namespace,omitempty"`
	Matches   map[string][]string `json:"matches"`
	Total     int                 `json:"total_matches"`
	Truncated []string            `json:"truncated_contexts,omitempty"`
	Note      string              `json:"note,omitempty"`
}

// Search resolves a partial ID or name to real resources.
func Search(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("search",
			mcp.WithDescription(
				"Find Nomad resources whose ID or name starts with a given prefix, across jobs, "+
					"allocations, nodes, deployments, evaluations, namespaces, volumes and more.\n\n"+
					"This is how you turn a partial identifier into a real one. Nomad IDs are UUIDs and "+
					"people almost always quote the short form they saw in the CLI or UI, such as "+
					"\"3f9a1c2e\". Most other tools need the full ID, so when a user gives you a short "+
					"or partial identifier, resolve it here first rather than guessing.\n\n"+
					"Also useful when you do not know what kind of thing an identifier refers to: "+
					"search with context \"all\" and Nomad will report which category it matched.\n\n"+
					"Prefix matching only. This does not search job contents, task names or log text."),
			utils.ReadOnlyTool(),
			mcp.WithString("prefix",
				mcp.Required(),
				mcp.Description(
					"The leading characters of the ID or name to look for, for example \"3f9a1c2e\" or \"web\". "+
						"Matching is by prefix, so a fragment from the middle of an ID will not match."),
			),
			mcp.WithString("context",
				mcp.DefaultString("all"),
				mcp.Enum("all", "jobs", "allocs", "nodes", "deployments", "evals",
					"namespaces", "node_pools", "plugins", "volumes", "vars"),
				mcp.Description(
					"Which kind of resource to search. Defaults to \"all\", which searches every "+
						"category and is the right choice when you do not already know what the "+
						"identifier refers to."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return search(ctx, req, p)
		},
	}
}

func search(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	prefix, err := req.RequireString("prefix")
	if err != nil {
		return utils.ErrorResult("The 'prefix' argument is required: give the leading characters of the ID or name to search for.")
	}
	if strings.TrimSpace(prefix) == "" {
		return utils.ErrorResult("The 'prefix' argument cannot be empty.")
	}

	contextName := req.GetString("context", "all")
	searchCtx, ok := searchContexts[contextName]
	if !ok {
		return utils.ErrorResultf(
			"Unknown context %q. Valid values are: all, jobs, allocs, nodes, deployments, evals, namespaces, node_pools, plugins, volumes, vars.",
			contextName)
	}

	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	resp, _, err := nomad.Search().PrefixSearch(prefix, searchCtx, &api.QueryOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "search for " + prefix,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "read-job (plus node:read for nodes)",
		}, p.Redactor()))
	}

	out := searchResult{
		Prefix:    prefix,
		Namespace: namespace,
		Matches:   map[string][]string{},
	}

	for kind, ids := range resp.Matches {
		if len(ids) == 0 {
			continue
		}
		out.Matches[string(kind)] = ids
		out.Total += len(ids)
	}
	for kind, wasTruncated := range resp.Truncations {
		if wasTruncated {
			out.Truncated = append(out.Truncated, string(kind))
		}
	}
	sortStrings(out.Truncated)

	switch {
	case out.Total == 0:
		out.Note = "Nothing matched that prefix in namespace " + namespace +
			". The resource may be in a different namespace — try namespace \"*\" — or the prefix may be from the middle of an ID rather than the start."
	case len(out.Truncated) > 0:
		out.Note = "Some categories returned more matches than Nomad will list. Use a longer prefix to narrow the search."
	}

	return utils.JSONResult(out)
}
