// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// expectedEnterpriseTools is the set of tools that call an endpoint Nomad
// Community Edition does not have.
//
// Like expectedMutatingTools, this is a literal table rather than something
// derived from the catalog, because a test that reads the answer off the code
// agrees with whatever the code says. Getting this wrong is not cosmetic: a
// tool wrongly marked here disappears from Community Edition clusters, and one
// wrongly left off is offered to a cluster that will answer it with a 501.
var expectedEnterpriseTools = []string{
	"apply_recommendations",
	"create_quota",
	"delete_quota",
	"delete_sentinel_policy",
	"dismiss_recommendations",
	"get_license",
	"list_quotas",
	"list_recommendations",
	"list_sentinel_policies",
	"read_quota",
	"read_sentinel_policy",
	"write_sentinel_policy",
}

func TestEnterpriseToolSetMatchesExpectation(t *testing.T) {
	p := testProvider(t, true)

	var marked []string
	for _, tool := range Catalog(p) {
		if utils.IsEnterpriseTool(tool.Tool) {
			marked = append(marked, tool.Tool.Name)
		}
	}

	require.ElementsMatch(t, expectedEnterpriseTools, marked,
		"the set of Enterprise-only tools has changed; update expectedEnterpriseTools if that was intended")
}

// The Community catalog is the full one minus exactly the Enterprise tools.
func TestCommunityCatalogDropsOnlyTheEnterpriseTools(t *testing.T) {
	p := testProvider(t, true)

	full := Catalog(p)
	community := CatalogFor(p, false, nil)

	require.Len(t, community, len(full)-len(expectedEnterpriseTools))

	for _, tool := range community {
		require.False(t, utils.IsEnterpriseTool(tool.Tool),
			"%q is Enterprise-only and should not be in the Community catalog", tool.Tool.Name)
	}

	// Every non-Enterprise tool must survive: dropping a core tool alongside
	// them would be a much worse bug than the one this guards.
	kept := map[string]bool{}
	for _, tool := range community {
		kept[tool.Tool.Name] = true
	}
	for _, tool := range full {
		if !utils.IsEnterpriseTool(tool.Tool) {
			require.True(t, kept[tool.Tool.Name],
				"%q is not Enterprise-only but was dropped from the Community catalog", tool.Tool.Name)
		}
	}
}

func TestEnterpriseCatalogKeepsEverything(t *testing.T) {
	p := testProvider(t, true)
	require.Len(t, CatalogFor(p, true, nil), len(Catalog(p)))
}

// A model reading tools/list on a Community cluster may still see these if the
// probe was inconclusive, so the description has to say so itself rather than
// relying on the tool having been filtered out.
func TestEnterpriseToolsSaySoInTheirDescription(t *testing.T) {
	p := testProvider(t, true)

	for _, tool := range Catalog(p) {
		if !utils.IsEnterpriseTool(tool.Tool) {
			continue
		}
		t.Run(tool.Tool.Name, func(t *testing.T) {
			require.Contains(t, strings.ToUpper(tool.Tool.Description), "ENTERPRISE",
				"an Enterprise-only tool must say so in the text the model reads")
		})
	}
}

// The marker rides in _meta, so it must survive being attached alongside the
// annotation options rather than overwriting them.
func TestEnterpriseMarkerCoexistsWithAnnotations(t *testing.T) {
	p := testProvider(t, true)

	byName := map[string]bool{}
	for _, tool := range Catalog(p) {
		byName[tool.Tool.Name] = true

		if !utils.IsEnterpriseTool(tool.Tool) {
			continue
		}
		require.NotNil(t, tool.Tool.Annotations.ReadOnlyHint,
			"%q lost its read-only annotation to the Enterprise marker", tool.Tool.Name)
	}

	for _, name := range expectedEnterpriseTools {
		require.True(t, byName[name], "%q is missing from the catalog entirely", name)
	}
}

// Not every Enterprise tool is a read tool, and the destructive ones among them
// must still be gated. This catches an Enterprise tool that was marked but
// never annotated.
func TestEnterpriseWriteToolsAreStillGated(t *testing.T) {
	p := testProvider(t, true)
	gate := gateFor(t, p, true)

	for _, name := range []string{
		"create_quota", "delete_quota", "write_sentinel_policy",
		"delete_sentinel_policy", "apply_recommendations", "dismiss_recommendations",
	} {
		require.True(t, gate.IsMutating(name),
			"%q writes to the cluster and must be gated like any other write tool", name)
	}
}
