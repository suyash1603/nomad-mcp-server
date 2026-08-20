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
	"github.com/suyash1603/nomad-mcp-server/pkg/config"

	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
)

// InitTools registers the tool catalog on the MCP server.
//
// Mutating tools are not filtered out here. They are registered unconditionally
// and refused at call time by the read-only gate, so that a model asking "what
// can you do?" sees an accurate catalog and gets an explanatory refusal rather
// than a confusing "no such tool".
func InitTools(_ *server.MCPServer, _ *config.Config, logger *log.Logger) {
	// The domain packages populate this. The skeleton registers no tools.
	logger.Debug("registering tools")
}
