// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package tools registers every MCP tool on the server.
//
// Each domain lives in its own subpackage (jobs, allocs, nodes, ...) with one
// file per tool, and each tool file exports a constructor returning a
// server.ServerTool. InitTools is the single place that decides what the server
// exposes.
package tools

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
)

// InitTools registers the tool catalog on the MCP server.
//
// Mutating tools are registered even when the server is read-only. They are
// refused at call time by the gate instead, so that tools/list describes the
// server honestly and a blocked call returns an explanation rather than an
// "unknown tool" error that looks like a bug.
func InitTools(s *server.MCPServer, p *client.Provider, gate *client.Gate) {
	// The domain packages populate this.
	p.Logger().Debug("registering tools")

	_ = s
	_ = gate
}

// register adds one tool to the server and classifies it for the read-only
// gate in the same step.
//
// Classification is derived from the tool's own MCP read-only annotation rather
// than from a separate list, so the two cannot drift apart. A tool that omits
// the annotation is treated as mutating and blocked in read-only mode, which
// makes forgetting it a visible failure rather than a silent hole.
func register(s *server.MCPServer, gate *client.Gate, tools ...server.ServerTool) {
	for _, t := range tools {
		gate.Classify(t.Tool)
		s.AddTool(t.Tool, t.Handler)
	}
}
