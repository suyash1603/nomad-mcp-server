// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// aclToolNames is the ACL toolset as the wire sees it.
var aclToolNames = []string{
	"list_acl_policies",
	"read_acl_policy",
	"write_acl_policy",
	"list_acl_tokens",
	"read_acl_token",
	"create_acl_token",
	"update_acl_token",
	"list_acl_roles",
	"read_acl_role",
	"create_acl_role",
	"update_acl_role",
}

// TestACLToolsAreAbsentByDefault drives the real startup path to prove the
// property that matters to anyone upgrading: a server started the way every
// existing deployment starts it offers no way to read or mint credentials.
//
// The unit tests assert this against CatalogFor. This asserts it against the
// binary, through the environment variable, the flag binding and registration —
// which is where an opt-in silently becomes an opt-out.
func TestACLToolsAreAbsentByDefault(t *testing.T) {
	skipUnlessReady(t)

	names := listedTools(t)
	for _, name := range aclToolNames {
		if has(names, name) {
			t.Errorf("a default server offers %s; the ACL toolset must be opt-in", name)
		}
	}
}

// TestACLToolsetNeedsItsOwnSwitch checks that the toolset setting alone does not
// unlock it. --toolsets is how operators NARROW the catalog, so letting it widen
// this one would be the wrong direction entirely.
func TestACLToolsetNeedsItsOwnSwitch(t *testing.T) {
	skipUnlessReady(t)

	names := listedTools(t, "NOMAD_MCP_TOOLSETS=acl")
	for _, name := range aclToolNames {
		if has(names, name) {
			t.Errorf("--toolsets=acl alone offered %s without NOMAD_MCP_ENABLE_ACL", name)
		}
	}
}

// TestACLToolsAppearWhenEnabled is the other direction: the switch works, and
// it brings the whole toolset rather than part of it.
func TestACLToolsAppearWhenEnabled(t *testing.T) {
	skipUnlessReady(t)

	names := listedTools(t, "NOMAD_MCP_ENABLE_ACL=true")
	for _, name := range aclToolNames {
		if !has(names, name) {
			t.Errorf("NOMAD_MCP_ENABLE_ACL=true did not offer %s", name)
		}
	}

	// Enabling ACLs must not disturb anything else.
	if !has(names, "list_jobs") {
		t.Error("enabling the ACL toolset dropped list_jobs")
	}
}

// TestACLToolsRespectReadOnlyMode: the ACL switch is orthogonal to the
// read-only gate, and turning the toolset on must not turn writes on with it.
func TestACLToolsRespectReadOnlyMode(t *testing.T) {
	skipUnlessReady(t)

	c := newClient(t, "NOMAD_MCP_ENABLE_ACL=true")

	msg := c.toolFails("create_acl_token", map[string]any{
		"name":     "e2e-should-not-exist",
		"policies": []any{"anything"},
	})
	if !strings.Contains(msg, "read-only mode") {
		t.Errorf("create_acl_token was not refused by the read-only gate: %s", msg)
	}
}

// TestReadACLToolsWorkAgainstADevAgent exercises the read path against a real
// agent. `nomad agent -dev` runs with ACLs disabled, so Nomad answers these
// endpoints with a 404 and "ACL support disabled" — which is a legitimate
// answer and the one most people meet first. What is being checked is that the
// tool reaches Nomad and maps the reply into a sentence, rather than returning
// a bare Go error.
func TestReadACLToolsWorkAgainstADevAgent(t *testing.T) {
	skipUnlessReady(t)

	c := newClient(t, "NOMAD_MCP_ENABLE_ACL=true")

	for _, name := range []string{"list_acl_policies", "list_acl_tokens", "list_acl_roles"} {
		t.Run(name, func(t *testing.T) {
			body := c.callTool(name, nil).text()
			if body == "" {
				t.Fatal("no text in the result")
			}
			if strings.Contains(body, "NOMAD_MCP_ENABLE_ACL") {
				t.Errorf("%s was refused despite the switch being on: %s", name, body)
			}
		})
	}
}
