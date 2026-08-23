// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func quietLogger() *log.Logger {
	l := log.New()
	l.SetOutput(io.Discard)
	return l
}

func boolPtr(b bool) *bool { return &b }

// readTool and writeTool model how real tools declare themselves.
func readTool(name string) mcp.Tool {
	return mcp.NewTool(name,
		mcp.WithDescription("a read tool"),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{ReadOnlyHint: boolPtr(true)}),
	)
}

func writeTool(name string) mcp.Tool {
	return mcp.NewTool(name,
		mcp.WithDescription("a write tool"),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{DestructiveHint: boolPtr(true)}),
	)
}

// call runs a tool through the gate's middleware and reports whether the
// underlying handler was reached.
func call(t *testing.T, g *Gate, toolName string) (result *mcp.CallToolResult, reached bool) {
	t.Helper()

	handler := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reached = true
		return mcp.NewToolResultText("ok"), nil
	}

	var req mcp.CallToolRequest
	req.Params.Name = toolName

	res, err := g.Middleware()(handler)(context.Background(), req)
	require.NoError(t, err, "the gate must refuse via a tool result, not a Go error")
	return res, reached
}

func TestGateClassifiesFromAnnotations(t *testing.T) {
	g := NewGate(true, true, quietLogger())

	require.False(t, g.Classify(readTool("list_jobs")), "a read-only tool must not be mutating")
	require.True(t, g.Classify(writeTool("stop_job")), "a destructive tool must be mutating")

	require.False(t, g.IsMutating("list_jobs"))
	require.True(t, g.IsMutating("stop_job"))
}

// TestUnannotatedToolIsTreatedAsMutating is the fail-closed property: a tool
// whose author forgot the annotation must be blocked, not silently allowed.
func TestUnannotatedToolIsTreatedAsMutating(t *testing.T) {
	g := NewGate(true, true, quietLogger())

	forgot := mcp.NewTool("forgot_annotation", mcp.WithDescription("oops"))
	require.True(t, g.Classify(forgot), "an unannotated tool must default to mutating")

	_, reached := call(t, g, "forgot_annotation")
	require.False(t, reached, "an unannotated tool must be refused in read-only mode")
}

// TestUnknownToolIsTreatedAsMutating covers a name the gate has never seen.
func TestUnknownToolIsTreatedAsMutating(t *testing.T) {
	g := NewGate(true, true, quietLogger())
	require.True(t, g.IsMutating("never_registered"))

	_, reached := call(t, g, "never_registered")
	require.False(t, reached)
}

// TestReadOnlyRefusesMutatingTools is the headline guarantee of the project.
func TestReadOnlyRefusesMutatingTools(t *testing.T) {
	g := NewGate(true, true, quietLogger())
	g.Classify(writeTool("stop_job"))

	res, reached := call(t, g, "stop_job")

	require.False(t, reached, "the handler must never run in read-only mode")
	require.True(t, res.IsError, "the refusal must be reported as a tool error")

	msg := resultText(t, res)
	require.Contains(t, msg, "read-only mode")
	require.Contains(t, msg, "NOMAD_MCP_READ_ONLY=false")
	require.Contains(t, msg, "--read-only=false")
	require.Contains(t, msg, "stop_job")
}

// TestRefusalTellsTheModelNotToRetry: without this, a model will typically
// retry the call or hunt for another tool that achieves the same effect.
func TestRefusalTellsTheModelNotToRetry(t *testing.T) {
	g := NewGate(true, true, quietLogger())
	g.Classify(writeTool("stop_job"))

	res, _ := call(t, g, "stop_job")
	require.Contains(t, resultText(t, res), "do not retry")
}

func TestReadOnlyAllowsReadTools(t *testing.T) {
	g := NewGate(true, true, quietLogger())
	g.Classify(readTool("list_jobs"))

	res, reached := call(t, g, "list_jobs")
	require.True(t, reached, "read tools must pass through in read-only mode")
	require.False(t, res.IsError)
}

func TestWritesEnabledAllowsEverything(t *testing.T) {
	g := NewGate(false, true, quietLogger())
	g.Classify(writeTool("stop_job"))
	g.Classify(readTool("list_jobs"))

	for _, name := range []string{"stop_job", "list_jobs", "never_registered"} {
		_, reached := call(t, g, name)
		require.True(t, reached, "%s must run when read-only is off", name)
	}
}

func TestMutatingToolsListing(t *testing.T) {
	g := NewGate(true, true, quietLogger())
	g.Classify(writeTool("stop_job"))
	g.Classify(writeTool("drain_node"))
	g.Classify(readTool("list_jobs"))

	require.Equal(t, []string{"drain_node", "stop_job"}, g.MutatingTools(),
		"mutating tools should be reported sorted")
}

func TestGateReadOnlyAccessor(t *testing.T) {
	require.True(t, NewGate(true, true, quietLogger()).ReadOnly())
	require.False(t, NewGate(false, true, quietLogger()).ReadOnly())
}

// TestGateIsConcurrencySafe: in HTTP mode many sessions call tools at once
// while classification may still be running.
func TestGateIsConcurrencySafe(t *testing.T) {
	g := NewGate(true, true, quietLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			g.Classify(readTool("list_jobs"))
		}
	}()
	for i := 0; i < 500; i++ {
		_ = g.IsMutating("list_jobs")
		_ = g.MutatingTools()
	}
	<-done
}

// resultText pulls the text out of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", res.Content[0])
	return tc.Text
}

// compile-time check that the gate middleware matches mcp-go's expected type.
var _ server.ToolHandlerMiddleware = NewGate(true, true, quietLogger()).Middleware()
