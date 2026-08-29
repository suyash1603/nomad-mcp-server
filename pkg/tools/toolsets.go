// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/allocs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/capacity"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/catalog"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/diag"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/enterprise"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/investigate"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/jobs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/nodes"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/scheduler"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/system"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/variables"
)

// Toolset names, as they are written in NOMAD_MCP_TOOLSETS.
const (
	ToolsetSystem      = "system"
	ToolsetJobs        = "jobs"
	ToolsetAllocs      = "allocs"
	ToolsetNodes       = "nodes"
	ToolsetDeployments = "deployments"
	ToolsetCatalog     = "catalog"
	ToolsetVariables   = "variables"
	ToolsetInvestigate = "investigate"
	ToolsetCapacity    = "capacity"
	ToolsetEnterprise  = "enterprise"
	ToolsetDiag        = "diag"
)

// ToolsetAll selects every toolset. It is the default, so a server started with
// no configuration offers the whole catalog exactly as it did before toolsets
// existed.
const ToolsetAll = "all"

// Toolset is one named group of tools, with its tools built.
type Toolset struct {
	// Name is the identifier used in NOMAD_MCP_TOOLSETS.
	Name string

	// Summary is one line describing what the group covers. It appears in the
	// error message for an unknown name and in the startup log.
	Summary string

	// Tools is every tool in the group, read and write alike. Write access is a
	// separate axis: the read-only gate decides whether a mutating tool may
	// run, and the toolset decides whether it is offered at all.
	Tools []server.ServerTool
}

// toolsetDef is a toolset before its tools have been built.
//
// The tools sit behind a function rather than a field so that the names can be
// read — for validating the setting, and for the error message listing what is
// valid — without constructing the catalog, which needs a live Provider.
type toolsetDef struct {
	name    string
	summary string
	build   func(p *client.Provider) []server.ServerTool
}

// toolsetDefs is the single source of truth for what the server exposes.
//
// Catalog is derived from this rather than the other way round, which is what
// makes "every tool belongs to exactly one toolset" true by construction rather
// than by a convention someone has to remember. Adding a tool means adding it
// to a group here; there is nowhere else it could be registered from.
var toolsetDefs = []toolsetDef{
	{
		name:    ToolsetSystem,
		summary: "cluster status, regions, node pools, agent and scheduler config, Raft and Autopilot, search",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				system.GetClusterStatus(p),
				system.ListRegions(p),
				system.ListNodePools(p),
				system.ReadNodePool(p),
				system.GetAgentConfig(p),
				system.GetSchedulerConfig(p),
				system.GetAutopilotConfig(p),
				system.GetAutopilotHealth(p),
				system.GetRaftConfig(p),
				system.CheckConnection(p),
				system.Search(p),

				// Write.
				system.CreateNodePool(p),
				system.DeleteNodePool(p),
				system.SetSchedulerConfig(p),
				system.SetAutopilotConfig(p),
				system.RemoveRaftPeer(p),
				system.TransferLeadership(p),
			}
		},
	},
	{
		name:    ToolsetJobs,
		summary: "job specifications, versions, planning and submission",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				jobs.ListJobs(p),
				jobs.ReadJob(p),
				jobs.ReadJobSummary(p),
				jobs.ListJobVersions(p),
				jobs.ListJobAllocations(p),
				jobs.ListJobDeployments(p),
				jobs.ListJobEvaluations(p),
				jobs.GetJobScaleStatus(p),
				jobs.ParseJobHCL(p),
				jobs.ValidateJob(p),
				jobs.PlanJob(p),

				// Write.
				jobs.RunJob(p),
				jobs.EditJob(p),
				jobs.StopJob(p),
				jobs.ScaleTaskGroup(p),
				jobs.RevertJobVersion(p),
				jobs.DispatchParameterizedJob(p),
				jobs.ForcePeriodicJob(p),
			}
		},
	},
	{
		name:    ToolsetAllocs,
		summary: "allocations, task logs, allocation files, resource statistics and health checks",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				allocs.ListAllocations(p),
				allocs.ReadAllocation(p),
				allocs.ReadAllocationLogs(p),
				allocs.ListAllocationFiles(p),
				allocs.ReadAllocationFile(p),
				allocs.GetAllocationStats(p),
				allocs.GetAllocationChecks(p),

				// Write.
				allocs.RestartAllocation(p),
				allocs.StopAllocation(p),
				allocs.SignalAllocation(p),
			}
		},
	},
	{
		name:    ToolsetInvestigate,
		summary: "cross-cutting investigation: cluster-wide problem scan, job log search, job timeline, volume and integration diagnosis",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// These fan out across many allocations and correlate several
				// object types, so they are grouped by what they are for
				// rather than by which endpoint they call.
				investigate.FindProblems(p),
				investigate.SearchJobLogs(p),
				investigate.BuildJobTimeline(p),
				investigate.DiagnoseVolume(p),
				investigate.DiagnoseIntegrations(p),
			}
		},
	},
	{
		name:    ToolsetCapacity,
		summary: "cluster capacity, per-node placement feasibility and job right-sizing",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Arithmetic over the node, allocation and job views. Nomad
				// exposes every number these need and joins none of them.
				capacity.GetClusterCapacity(p),
				capacity.ExplainPlacement(p),
				capacity.AnalyzeJobResources(p),
			}
		},
	},
	{
		name:    ToolsetNodes,
		summary: "client nodes, draining and eligibility",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				nodes.ListNodes(p),
				nodes.ReadNode(p),
				nodes.ListNodeAllocations(p),
				nodes.GetNodeStats(p),

				// Write.
				nodes.DrainNode(p),
				nodes.SetNodeEligibility(p),
				nodes.RestartNodeAllocations(p),
				nodes.ForceEvaluateNode(p),
				nodes.SetNodeMeta(p),
				nodes.PurgeNode(p),
			}
		},
	},
	{
		name:    ToolsetDeployments,
		summary: "deployments and scheduler evaluations",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				scheduler.ListDeployments(p),
				scheduler.ReadDeployment(p),
				scheduler.ListEvaluations(p),
				scheduler.ReadEvaluation(p),

				// Write.
				scheduler.PromoteDeployment(p),
				scheduler.FailDeployment(p),
				scheduler.PauseDeployment(p),
				scheduler.UnblockDeployment(p),
				scheduler.SetDeploymentAllocHealth(p),
			}
		},
	},
	{
		name:    ToolsetCatalog,
		summary: "namespaces, service registrations, storage volumes and CSI plugins",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				catalog.ListNamespaces(p),
				catalog.ReadNamespace(p),
				catalog.ListServices(p),
				catalog.ReadService(p),
				catalog.ListVolumes(p),
				catalog.ReadVolume(p),
				catalog.ListCSIPlugins(p),
				catalog.ReadCSIPlugin(p),

				// Write.
				catalog.CreateNamespace(p),
				catalog.DeleteNamespace(p),
			}
		},
	},
	{
		name:    ToolsetVariables,
		summary: "Nomad Variables, the cluster's secret store",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// Read.
				variables.ListVariables(p),
				variables.ReadVariable(p),

				// Write.
				variables.WriteVariable(p),
				variables.DeleteVariable(p),
			}
		},
	},
	{
		name:    ToolsetDiag,
		summary: "hcdiag support-bundle collection",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// The only tool that runs a local binary, and the only one with
				// its own enable switch; see pkg/tools/diag.
				diag.CollectHCDiag(p),
			}
		},
	},
	{
		name:    ToolsetEnterprise,
		summary: "licence, quotas, Sentinel policies and Dynamic Application Sizing (Nomad Enterprise only)",
		build: func(p *client.Provider) []server.ServerTool {
			return []server.ServerTool{
				// These are built like any other tool and then filtered by
				// CatalogFor when the cluster is known to be Community Edition;
				// see utils.EnterpriseTool.
				enterprise.GetLicense(p),
				enterprise.ListQuotas(p),
				enterprise.ReadQuota(p),
				enterprise.CreateQuota(p),
				enterprise.DeleteQuota(p),
				enterprise.ListSentinelPolicies(p),
				enterprise.ReadSentinelPolicy(p),
				enterprise.WriteSentinelPolicy(p),
				enterprise.DeleteSentinelPolicy(p),
				enterprise.ListRecommendations(p),
				enterprise.ApplyRecommendations(p),
				enterprise.DismissRecommendations(p),
			}
		},
	},
}

