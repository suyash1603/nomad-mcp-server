// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package tools registers every MCP tool on the server.
//
// Each domain lives in its own subpackage (system, jobs, allocs, ...) with one
// file per tool group, and each tool constructor returns a server.ServerTool.
// Catalog is the single place that decides what the server exposes.
package tools

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/allocs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/catalog"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/jobs"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/nodes"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/scheduler"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/system"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/variables"
)

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
		system.GetAgentConfig(p),
		system.Search(p),

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

		// Nodes (write).
		nodes.DrainNode(p),
		nodes.SetNodeEligibility(p),

		// Deployments and evaluations (read).
		scheduler.ListDeployments(p),
		scheduler.ReadDeployment(p),
		scheduler.ListEvaluations(p),
		scheduler.ReadEvaluation(p),

		// Deployments (write).
		scheduler.PromoteDeployment(p),
		scheduler.FailDeployment(p),

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
	}
}

// InitTools registers the tool catalog on the MCP server.
//
// Mutating tools are registered even when the server is read-only. They are
// refused at call time by the gate instead, so that tools/list describes the
// server honestly and a blocked call returns an explanation rather than an
// "unknown tool" error that looks like a bug.
func InitTools(s *server.MCPServer, p *client.Provider, gate *client.Gate) {
	tools := Catalog(p)

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
		Debug("registered tools")
}
