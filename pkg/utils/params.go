// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import "github.com/mark3labs/mcp-go/mcp"

// ReadOnlyTool marks a tool as making no changes to the cluster.
//
// This is what the read-only gate classifies on, so it is not documentation:
// omitting it causes the tool to be refused whenever the server runs in its
// default read-only mode.
func ReadOnlyTool() mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint: BoolPtr(true),
		// Every tool here reaches out to a Nomad cluster, which is an external
		// system whose state this server does not control.
		OpenWorldHint: BoolPtr(true),
	})
}

// MutatingTool marks a tool that changes the cluster.
//
// destructive says whether the change can discard state or interrupt running
// work; idempotent says whether repeating the call has any additional effect.
// Clients use both to decide whether to ask the user for confirmation.
func MutatingTool(destructive, idempotent bool) mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    BoolPtr(false),
		DestructiveHint: BoolPtr(destructive),
		IdempotentHint:  BoolPtr(idempotent),
		OpenWorldHint:   BoolPtr(true),
	})
}

// NamespaceParam declares the optional namespace argument shared by every
// namespaced tool.
func NamespaceParam() mcp.ToolOption {
	return mcp.WithString("namespace",
		mcp.Description(
			"Nomad namespace to operate in. Defaults to the server's configured namespace "+
				"(usually \"default\"). Use \"*\" to search across all namespaces where the tool supports it. "+
				"If a job or allocation cannot be found, a different namespace is the most common reason."),
	)
}

// RegionParam declares the optional region argument.
func RegionParam() mcp.ToolOption {
	return mcp.WithString("region",
		mcp.Description(
			"Nomad region to target. Defaults to the region of the agent the server is configured against. "+
				"Only set this on a multi-region cluster; use list_regions to see what exists."),
	)
}

// FilterParam declares the optional go-bexpr filter argument supported by
// Nomad's paginated list endpoints.
func FilterParam(examples string) mcp.ToolOption {
	desc := "Optional server-side filter, written as a Nomad go-bexpr expression. " +
		"Filtering on the server is much cheaper than fetching everything and discarding it here."
	if examples != "" {
		desc += " Examples: " + examples
	}
	return mcp.WithString("filter", mcp.Description(desc))
}

// PrefixParam declares the optional ID-prefix argument.
func PrefixParam(what string) mcp.ToolOption {
	return mcp.WithString("prefix",
		mcp.Description("Return only "+what+" whose ID starts with this prefix."),
	)
}

// BoolPtr returns a pointer to b, for the MCP annotation fields.
func BoolPtr(b bool) *bool { return &b }

// StringPtr returns a pointer to s.
func StringPtr(s string) *string { return &s }

// IntPtr returns a pointer to i.
func IntPtr(i int) *int { return &i }

// metaKeyEnterprise is the _meta field marking a tool as Enterprise-only.
//
// The marker rides on the tool itself for the same reason the read-only
// classification does: a separate list of Enterprise tool names would be one
// more thing to forget to update, and forgetting it here means either hiding a
// tool that works or advertising one that cannot.
const metaKeyEnterprise = "io.github.suyash1603.nomad-mcp/enterprise"

// EnterpriseTool marks a tool as calling an endpoint that exists only in Nomad
// Enterprise, and prepends that fact to its description.
//
// Apply it after the description option, which it edits in place. Community
// Edition answers these endpoints with HTTP 501, which utils.MapError already
// translates — this marker is what lets the server go one better and not offer
// the tool at all against a cluster known to be Community Edition.
func EnterpriseTool() mcp.ToolOption {
	return func(t *mcp.Tool) {
		if t.Meta == nil {
			t.Meta = &mcp.Meta{}
		}
		if t.Meta.AdditionalFields == nil {
			t.Meta.AdditionalFields = map[string]any{}
		}
		t.Meta.AdditionalFields[metaKeyEnterprise] = true

		t.Description = "REQUIRES NOMAD ENTERPRISE. This endpoint does not exist in Nomad " +
			"Community Edition and will be refused there; get_cluster_status reports which " +
			"edition this cluster runs.\n\n" + t.Description
	}
}

// IsEnterpriseTool reports whether a tool was marked with EnterpriseTool.
func IsEnterpriseTool(t mcp.Tool) bool {
	if t.Meta == nil || t.Meta.AdditionalFields == nil {
		return false
	}
	v, ok := t.Meta.AdditionalFields[metaKeyEnterprise].(bool)
	return ok && v
}
