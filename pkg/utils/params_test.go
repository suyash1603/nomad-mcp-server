// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func requestWithStatus(status string) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	if status != "" {
		req.Params.Arguments = map[string]any{"status": status}
	}
	return req
}

func TestAllocStatusFilter(t *testing.T) {
	t.Run("absent means no filter at all", func(t *testing.T) {
		require.Nil(t, AllocStatusFilter(requestWithStatus("")))
		require.Nil(t, AllocStatusFilter(requestWithStatus("   ")))
		require.Nil(t, AllocStatusFilter(requestWithStatus(",, ,")))
	})

	t.Run("single status", func(t *testing.T) {
		match := AllocStatusFilter(requestWithStatus("failed"))
		require.NotNil(t, match)
		require.True(t, match("failed"))
		require.False(t, match("running"))
	})

	t.Run("comma-separated list", func(t *testing.T) {
		match := AllocStatusFilter(requestWithStatus("failed, lost"))
		require.True(t, match("failed"))
		require.True(t, match("lost"))
		require.False(t, match("complete"))
	})

	t.Run("matching is case-insensitive and trims", func(t *testing.T) {
		match := AllocStatusFilter(requestWithStatus("  FAILED  "))
		require.True(t, match("failed"))
		require.True(t, match(" Failed "))
	})
}

func TestFilteredOutNote(t *testing.T) {
	t.Run("nothing filtered says nothing", func(t *testing.T) {
		require.Empty(t, FilteredOutNote(5, 0, "failed"))
	})

	t.Run("everything filtered distinguishes itself from an empty cluster", func(t *testing.T) {
		note := FilteredOutNote(0, 12, "failed")
		require.Contains(t, note, "12")
		require.Contains(t, note, "remove it")
	})

	t.Run("some filtered reports the remainder", func(t *testing.T) {
		note := FilteredOutNote(3, 40, "failed,lost")
		require.Contains(t, note, "40")
		require.Contains(t, note, "failed,lost")
	})
}
