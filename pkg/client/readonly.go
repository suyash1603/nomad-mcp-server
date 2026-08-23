// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
)

// Gate refuses mutating tools when the server is in read-only mode.
//
// It is one piece of middleware wrapping every tool handler, rather than a
// check at the top of each mutating handler. That matters for two reasons: a
// new write tool cannot forget to include the check, and the refusal behaviour
// can be tested once, against the gate, instead of once per tool.
//
// Which tools count as mutating is not a hand-maintained list. Tools are
// classified from the MCP read-only annotation they already declare, via
// Classify, and anything not explicitly annotated read-only is treated as
// mutating. That fails closed: a tool whose author forgot the annotation is
// blocked in read-only mode rather than silently permitted.
type Gate struct {
	readOnly         bool
	allowDestructive bool
	logger           *log.Logger

	mu          sync.RWMutex
	mutating    map[string]bool
	destructive map[string]bool
}

// NewGate returns a Gate.
//
// readOnly refuses every mutating tool. allowDestructive is the second, finer
// tier: with writes enabled but allowDestructive false, a tool may change the
// cluster but not discard state or interrupt running work — scale_task_group
// runs, purge_node and delete_namespace do not. Both false-positive safely,
// because both classifications are derived from annotations that default to
// the restrictive answer when absent.
func NewGate(readOnly, allowDestructive bool, logger *log.Logger) *Gate {
	return &Gate{
		readOnly:         readOnly,
		allowDestructive: allowDestructive,
		logger:           logger,
		mutating:         make(map[string]bool),
		destructive:      make(map[string]bool),
	}
}

// Classify records whether a tool mutates the cluster, reading the MCP
// annotation the tool already carries. It returns the classification so that
// callers can log or test it.
//
// A tool counts as read-only only if it says so explicitly:
// ReadOnlyHint set and true.
func (g *Gate) Classify(tool mcp.Tool) bool {
	readOnly := tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint

	// A mutating tool counts as destructive unless it explicitly says
	// otherwise, for the same fail-closed reason: an unannotated write tool
	// should be the one that gets blocked, not the one that slips through.
	destructive := !readOnly &&
		(tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint)

	g.mu.Lock()
	g.mutating[tool.Name] = !readOnly
	g.destructive[tool.Name] = destructive
	g.mu.Unlock()

	return !readOnly
}

// IsDestructive reports whether a tool can discard state or interrupt running
// work. An unknown tool name is reported as destructive, so a misspelled or
// unregistered tool cannot slip past.
func (g *Gate) IsDestructive(name string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	destructive, known := g.destructive[name]
	if !known {
		return true
	}
	return destructive
}

// DestructiveTools returns the sorted names of every tool classified as
// destructive.
func (g *Gate) DestructiveTools() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]string, 0, len(g.destructive))
	for name, destructive := range g.destructive {
		if destructive {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// AllowsDestructive reports whether the destructive tier is unlocked.
func (g *Gate) AllowsDestructive() bool { return g.allowDestructive }

// IsMutating reports whether a tool has been classified as mutating. An unknown
// tool name is reported as mutating, so an unregistered or misspelled tool
// cannot slip past the gate.
func (g *Gate) IsMutating(name string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	mutating, known := g.mutating[name]
	if !known {
		return true
	}
	return mutating
}

// MutatingTools returns the sorted names of every tool classified as mutating.
func (g *Gate) MutatingTools() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]string, 0, len(g.mutating))
	for name, mutating := range g.mutating {
		if mutating {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ReadOnly reports whether the gate is enforcing.
func (g *Gate) ReadOnly() bool { return g.readOnly }

// Middleware returns the tool handler middleware that enforces the gate.
func (g *Gate) Middleware() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := req.Params.Name

			// A read tool is never refused by either tier, so check that first
			// and spend nothing on the common case.
			if !g.IsMutating(name) {
				return next(ctx, req)
			}

			if g.readOnly {
				g.logger.WithFields(log.Fields{
					"tool":      name,
					"read_only": true,
				}).Warn("refused mutating tool: server is in read-only mode")

				return mcp.NewToolResultError(refusalMessage(name)), nil
			}

			if !g.allowDestructive && g.IsDestructive(name) {
				g.logger.WithFields(log.Fields{
					"tool":              name,
					"allow_destructive": false,
				}).Warn("refused destructive tool: destructive operations are disabled")

				return mcp.NewToolResultError(destructiveRefusalMessage(name)), nil
			}

			return next(ctx, req)
		}
	}
}

// refusalMessage explains the refusal to the model and tells the user exactly
// how to lift it.
//
// It is deliberately explicit that the model cannot fix this itself. Without
// that, a model will often retry the same call, or go looking for another tool
// to achieve the same effect.
func refusalMessage(tool string) string {
	return fmt.Sprintf(
		"Refused: %q modifies the cluster, and this MCP server is running in read-only mode.\n\n"+
			"This is the default and is not something you can change from here — no other tool "+
			"will work around it, so do not retry.\n\n"+
			"To allow writes, the person running this server must restart it with either:\n"+
			"  NOMAD_MCP_READ_ONLY=false\n"+
			"  --read-only=false\n\n"+
			"Read-only tools are unaffected. If the goal was to understand a problem rather than "+
			"change something, plan_job, read_job, list_allocations and read_allocation_logs all "+
			"remain available.",
		tool)
}

// destructiveRefusalMessage explains the finer tier: writes are on, but this
// particular tool can discard state, and the operator asked for that to be
// off. It names a non-destructive alternative where an obvious one exists,
// because the model's next move is otherwise to look for a workaround.
func destructiveRefusalMessage(tool string) string {
	msg := fmt.Sprintf(
		"Refused: %q can discard state or interrupt running work, and this MCP server was "+
			"started with destructive operations disabled.\n\n"+
			"Writes in general ARE enabled here — this is a narrower restriction, and it is not "+
			"something you can change from here, so do not retry or look for another tool to "+
			"achieve the same effect.\n\n"+
			"To allow it, the person running this server must restart it with either:\n"+
			"  NOMAD_MCP_ALLOW_DESTRUCTIVE=true\n"+
			"  --allow-destructive=true",
		tool)

	if alt, ok := gentlerAlternative[tool]; ok {
		msg += "\n\n" + alt
	}
	return msg
}

// gentlerAlternative maps a destructive tool to the non-destructive tool that
// achieves as much of the intent as can be achieved safely. Only entries where
// the alternative is genuinely useful are listed; a wrong suggestion here is
// worse than none.
var gentlerAlternative = map[string]string{
	"drain_node": "To stop new work landing on the node without moving what is already " +
		"running, use set_node_eligibility, which is not destructive and is permitted.",
	"purge_node": "To confirm the node really is gone and see what it was running, use " +
		"read_node and list_node_allocations, both of which are permitted.",
	"run_job": "To see exactly what a submission would change without making the change, " +
		"use plan_job, which is permitted.",
	"restart_node_allocations": "To see what would be restarted, use list_node_allocations, " +
		"which is permitted.",
	"delete_namespace": "To see what the namespace holds before anyone deletes it, use " +
		"list_jobs and list_variables against it, both of which are permitted.",
}
