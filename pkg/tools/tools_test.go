// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// testProvider builds a Provider pointed at an address nothing is listening on.
//
// None of these tests should reach Nomad: the read-only gate must refuse a
// mutating call before the handler runs. If a tool ever did reach the network,
// it would fail with connection refused rather than quietly succeeding against
// a real cluster, which is the safe direction for a test to fail in.
func testProvider(t *testing.T, readOnly bool) *client.Provider {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	p, err := client.New(&config.Config{
		NomadAddr:      "http://127.0.0.1:1",
		NomadNamespace: config.DefaultNomadNamespace,
		ReadOnly:       readOnly,
		MaxLogBytes:    config.DefaultMaxLogBytes,
	}, logger)
	require.NoError(t, err)
	return p
}

func gateFor(t *testing.T, p *client.Provider, readOnly bool) *client.Gate {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	gate := client.NewGate(readOnly, logger)
	for _, tool := range Catalog(p) {
		gate.Classify(tool.Tool)
	}
	return gate
}

// callThroughGate invokes a tool the way the server does: through the
// read-only middleware, not directly.
func callThroughGate(t *testing.T, gate *client.Gate, tool string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)) (*mcp.CallToolResult, bool) {
	t.Helper()

	reached := false
	wrapped := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reached = true
		return handler(ctx, req)
	}

	var req mcp.CallToolRequest
	req.Params.Name = tool

	res, err := gate.Middleware()(wrapped)(context.Background(), req)
	require.NoError(t, err, "the gate must refuse via a tool result, not a Go error")
	return res, reached
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", res.Content[0])
	return tc.Text
}

// expectedMutatingTools is the write catalog.
//
// This is written out by hand on purpose. Everything else in the codebase
// derives the mutating set from annotations, which is right for the runtime but
// useless as a test: if a tool lost its annotation, the derived set would shrink
// and a derived test would still pass. Comparing against a literal list is what
// catches that.
var expectedMutatingTools = []string{
	"create_namespace",
	"delete_namespace",
	"delete_variable",
	"dispatch_parameterized_job",
	"drain_node",
	"fail_deployment",
	"force_periodic_job",
	"promote_deployment",
	"restart_allocation",
	"revert_job_version",
	"run_job",
	"scale_task_group",
	"set_node_eligibility",
	"signal_allocation",
	"stop_allocation",
	"stop_job",
	"write_variable",
}

// TestEveryMutatingToolIsRefusedInReadOnlyMode is the guarantee the whole
// project rests on: with NOMAD_MCP_READ_ONLY=true, no write tool runs.
func TestEveryMutatingToolIsRefusedInReadOnlyMode(t *testing.T) {
	p := testProvider(t, true)
	gate := gateFor(t, p, true)

	byName := map[string]int{}
	for i, tool := range Catalog(p) {
		byName[tool.Tool.Name] = i
	}
	all := Catalog(p)

	for _, name := range expectedMutatingTools {
		t.Run(name, func(t *testing.T) {
			idx, ok := byName[name]
			require.True(t, ok, "tool %q is missing from the catalog entirely", name)

			require.True(t, gate.IsMutating(name),
				"%q must be classified as mutating", name)

			res, reached := callThroughGate(t, gate, name, all[idx].Handler)

			require.False(t, reached,
				"%q ran its handler in read-only mode; the gate did not refuse it", name)
			require.True(t, res.IsError, "%q should be refused as a tool error", name)

			msg := resultText(t, res)
			require.Contains(t, msg, "read-only mode")
			require.Contains(t, msg, "NOMAD_MCP_READ_ONLY=false",
				"the refusal must tell the operator how to enable writes")
			require.Contains(t, msg, name)
		})
	}
}

// TestMutatingSetMatchesExpectation catches a write tool added without being
// annotated, and a read tool wrongly marked as mutating.
func TestMutatingSetMatchesExpectation(t *testing.T) {
	p := testProvider(t, true)
	gate := gateFor(t, p, true)

	require.ElementsMatch(t, expectedMutatingTools, gate.MutatingTools(),
		"the set of mutating tools has changed; update expectedMutatingTools if that was intended")
}

// TestWriteToolsRunWhenWritesAreEnabled proves the gate is the only thing
// blocking them: with read-only off, the handler is reached.
func TestWriteToolsRunWhenWritesAreEnabled(t *testing.T) {
	p := testProvider(t, false)
	gate := gateFor(t, p, false)

	all := Catalog(p)
	byName := map[string]int{}
	for i, tool := range all {
		byName[tool.Tool.Name] = i
	}

	for _, name := range expectedMutatingTools {
		t.Run(name, func(t *testing.T) {
			_, reached := callThroughGate(t, gate, name, all[byName[name]].Handler)
			require.True(t, reached,
				"%q must run when read-only is off, otherwise something other than the gate is blocking it", name)
		})
	}
}

