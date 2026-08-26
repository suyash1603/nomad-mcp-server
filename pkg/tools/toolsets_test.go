// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// TestEveryToolBelongsToExactlyOneToolset is the property that makes toolsets
// safe to add tools to. A tool in two toolsets would register twice; a tool in
// none would be unreachable however the server is configured.
func TestEveryToolBelongsToExactlyOneToolset(t *testing.T) {
	p := testProvider(t, true)

	seen := map[string]string{}
	for _, ts := range Toolsets(p) {
		require.NotEmpty(t, ts.Tools, "toolset %q has no tools", ts.Name)
		require.NotEmpty(t, ts.Summary, "toolset %q has no summary", ts.Name)

		for _, tool := range ts.Tools {
			if other, dup := seen[tool.Tool.Name]; dup {
				t.Fatalf("tool %q is in both the %q and %q toolsets", tool.Tool.Name, other, ts.Name)
			}
			seen[tool.Tool.Name] = ts.Name
		}
	}

	require.Len(t, seen, len(Catalog(p)),
		"Catalog and the toolsets disagree about how many tools exist")
}

// TestToolsetNamesAreUnique guards the lookup that ValidateToolsets builds.
func TestToolsetNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range ToolsetNames() {
		require.False(t, seen[n], "duplicate toolset name %q", n)
		require.Equal(t, strings.ToLower(n), n, "toolset names are matched lowercased")
		seen[n] = true
	}
	require.NotEmpty(t, seen)
}

func TestCatalogForSelectsToolsets(t *testing.T) {
	p := testProvider(t, true)

	t.Run("nil selects everything", func(t *testing.T) {
		require.Len(t, CatalogFor(p, true, nil), len(Catalog(p)))
	})

	t.Run("all selects everything", func(t *testing.T) {
		require.Len(t, CatalogFor(p, true, []string{ToolsetAll}), len(Catalog(p)))
	})

	t.Run("a single toolset selects only its own tools", func(t *testing.T) {
		got := CatalogFor(p, true, []string{ToolsetJobs})
		require.NotEmpty(t, got)

		names := toolNames(got)
		require.Contains(t, names, "list_jobs")
		require.NotContains(t, names, "list_nodes")
		require.NotContains(t, names, "read_variable")
	})

	t.Run("several toolsets union", func(t *testing.T) {
		names := toolNames(CatalogFor(p, true, []string{ToolsetJobs, ToolsetNodes}))
		require.Contains(t, names, "list_jobs")
		require.Contains(t, names, "list_nodes")
		require.NotContains(t, names, "read_variable")
	})

	t.Run("names are matched case-insensitively and trimmed", func(t *testing.T) {
		require.Equal(t,
			toolNames(CatalogFor(p, true, []string{ToolsetJobs})),
			toolNames(CatalogFor(p, true, []string{"  JOBS "})),
		)
	})

	t.Run("an unknown toolset selects nothing rather than everything", func(t *testing.T) {
		// Startup validation rejects this before it can happen. The behaviour
		// is asserted anyway because the failure direction matters: selecting
		// nothing is visible, whereas falling back to everything would silently
		// ignore an operator's attempt to restrict the server.
		require.Empty(t, CatalogFor(p, true, []string{"nonsense"}))
	})
}

// TestCatalogForAppliesBothFilters checks the toolset and edition filters
// compose rather than one overriding the other.
func TestCatalogForAppliesBothFilters(t *testing.T) {
	p := testProvider(t, true)

	full := CatalogFor(p, true, []string{ToolsetEnterprise})
	require.NotEmpty(t, full)

	community := CatalogFor(p, false, []string{ToolsetEnterprise})
	require.Empty(t, community,
		"the enterprise toolset must be empty on Community Edition, not fall back to everything")

	// A non-Enterprise toolset is unaffected by the edition filter.
	require.Equal(t,
		toolNames(CatalogFor(p, true, []string{ToolsetJobs})),
		toolNames(CatalogFor(p, false, []string{ToolsetJobs})),
	)
}

func TestValidateToolsets(t *testing.T) {
	t.Run("accepts every real name", func(t *testing.T) {
		require.NoError(t, ValidateToolsets(ToolsetNames()))
	})

	t.Run("accepts all, empty and nil", func(t *testing.T) {
		require.NoError(t, ValidateToolsets(nil))
		require.NoError(t, ValidateToolsets([]string{}))
		require.NoError(t, ValidateToolsets([]string{ToolsetAll}))
		require.NoError(t, ValidateToolsets([]string{"  Jobs  "}))
	})

	t.Run("rejects an unknown name and says what is valid", func(t *testing.T) {
		err := ValidateToolsets([]string{ToolsetJobs, "jobss"})
		require.Error(t, err)
		require.Contains(t, err.Error(), `"jobss"`)
		// The message has to carry the valid values: an operator who typoed a
		// name needs the answer in the error, not in the documentation.
		for _, n := range ToolsetNames() {
			require.Contains(t, err.Error(), `"`+n+`"`)
		}
	})
}

// TestToolsetFlagUsageListsEveryToolset stops the help text in pkg/config
// drifting from the real toolsets. config cannot import tools — tools already
// depends on config — so the usage string is written out by hand there, and
// this is what keeps it honest.
func TestToolsetFlagUsageListsEveryToolset(t *testing.T) {
	usage := config.ToolsetsFlagUsage()
	for _, n := range ToolsetNames() {
		require.Contains(t, usage, n,
			"the %s flag usage in pkg/config does not mention the %q toolset", config.EnvToolsets, n)
	}
}

// TestToolsetSelectionTreatsDefaultAsUnrestricted is the check behind the
// startup log. The default value is the non-empty "all", so a naive
// len(toolsets) > 0 reports every server as restricted — telling an operator
// debugging a missing tool the exact opposite of the truth.
func TestToolsetSelectionTreatsDefaultAsUnrestricted(t *testing.T) {
	require.Nil(t, toolsetSelection(nil))
	require.Nil(t, toolsetSelection([]string{}))
	require.Nil(t, toolsetSelection([]string{ToolsetAll}),
		"the default must not read as a restriction")
	require.Nil(t, toolsetSelection([]string{config.DefaultToolsets}))
	require.Nil(t, toolsetSelection([]string{ToolsetJobs, ToolsetAll}),
		"an explicit all alongside other names still means all")

	require.NotNil(t, toolsetSelection([]string{ToolsetJobs}))
}

// TestToolsetAllMatchesConfig pins the two spellings of "all" together.
// pkg/config needs the value for its own default and cannot import this
// package, so the constant exists in both places; this is what stops them
// diverging into a default that selects nothing.
func TestToolsetAllMatchesConfig(t *testing.T) {
	require.Equal(t, ToolsetAll, config.ToolsetAllValue)
	require.Equal(t, ToolsetAll, config.DefaultToolsets,
		"the default must select every toolset")
}

// TestSelectedToolsAreStillClassified checks that restricting the toolsets does
// not smuggle an unannotated tool past the read-only gate.
func TestSelectedToolsAreStillClassified(t *testing.T) {
	p := testProvider(t, true)
	for _, tool := range CatalogFor(p, true, []string{ToolsetJobs, ToolsetVariables}) {
		require.NotNil(t, tool.Tool.Annotations.ReadOnlyHint,
			"tool %q has no read-only annotation", tool.Tool.Name)
		require.NotEmpty(t, tool.Tool.Description, "tool %q has no description", tool.Tool.Name)
		_ = utils.IsEnterpriseTool(tool.Tool)
	}
}

func toolNames(tools []server.ServerTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Tool.Name)
	}
	return out
}
