// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package tools registers every MCP tool on the server.
//
// Each domain lives in its own subpackage (system, jobs, allocs, ...) with one
// file per tool group, and each tool constructor returns a server.ServerTool.
// Those constructors are grouped into named toolsets in toolsets.go, which is
// the single place that decides what the server exposes; everything here
// derives from it.
package tools

import (
	"context"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// editionProbeTimeout bounds the startup probe. Registration must not wait on
// an unreachable cluster: stdio in particular has a client on the other end
// expecting an initialize response.
const editionProbeTimeout = 5 * time.Second

// Catalog returns every tool the server exposes, from every toolset.
//
// It is exported and separate from InitTools so that tests can inspect the
// catalog directly — checking that each tool is annotated, that every mutating
// tool is refused in read-only mode, and that descriptions exist — without
// standing up an MCP server and driving it over a transport. It ignores every
// filter, so tests see every tool regardless of how any given server is
// configured, what edition any cluster happens to run, and whether the opt-in
// toolsets are switched on.
func Catalog(p *client.Provider) []server.ServerTool {
	var out []server.ServerTool
	for _, ts := range Toolsets(p) {
		out = append(out, ts.Tools...)
	}
	return out
}

// CatalogFor returns the tools to register for one particular server.
//
// Three filters apply, and they are independent. The toolset filter is the
// operator's choice about what this server should offer at all; an empty or nil
// toolsets slice means every one of them. The opt-in filter drops a toolset
// whose own switch is off, even when the operator named it — that is what makes
// "all" safe to be the default while the ACL tools exist. The edition filter
// drops the Enterprise-only tools when the cluster is known to be Community
// Edition, so the model is not offered tools that can only fail.
func CatalogFor(p *client.Provider, includeEnterprise bool, toolsets []string) []server.ServerTool {
	selected := toolsetSelection(toolsets)

	var out []server.ServerTool
	for _, ts := range Toolsets(p) {
		if selected != nil && !selected[ts.Name] {
			continue
		}
		// An opt-in toolset is skipped whether it was selected explicitly or
		// swept up by "all". Its switch is the only thing that offers it, so
		// that a server upgraded in place never gains reach its operator did
		// not ask for.
		if ts.OptIn && !ts.Enabled {
			continue
		}
		for _, t := range ts.Tools {
			if !includeEnterprise && utils.IsEnterpriseTool(t.Tool) {
				continue
			}
			out = append(out, t)
		}
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
//
// What gets registered is CatalogFor's decision: the operator's toolset
// selection, then the cluster's edition.
func InitTools(s *server.MCPServer, p *client.Provider, gate *client.Gate) []server.ServerTool {
	// The probe needs a context and a bounded wait: registration happens at
	// startup, and a cluster that is slow or absent must not hold the server
	// off the transport.
	ctx, cancel := context.WithTimeout(context.Background(), editionProbeTimeout)
	defer cancel()

	includeEnterprise, why := includeEnterpriseTools(ctx, p)
	toolsets := p.Config().Toolsets
	tools := CatalogFor(p, includeEnterprise, toolsets)

	p.Logger().WithField("enterprise_tools", includeEnterprise).
		WithField("reason", why).
		Debug("decided whether to offer the Enterprise-only tools")

	// A restricted catalog is logged at info rather than debug. An operator who
	// narrowed the toolsets wants confirmation it took effect, and anyone
	// debugging "why can the model not see list_jobs" should find the answer in
	// the startup output rather than by reading the configuration back.
	//
	// The test is the resolved selection, not whether the setting was written:
	// the default is the non-empty "all", and logging that as a restriction
	// would tell the operator the opposite of the truth.
	if toolsetSelection(toolsets) != nil {
		p.Logger().WithField("toolsets", strings.Join(toolsets, ",")).
			Info("tool catalog restricted to the configured toolsets")
	}

	warnUnenabledOptIns(p, toolsets)

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

// warnUnenabledOptIns logs when the operator named an opt-in toolset whose own
// switch is off.
//
// Silently registering nothing would look exactly like a broken tool from the
// model's side and like a broken setting from the operator's. This is the one
// case where a startup warning is worth the noise: the operator has said
// unambiguously that they want the toolset, and the answer is a second setting
// they did not know about.
func warnUnenabledOptIns(p *client.Provider, toolsets []string) {
	selected := toolsetSelection(toolsets)
	if selected == nil {
		return
	}
	for _, ts := range Toolsets(p) {
		if ts.OptIn && !ts.Enabled && selected[ts.Name] {
			p.Logger().WithField("toolset", ts.Name).
				Warn("toolset was requested but its own enable switch is off, so none of its " +
					"tools are registered; see " + config.EnvEnableACL)
		}
	}
}