// TestReadToolsAreNeverRefused: a read tool blocked in read-only mode would be
// a regression that makes the server useless in its default configuration.
func TestReadToolsAreNeverRefused(t *testing.T) {
	p := testProvider(t, true)
	gate := gateFor(t, p, true)

	mutating := map[string]bool{}
	for _, name := range expectedMutatingTools {
		mutating[name] = true
	}

	for _, tool := range Catalog(p) {
		if mutating[tool.Tool.Name] {
			continue
		}
		t.Run(tool.Tool.Name, func(t *testing.T) {
			_, reached := callThroughGate(t, gate, tool.Tool.Name, tool.Handler)
			require.True(t, reached,
				"read tool %q must not be refused in read-only mode", tool.Tool.Name)
		})
	}
}

// TestEveryToolIsAnnotated enforces the contract the gate depends on.
func TestEveryToolIsAnnotated(t *testing.T) {
	p := testProvider(t, true)

	for _, tool := range Catalog(p) {
		t.Run(tool.Tool.Name, func(t *testing.T) {
			require.NotNil(t, tool.Tool.Annotations.ReadOnlyHint,
				"%q has no readOnlyHint; the gate would treat it as mutating", tool.Tool.Name)

			if *tool.Tool.Annotations.ReadOnlyHint {
				return
			}
			// Mutating tools must declare both remaining hints, since clients
			// use them to decide whether to ask for confirmation.
			require.NotNil(t, tool.Tool.Annotations.DestructiveHint,
				"mutating tool %q must declare destructiveHint", tool.Tool.Name)
			require.NotNil(t, tool.Tool.Annotations.IdempotentHint,
				"mutating tool %q must declare idempotentHint", tool.Tool.Name)
		})
	}
}

// TestToolNamesAreWellFormed keeps the catalog consistent for the model.
func TestToolNamesAreWellFormed(t *testing.T) {
	p := testProvider(t, true)
	seen := map[string]bool{}

	for _, tool := range Catalog(p) {
		name := tool.Tool.Name

		require.False(t, seen[name], "duplicate tool name %q", name)
		seen[name] = true

		require.Equal(t, strings.ToLower(name), name, "tool names must be lowercase: %q", name)
		require.NotContains(t, name, "-", "tool names must use snake_case, not dashes: %q", name)
		require.NotContains(t, name, " ", "tool names must not contain spaces: %q", name)
	}
}

// TestDescriptionsAreUsable: an MCP tool description is the model's only guide
// to when a tool applies, so a thin one is a real defect.
func TestDescriptionsAreUsable(t *testing.T) {
	p := testProvider(t, true)

	for _, tool := range Catalog(p) {
		t.Run(tool.Tool.Name, func(t *testing.T) {
			desc := tool.Tool.Description
			require.NotEmpty(t, desc)
			require.Greater(t, len(desc), 120,
				"%q has a description too thin for a model to act on", tool.Tool.Name)
		})
	}
}

// TestDestructiveToolsWarnInTheirDescription: clients may not surface the
// destructive annotation, so the warning has to be in the text the model reads.
func TestDestructiveToolsWarnInTheirDescription(t *testing.T) {
	p := testProvider(t, true)

	for _, tool := range Catalog(p) {
		ann := tool.Tool.Annotations
		if ann.ReadOnlyHint == nil || *ann.ReadOnlyHint {
			continue
		}
		if ann.DestructiveHint == nil || !*ann.DestructiveHint {
			continue
		}

		t.Run(tool.Tool.Name, func(t *testing.T) {
			desc := strings.ToLower(tool.Tool.Description)
			warned := strings.Contains(desc, "confirm") ||
				strings.Contains(desc, "irreversible") ||
				strings.Contains(desc, "cannot be undone") ||
				strings.Contains(desc, "permanently") ||
				strings.Contains(desc, "careful") ||
				strings.Contains(desc, "disruptive") ||
				strings.Contains(desc, "always run plan_job")

			require.True(t, warned,
				"destructive tool %q should tell the model to confirm or warn about consequences",
				tool.Tool.Name)
		})
	}
}

// TestCatalogSize is a coarse guard against a domain being dropped from
// registration during a refactor.
func TestCatalogSize(t *testing.T) {
	p := testProvider(t, true)
	all := Catalog(p)

	require.GreaterOrEqual(t, len(all), 54, "tools appear to have gone missing from the catalog")
	require.Equal(t, len(expectedMutatingTools), 17)
}
