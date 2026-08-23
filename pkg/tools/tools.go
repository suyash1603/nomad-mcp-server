// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package tools registers every MCP tool on the server.
//
// Each domain lives in its own subpackage (system, jobs, allocs, ...) with one
// file per tool group, and each tool constructor returns a server.ServerTool.
// Catalog is the single place that decides what the server exposes.
package tools

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/allocs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/catalog"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/diag"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/enterprise"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/jobs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/nodes"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/scheduler"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/system"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/variables"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// editionProbeTimeout bounds the startup probe. Registration must not wait on
// an unreachable cluster: stdio in particular has a client on the other end
// expecting an initialize response.
const editionProbeTimeout = 5 * time.Second

// Catalog returns every tool the server exposes.
//
// It is exported and separate from InitTools so that tests can inspect the
// catalog directly — checking that each tool is annotated, that every mutating
// tool is refused in read-only mode, and that descriptions exist — without
// standing up an MCP server and driving it over a transport.
func Catalog(p *client.Provider) []server.ServerTool {
	return []server.ServerTool{
		// System and cluster (read).
		system.GetClusterStatus(p),
		system.ListRegions(p),
		system.ListNodePools(p),
		system.ReadNodePool(p),
		system.GetAgentConfig(p),
		system.GetSchedulerConfig(p),
		system.CheckConnection(p),
		system.Search(p),

		// Diagnostics. The only tool that runs a local binary, and the only
		// one with its own enable switch; see pkg/tools/diag.
		diag.CollectHCDiag(p),

		// System and cluster (write).
		system.CreateNodePool(p),
		system.DeleteNodePool(p),
		system.SetSchedulerConfig(p),

		// Jobs (read).
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

		// Jobs (write).
		jobs.RunJob(p),
		jobs.EditJob(p),
		jobs.StopJob(p),
		jobs.ScaleTaskGroup(p),
		jobs.RevertJobVersion(p),
		jobs.DispatchParameterizedJob(p),
		jobs.ForcePeriodicJob(p),

		// Allocations (read).
		allocs.ListAllocations(p),
		allocs.ReadAllocation(p),
		allocs.ReadAllocationLogs(p),
		allocs.ListAllocationFiles(p),
		allocs.ReadAllocationFile(p),
		allocs.GetAllocationStats(p),

		// Allocations (write).
		allocs.RestartAllocation(p),
		allocs.StopAllocation(p),
		allocs.SignalAllocation(p),

		// Nodes (read).
		nodes.ListNodes(p),
		nodes.ReadNode(p),
		nodes.ListNodeAllocations(p),
		nodes.GetNodeStats(p),

		// Nodes (write).
		nodes.DrainNode(p),
		nodes.SetNodeEligibility(p),
		nodes.RestartNodeAllocations(p),
		nodes.ForceEvaluateNode(p),
		nodes.SetNodeMeta(p),
		nodes.PurgeNode(p),

		// Deployments and evaluations (read).
		scheduler.ListDeployments(p),
		scheduler.ReadDeployment(p),
		scheduler.ListEvaluations(p),
		scheduler.ReadEvaluation(p),

		// Deployments (write).
		scheduler.PromoteDeployment(p),
		scheduler.FailDeployment(p),
		scheduler.PauseDeployment(p),
		scheduler.UnblockDeployment(p),
		scheduler.SetDeploymentAllocHealth(p),

		// Namespaces, services and volumes (read).
		catalog.ListNamespaces(p),
		catalog.ReadNamespace(p),
		catalog.ListServices(p),
		catalog.ReadService(p),
		catalog.ListVolumes(p),
		catalog.ReadVolume(p),

		// Namespaces (write).
		catalog.CreateNamespace(p),
		catalog.DeleteNamespace(p),

		// Variables (read).
		variables.ListVariables(p),
		variables.ReadVariable(p),

		// Variables (write).
		variables.WriteVariable(p),
		variables.DeleteVariable(p),

		// Enterprise only. These are registered like any other tool and then
		// filtered by CatalogFor when the cluster is known to be Community
		// Edition; see utils.EnterpriseTool.
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
}

// CatalogFor returns the tools to register against a particular cluster.
//
// It is Catalog minus the Enterprise-only tools when includeEnterprise is
// false. Catalog itself always returns everything, so tests can inspect every
// tool regardless of what any cluster happens to be.
func CatalogFor(p *client.Provider, includeEnterprise bool) []server.ServerTool {
	all := Catalog(p)
	if includeEnterprise {
		return all
	}

	out := make([]server.ServerTool, 0, len(all))
	for _, t := range all {
		if utils.IsEnterpriseTool(t.Tool) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// includeEnterpriseTools decides whether the Enterprise-only tools are offered.
//
// The "auto" policy probes the cluster and hides them only on a positive
// identification of Community Edition. An unreachable cluster or an
// inconclusive probe offers them, so a server started before its Nomad — or
// pointed at a cluster whose token cannot read the version — does not come up
// silently missing a third of its catalog. A tool that is offered and turns out
// not to exist still fails legibly, because utils.MapError translates Nomad's
// 501 into a plain sentence.
func includeEnterpriseTools(ctx context.Context, p *client.Provider) (bool, string) {
	cfg := p.Config()
	switch {
	case cfg.EnterpriseAlways():
		return true, "NOMAD_MCP_ENTERPRISE=true"
	case cfg.EnterpriseNever():
		return false, "NOMAD_MCP_ENTERPRISE=false"
	}

	info := p.Edition(ctx)
	if info.Edition == client.EditionCommunity {
		return false, "the cluster is Nomad Community Edition (" + info.Reason + ")"
	}
	return true, "edition is " + string(info.Edition)
}

// InitTools registers the tool catalog on the MCP server and returns it.
//
// Mutating tools are registered even when the server is read-only. They are
// refused at call time by the gate instead, so that tools/list describes the
// server honestly and a blocked call returns an explanation rather than an
// "unknown tool" error that looks like a bug.
//
// The catalog is returned because pkg/resources delegates to these same
// handlers: a resource read is the same view as the equivalent tool call, and
// passing the registered catalog along is what guarantees that rather than
// merely intending it.
func InitTools(s *server.MCPServer, p *client.Provider, gate *client.Gate) []server.ServerTool {
	// The probe needs a context and a bounded wait: registration happens at
	// startup, and a cluster that is slow or absent must not hold the server
	// off the transport.
	ctx, cancel := context.WithTimeout(context.Background(), editionProbeTimeout)
	defer cancel()

	includeEnterprise, why := includeEnterpriseTools(ctx, p)
	tools := CatalogFor(p, includeEnterprise)

	p.Logger().WithField("enterprise_tools", includeEnterprise).
		WithField("reason", why).
		Debug("decided whether to offer the Enterprise-only tools")

	for _, t := range tools {
		// Classification is derived from the tool's own MCP read-only
		// annotation rather than from a separate list, so the two cannot drift
		// apart. A tool that omits the annotation is treated as mutating and
		// blocked in read-only mode, which makes forgetting it a visible
		// failure rather than a silent hole.
		gate.Classify(t.Tool)
		s.AddTool(t.Tool, t.Handler)
	}

	p.Logger().WithField("tools", len(tools)).
		WithField("mutating", len(gate.MutatingTools())).
		WithField("destructive", len(gate.DestructiveTools())).
		Debug("registered tools")

	return tools
}
