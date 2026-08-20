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
	readOnly bool
	logger   *log.Logger

	mu       sync.RWMutex
	mutating map[string]bool
}

// NewGate returns a Gate. When readOnly is false the gate permits everything,
// but still records classifications so tools can be introspected.
func NewGate(readOnly bool, logger *log.Logger) *Gate {
	return &Gate{
		readOnly: readOnly,
		logger:   logger,
		mutating: make(map[string]bool),
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

	g.mu.Lock()
	g.mutating[tool.Name] = !readOnly
	g.mu.Unlock()

	return !readOnly
}

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
			if !g.readOnly {
				return next(ctx, req)
			}

			name := req.Params.Name
			if !g.IsMutating(name) {
				return next(ctx, req)
			}

			g.logger.WithFields(log.Fields{
				"tool":      name,
				"read_only": true,
			}).Warn("refused mutating tool: server is in read-only mode")

			return mcp.NewToolResultError(refusalMessage(name)), nil
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
