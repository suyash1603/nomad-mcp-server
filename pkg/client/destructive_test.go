// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// tool builds a Tool carrying the annotations the gate classifies on.
func tool(name string, readOnly, destructive bool) mcp.Tool {
	return mcp.Tool{
		Name: name,
		Annotations: mcp.ToolAnnotation{
			ReadOnlyHint:    &readOnly,
			DestructiveHint: &destructive,
		},
	}
}

// unannotated is a tool whose author forgot the annotations entirely. Both
// classifications must fail closed on it.
func unannotated(name string) mcp.Tool { return mcp.Tool{Name: name} }

func callGate(t *testing.T, g *Gate, name string) (*mcp.CallToolResult, bool) {
	t.Helper()

	reached := false
	next := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reached = true
		return mcp.NewToolResultText("ran"), nil
	}

	var req mcp.CallToolRequest
	req.Params.Name = name

	res, err := g.Middleware()(next)(context.Background(), req)
	require.NoError(t, err, "the gate must refuse via a tool result, not a Go error")
	return res, reached
}

// classifiedGate returns a gate that has seen one of each kind of tool.
func classifiedGate(t *testing.T, readOnly, allowDestructive bool) *Gate {
	t.Helper()

	g := NewGate(readOnly, allowDestructive, quietLogger())
	g.Classify(tool("read_thing", true, false))
	g.Classify(tool("gentle_write", false, false))
	g.Classify(tool("harsh_write", false, true))
	g.Classify(unannotated("forgotten"))
	return g
}

func TestDestructiveTierBlocksOnlyDestructiveWrites(t *testing.T) {
	g := classifiedGate(t, false, false)

	res, reached := callGate(t, g, "gentle_write")
	require.True(t, reached, "a non-destructive write must run when only the destructive tier is closed")
	require.False(t, res.IsError)

	res, reached = callGate(t, g, "harsh_write")
	require.False(t, reached, "a destructive write must not run when the destructive tier is closed")
	require.True(t, res.IsError)
}

func TestDestructiveTierLeavesReadsAlone(t *testing.T) {
	g := classifiedGate(t, false, false)

	_, reached := callGate(t, g, "read_thing")
	require.True(t, reached, "a read tool must never be refused by the destructive tier")
}

// A tool with no DestructiveHint is treated as destructive, for the same reason
// a tool with no ReadOnlyHint is treated as mutating: forgetting an annotation
// should block the tool, not quietly permit it.
func TestUnannotatedToolIsTreatedAsDestructive(t *testing.T) {
	g := classifiedGate(t, false, false)

	require.True(t, g.IsDestructive("forgotten"))

	_, reached := callGate(t, g, "forgotten")
	require.False(t, reached, "an unannotated tool must fail closed")
}

func TestUnknownToolIsTreatedAsDestructive(t *testing.T) {
	g := classifiedGate(t, false, false)

	require.True(t, g.IsDestructive("never_registered"),
		"an unknown tool name must not slip through the destructive tier")
}

// Read-only is the outer tier: with it on, a non-destructive write is still
// refused, and the message must be the read-only one rather than the
// destructive one, or the operator is told to set the wrong variable.
func TestReadOnlyTakesPrecedenceOverTheDestructiveTier(t *testing.T) {
	g := classifiedGate(t, true, true)

	res, reached := callGate(t, g, "gentle_write")
	require.False(t, reached)
	require.True(t, res.IsError)

	msg := resultText(t, res)
	require.Contains(t, msg, "read-only mode")
	require.Contains(t, msg, "NOMAD_MCP_READ_ONLY=false")
	require.NotContains(t, msg, "NOMAD_MCP_ALLOW_DESTRUCTIVE",
		"a read-only refusal must not point the operator at the destructive flag")
}

func TestAllowingDestructiveRunsEverything(t *testing.T) {
	g := classifiedGate(t, false, true)

	for _, name := range []string{"read_thing", "gentle_write", "harsh_write", "forgotten"} {
		_, reached := callGate(t, g, name)
		require.True(t, reached, "%s must run when both tiers are open", name)
	}
}

// The refusal has to say which knob lifts it, and say that the model cannot
// lift it itself — otherwise the model retries or hunts for a way around.
func TestDestructiveRefusalNamesTheFlagAndForbidsRetrying(t *testing.T) {
	g := classifiedGate(t, false, false)

	res, _ := callGate(t, g, "harsh_write")
	msg := resultText(t, res)

	require.Contains(t, msg, "harsh_write")
	require.Contains(t, msg, "NOMAD_MCP_ALLOW_DESTRUCTIVE=true")
	require.Contains(t, msg, "--allow-destructive=true")
	require.Contains(t, strings.ToLower(msg), "do not retry")
	require.Contains(t, msg, "Writes in general ARE enabled",
		"the refusal must distinguish itself from the read-only one")
}

// Where a safe alternative exists, the refusal should name it. Otherwise the
// model's next move is to look for a workaround on its own.
func TestDestructiveRefusalSuggestsASaferTool(t *testing.T) {
	g := NewGate(false, false, quietLogger())
	g.Classify(tool("drain_node", false, true))

	res, _ := callGate(t, g, "drain_node")
	require.Contains(t, resultText(t, res), "set_node_eligibility")
}

func TestDestructiveToolsListing(t *testing.T) {
	g := classifiedGate(t, false, false)

	require.ElementsMatch(t, []string{"harsh_write", "forgotten"}, g.DestructiveTools())
	require.False(t, g.AllowsDestructive())
	require.True(t, classifiedGate(t, false, true).AllowsDestructive())
}