// Toolsets returns every toolset with its tools built, in offering order.
func Toolsets(p *client.Provider) []Toolset {
	out := make([]Toolset, 0, len(toolsetDefs))
	for _, d := range toolsetDefs {
		out = append(out, Toolset{
			Name:    d.name,
			Summary: d.summary,
			Tools:   d.build(p),
		})
	}
	return out
}

// ToolsetNames returns every valid toolset name, in offering order.
func ToolsetNames() []string {
	out := make([]string, 0, len(toolsetDefs))
	for _, d := range toolsetDefs {
		out = append(out, d.name)
	}
	return out
}

// ToolsetSummaries returns each toolset name with its one-line summary, in
// offering order, for `--help` text and documentation.
func ToolsetSummaries() []string {
	out := make([]string, 0, len(toolsetDefs))
	for _, d := range toolsetDefs {
		out = append(out, d.name+": "+d.summary)
	}
	return out
}

// ValidateToolsets rejects an unrecognised toolset name.
//
// This runs at startup, before anything binds a transport, so a typo becomes a
// startup error naming the valid values rather than a server that comes up
// quietly missing a third of its tools. That failure mode is the whole reason
// the check exists: to a model, a tool that was never registered is
// indistinguishable from a capability the server does not have.
func ValidateToolsets(requested []string) error {
	valid := make(map[string]bool, len(toolsetDefs))
	for _, d := range toolsetDefs {
		valid[d.name] = true
	}

	var unknown []string
	for _, r := range requested {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || r == ToolsetAll || valid[r] {
			continue
		}
		unknown = append(unknown, r)
	}
	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)
	return fmt.Errorf("unknown toolset %s: valid toolsets are %s, or %q for all of them",
		strings.Join(quoteAll(unknown), ", "),
		strings.Join(quoteAll(ToolsetNames()), ", "),
		ToolsetAll)
}

// toolsetSelection turns the requested names into a lookup, or nil for "all".
//
// An empty request, or one containing "all", selects everything. Empty meaning
// all is what keeps the setting's default behaviour identical to the behaviour
// before the setting existed.
func toolsetSelection(requested []string) map[string]bool {
	if len(requested) == 0 {
		return nil
	}

	sel := make(map[string]bool, len(requested))
	for _, r := range requested {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == ToolsetAll {
			return nil
		}
		if r != "" {
			sel[r] = true
		}
	}
	if len(sel) == 0 {
		return nil
	}
	return sel
}

// quoteAll quotes each entry, so an error message survives a name containing a
// space or an empty string.
func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
