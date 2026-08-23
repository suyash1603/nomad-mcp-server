// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package system holds the cluster-wide read tools: leadership, membership,
// regions, node pools, agent configuration and prefix search.
package system

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// clusterStatus is the projection returned by get_cluster_status.
type clusterStatus struct {
	Leader         string       `json:"leader"`
	Edition        string       `json:"edition"`
	Region         string       `json:"region"`
	Datacenter     string       `json:"datacenter,omitempty"`
	Servers        []serverInfo `json:"servers"`
	ServerCount    int          `json:"server_count"`
	Nodes          nodeSummary  `json:"clients"`
	Regions        []string     `json:"regions,omitempty"`
	Versions       []string     `json:"versions,omitempty"`
	Namespaces     []string     `json:"namespaces,omitempty"`
	Pools          []string     `json:"node_pools,omitempty"`
	LicenseExpires string       `json:"license_expires,omitempty"`
	Degraded       bool         `json:"degraded"`
	Warnings       []string     `json:"warnings,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Status  string `json:"status"`
	Region  string `json:"region,omitempty"`
	DC      string `json:"datacenter,omitempty"`
	Version string `json:"version,omitempty"`
	Leader  bool   `json:"leader,omitempty"`
}

// nodeSummary counts client nodes by the states that matter for triage rather
// than listing every node, which get_cluster_status is not for.
type nodeSummary struct {
	Total       int            `json:"total"`
	ByStatus    map[string]int `json:"by_status"`
	Draining    int            `json:"draining,omitempty"`
	Ineligible  int            `json:"ineligible,omitempty"`
	Datacenters []string       `json:"datacenters,omitempty"`
}

// GetClusterStatus reports overall cluster health in one call.
func GetClusterStatus(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_cluster_status",
			mcp.WithDescription(
				"Get an overview of the Nomad cluster's health in a single call: who the leader is, "+
					"which servers are alive, how many client nodes exist and what state they are in, "+
					"the Nomad versions in use, whether it is Community or Enterprise, and the "+
					"available regions, namespaces and node pools.\n\n"+
					"Reach for this first when asked anything about the cluster as a whole, when a user reports "+
					"that \"Nomad is broken\", or before diagnosing why work is not being scheduled. "+
					"A missing leader, a server that is not alive, or client nodes that are down or draining "+
					"explain most cluster-wide scheduling problems.\n\n"+
					"Returns a summary, not a full inventory: use list_nodes for individual client nodes "+
					"and list_jobs for workloads."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return getClusterStatus(ctx, req, p)
		},
	}
}

func getClusterStatus(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	q := &api.QueryOptions{Region: region}

	status := clusterStatus{
		Nodes: nodeSummary{ByStatus: map[string]int{}},
	}

	// Leadership. This is the one call whose failure is fatal to the whole
	// tool: without a leader nothing else is meaningful.
	leader, err := nomad.Status().Leader()
	if err != nil {
		if utils.IsForbidden(err) || utils.IsNotFound(err) {
			return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
				Op:         "read cluster status",
				Address:    p.Address(),
				Capability: "node:read",
			}, p.Redactor()))
		}
		// A connection-level failure is the common case and deserves the
		// dedicated message.
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:      "read cluster status",
			Address: p.Address(),
		}, p.Redactor()))
	}
	status.Leader = leader

	// The edition decides whether the quota, Sentinel, licence and
	// recommendation tools can work at all, so it belongs in the overview
	// rather than behind a separate call. The probe is cached and cheap.
	edition := p.Edition(ctx)
	status.Edition = string(edition.Edition)
	if edition.Edition == client.EditionEnterprise && edition.LicenseExpires != "" {
		status.LicenseExpires = edition.LicenseExpires
	}

	if leader == "" {
		status.Degraded = true
		status.Warnings = append(status.Warnings,
			"The cluster has no leader. Nothing can be scheduled until a leader is elected. "+
				"Check that a quorum of servers is running and can reach each other.")
	}

	// Server membership. Each remaining call is best-effort: a token scoped to
	// jobs can read some of this and not the rest, and a partial picture is
	// more useful than a blanket failure.
	if members, err := nomad.Agent().MembersOpts(q); err == nil && members != nil {
		status.Region = members.ServerRegion
		status.Datacenter = members.ServerDC

		versions := map[string]bool{}
		for _, m := range members.Members {
			if m == nil {
				continue
			}
			info := serverInfo{
				Name:    m.Name,
				Address: m.Addr,
				Status:  m.Status,
				Region:  m.Tags["region"],
				DC:      m.Tags["dc"],
				Version: m.Tags["build"],
			}
			// Nomad advertises the RPC port in tags; the leader string is
			// host:rpc_port, so this is how a member is matched to it.
			if port, ok := m.Tags["port"]; ok && leader != "" {
				if m.Addr+":"+port == leader {
					info.Leader = true
				}
			}
			if info.Version != "" {
				versions[info.Version] = true
			}
			if m.Status != "alive" {
				status.Degraded = true
				status.Warnings = append(status.Warnings,
					"Server "+m.Name+" is "+m.Status+", not alive.")
			}
			status.Servers = append(status.Servers, info)
		}
		status.ServerCount = len(status.Servers)
		status.Versions = keys(versions)

		if len(status.Versions) > 1 {
			status.Warnings = append(status.Warnings,
				"Servers are running mixed Nomad versions "+join(status.Versions, ", ")+
					". This is expected during an upgrade and a problem if it is not one.")
		}
	} else if err != nil {
		status.Warnings = append(status.Warnings, bestEffortNote("server membership", err, p))
	}

	// Client nodes.
	if nodes, _, err := nomad.Nodes().List(q); err == nil {
		dcs := map[string]bool{}
		for _, n := range nodes {
			if n == nil {
				continue
			}
			status.Nodes.Total++
			status.Nodes.ByStatus[n.Status]++
			if n.Drain {
				status.Nodes.Draining++
			}
			if n.SchedulingEligibility == "ineligible" {
				status.Nodes.Ineligible++
			}
			if n.Datacenter != "" {
				dcs[n.Datacenter] = true
			}
		}
		status.Nodes.Datacenters = keys(dcs)

		if status.Nodes.Total == 0 {
			status.Degraded = true
			status.Warnings = append(status.Warnings,
				"The cluster has no client nodes registered, so no work can be placed.")
		}
		if down := status.Nodes.ByStatus["down"]; down > 0 {
			status.Degraded = true
			status.Warnings = append(status.Warnings,
				plural(down, "client node is", "client nodes are")+" down.")
		}
		if status.Nodes.Draining > 0 {
			status.Warnings = append(status.Warnings,
				plural(status.Nodes.Draining, "client node is", "client nodes are")+
					" draining, so allocations are being migrated off "+
					plural2(status.Nodes.Draining, "it", "them")+".")
		}
	} else {
		status.Warnings = append(status.Warnings, bestEffortNote("client nodes", err, p))
	}

	if regions, err := nomad.Regions().List(); err == nil {
		status.Regions = regions
	}

	if namespaces, _, err := nomad.Namespaces().List(q); err == nil {
		for _, ns := range namespaces {
			if ns != nil && p.Config().NamespaceAllowed(ns.Name) {
				status.Namespaces = append(status.Namespaces, ns.Name)
			}
		}
	}

	if pools, _, err := nomad.NodePools().List(q); err == nil {
		for _, pool := range pools {
			if pool != nil {
				status.Pools = append(status.Pools, pool.Name)
			}
		}
	}

	return utils.JSONResult(status)
}

// bestEffortNote explains a sub-query that failed without failing the tool.
func bestEffortNote(what string, err error, p *client.Provider) string {
	return "Could not read " + what + ": " +
		utils.MapError(err, utils.ErrorContext{
			Op:      "read " + what,
			Address: p.Address(),
		}, p.Redactor())
}

func keys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings is a small insertion sort; these slices hold a handful of
// entries, so pulling in sort for them is not worth the import.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

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

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
