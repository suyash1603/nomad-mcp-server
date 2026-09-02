// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// The ACL tools are the only ones in this server that operate on who may use
// the cluster rather than on the workload, and they carry two gates nothing
// else has. These tests are about those gates: that the toolset is absent
// unless switched on, and that a token's secret does not reach the model.

// aclTools is every tool in the ACL toolset. Written out by hand for the same
// reason expectedMutatingTools is: deriving it from the toolset would make the
// test agree with whatever the toolset happens to contain, including tools
// nobody meant to add.
var aclTools = []string{
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

// aclEnabled turns the toolset on. Used by the handler tests below.
func aclEnabled(c *config.Config) { c.EnableACL = true }

// TestACLToolsetIsOptIn is the property the whole design rests on: an operator
// who upgrades this server does not silently acquire the ability to read or
// mint credentials. "all" is the default toolset selection, and it must not
// reach these.
func TestACLToolsetIsOptIn(t *testing.T) {
	off := testProviderWithACL(t, false)
	on := testProviderWithACL(t, true)

	t.Run("all does not select it", func(t *testing.T) {
		names := toolNames(CatalogFor(off, true, nil))
		for _, name := range aclTools {
			require.NotContains(t, names, name,
				"%q is offered by default; the ACL toolset must be opt-in", name)
		}
	})

	t.Run("naming it explicitly does not select it either", func(t *testing.T) {
		// The switch is the only thing that offers the toolset. Letting
		// --toolsets=acl override it would mean the safe default could be
		// undone by the setting operators reach for to *narrow* the catalog,
		// which is the opposite of what that setting is for.
		require.Empty(t, CatalogFor(off, true, []string{ToolsetACL}))
	})

	t.Run("the switch offers it", func(t *testing.T) {
		names := toolNames(CatalogFor(on, true, nil))
		for _, name := range aclTools {
			require.Contains(t, names, name)
		}
	})

	t.Run("the switch does not override a narrowed selection", func(t *testing.T) {
		// Enabling ACLs must not smuggle the toolset past an operator who
		// restricted the catalog to something else entirely.
		require.NotContains(t, toolNames(CatalogFor(on, true, []string{ToolsetJobs})), "list_acl_tokens")
	})

	t.Run("acl is a valid toolset name either way", func(t *testing.T) {
		require.NoError(t, ValidateToolsets([]string{ToolsetACL}))
		require.Contains(t, OptInToolsets(), ToolsetACL)
	})

	t.Run("Catalog still sees them, so the catalog-wide tests cover them", func(t *testing.T) {
		require.Subset(t, toolNames(Catalog(off)), aclTools)
	})
}

// TestACLHandlersRefuseWhenDisabled is defence in depth. CatalogFor already
// declines to register these, so in a correctly wired server this is
// unreachable — which is exactly why it is worth pinning: a future refactor
// that registers the catalog by another route must not expose them.
func TestACLHandlersRefuseWhenDisabled(t *testing.T) {
	h := newHarness(t) // EnableACL left at its default of false

	for _, name := range aclTools {
		t.Run(name, func(t *testing.T) {
			msg := h.fails(name, map[string]any{
				"name":        "anything",
				"accessor_id": "anything",
				"rules":       "namespace \"default\" { policy = \"read\" }",
				"policies":    []any{"some-policy"},
			})
			require.Contains(t, msg, "NOMAD_MCP_ENABLE_ACL=true",
				"the refusal must tell the operator how to enable the tools")
			require.Contains(t, msg, "do not retry")
		})
	}

	// Nothing may have reached Nomad on the way to being refused.
	for _, req := range h.nomad.Requests() {
		require.NotContains(t, req.Path, "/v1/acl/",
			"a disabled ACL tool called Nomad at %s before refusing", req.Path)
	}
}

// --- secrets ----------------------------------------------------------------

const (
	testAccessor = "88888888-8888-8888-8888-888888888888"
	testSecret   = "99999999-9999-9999-9999-999999999999"
)

func aclTokenFixture() map[string]any {
	return map[string]any{
		"AccessorID": testAccessor,
		"SecretID":   testSecret,
		"Name":       "ci-deployer",
		"Type":       "client",
		"Policies":   []string{"deploy"},
		"Global":     false,
		"CreateTime": "2026-08-01T00:00:00Z",
	}
}

// TestReadACLTokenWithholdsTheSecret is the disclosure gate. Nomad returns the
// SecretID on this endpoint; the tool must not pass it on.
func TestReadACLTokenWithholdsTheSecret(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	raw := h.raw("read_acl_token", map[string]any{"accessor_id": testAccessor})
	require.NotContains(t, raw, testSecret,
		"the token's secret reached the model's context")

	out := h.ok("read_acl_token", map[string]any{"accessor_id": testAccessor})
	require.Equal(t, testAccessor, out["accessor_id"])
	require.Equal(t, true, out["secret_id_withheld"])
	require.Contains(t, out["secret_note"], "nomad acl token info",
		"the note must say how the operator retrieves the secret instead")
	require.NotContains(t, out, "secret_id")
}

// TestReadACLTokenDisclosesWhenAllowed proves the gate is the only thing
// withholding it, and that disclosure carries the handling warning.
func TestReadACLTokenDisclosesWhenAllowed(t *testing.T) {
	h := newHarness(t, aclEnabled, func(c *config.Config) { c.AllowTokenSecrets = true })
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	out := h.ok("read_acl_token", map[string]any{"accessor_id": testAccessor})
	require.Equal(t, testSecret, out["secret_id"])
	require.Contains(t, out["secret_warning"], "Do not repeat it back")
}

// TestCreateACLTokenWithholdsTheSecret covers the moment the secret actually
// comes into existence, which is the one that matters most.
func TestCreateACLTokenWithholdsTheSecret(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.Handle("PUT /v1/acl/token", func(w http.ResponseWriter, _ *http.Request) {
		writeACLJSON(w, aclTokenFixture())
	})

	raw := h.raw("create_acl_token", map[string]any{
		"name":     "ci-deployer",
		"policies": []any{"deploy"},
	})
	require.NotContains(t, raw, testSecret)

	out := h.ok("create_acl_token", map[string]any{
		"name":     "ci-deployer",
		"policies": []any{"deploy"},
	})
	require.Equal(t, "created", out["action"])
	require.Equal(t, testAccessor, out["accessor_id"])
	require.Equal(t, true, out["secret_id_withheld"])
	// A token created with no TTL is forever, and Nomad cannot add one later.
	require.Contains(t, out["expiry_note"], "NO expiry")
}

func TestCreateACLTokenFlagsAManagementToken(t *testing.T) {
	h := newHarness(t, aclEnabled)
	fixture := aclTokenFixture()
	fixture["Type"] = "management"
	fixture["Policies"] = []string{}
	h.nomad.Handle("PUT /v1/acl/token", func(w http.ResponseWriter, _ *http.Request) {
		writeACLJSON(w, fixture)
	})

	out := h.ok("create_acl_token", map[string]any{"name": "break-glass", "type": "management"})
	require.Contains(t, out["warning"], "MANAGEMENT token")
	require.Contains(t, out["capabilities"], "every capability")
}

// TestListACLTokensNeverCarriesASecret guards the projection rather than the
// endpoint: Nomad's list stub has no SecretID, but a future change that
// switched this tool to the full object must not leak one.
func TestListACLTokensNeverCarriesASecret(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/tokens", []map[string]any{
		aclTokenFixture(),
		{
			"AccessorID": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"SecretID":   testSecret,
			"Name":       "root",
			"Type":       "management",
			"CreateTime": "2026-01-01T00:00:00Z",
		},
	})

	raw := h.raw("list_acl_tokens", nil)
	require.NotContains(t, raw, testSecret)

	out := h.ok("list_acl_tokens", nil)
	require.EqualValues(t, 2, out["count"])
	require.Contains(t, out["note"], "management tokens",
		"a management token in the list is a finding worth stating in words")
}

// --- merge semantics --------------------------------------------------------

// TestUpdateACLTokenPreservesUnmentionedFields is the read-before-write
// guarantee. Nomad's update replaces the whole token, so a rename that sent
// only the name would silently strip the token's policies.
func TestUpdateACLTokenPreservesUnmentionedFields(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	out := h.ok("update_acl_token", map[string]any{
		"accessor_id": testAccessor,
		"name":        "ci-deployer-renamed",
	})
	require.Equal(t, "updated", out["action"])

	sent := h.nomad.Last("/v1/acl/token/" + testAccessor)
	require.Equal(t, http.MethodPut, sent.Method)

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(sent.Body), &body))
	require.Equal(t, "ci-deployer-renamed", body["Name"])
	require.Equal(t, []any{"deploy"}, body["Policies"],
		"renaming a token must not drop the policies it was not asked about")
}

// TestUpdateACLTokenReplacesWhatItIsGiven is the other half: a supplied list is
// the whole list, and the response has to say what it displaced.
func TestUpdateACLTokenReplacesWhatItIsGiven(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	out := h.ok("update_acl_token", map[string]any{
		"accessor_id": testAccessor,
		"policies":    []any{"read-only"},
	})

	changed, ok := out["changed"].(map[string]any)
	require.True(t, ok, "the response must say what changed: %v", out)
	policies := changed["policies"].(map[string]any)
	require.Equal(t, []any{"deploy"}, policies["from"])
	require.Equal(t, []any{"read-only"}, policies["to"])
	require.Contains(t, out["note"], "secret is unchanged")
}

func TestUpdateACLTokenRefusesANoOp(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	msg := h.fails("update_acl_token", map[string]any{"accessor_id": testAccessor})
	require.Contains(t, msg, "Nothing to update")
}

// TestWriteACLPolicyCarriesTheJobACLForward: a policy attached to a workload
// identity must not be silently detached by an unrelated rules update, and
// write_acl_policy cannot set the attachment itself.
func TestWriteACLPolicyCarriesTheJobACLForward(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/policy/workload", map[string]any{
		"Name":        "workload",
		"Description": "for the web job",
		"Rules":       `namespace "default" { policy = "read" }`,
		"JobACL":      map[string]any{"Namespace": "default", "JobID": "web"},
	})

	out := h.ok("write_acl_policy", map[string]any{
		"name":  "workload",
		"rules": `namespace "default" { policy = "write" }`,
	})
	require.Equal(t, "replaced", out["action"])
	require.Contains(t, out, "job_acl_preserved")
	require.Contains(t, out["warning"], "replaced in full")

	sent := h.nomad.Last("/v1/acl/policy/workload")
	require.Equal(t, http.MethodPut, sent.Method)
	require.Contains(t, sent.Body, `"JobID":"web"`,
		"the workload attachment was dropped by a rules-only update")
	require.Contains(t, sent.Body, "for the web job",
		"an omitted description must mean unchanged, not cleared")
}

func TestWriteACLPolicyRefusesEmptyRules(t *testing.T) {
	h := newHarness(t, aclEnabled)

	msg := h.fails("write_acl_policy", map[string]any{"name": "p", "rules": "   "})
	require.Contains(t, msg, "silently revokes access")
}

// TestUpdateACLRoleResolvesTheNameToAnID: Nomad's update endpoint needs the ID,
// and people have the name. The lookup is what bridges that.
func TestUpdateACLRoleResolvesTheNameToAnID(t *testing.T) {
	h := newHarness(t, aclEnabled)
	roleID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	h.nomad.JSON("/v1/acl/role/name/deployers", map[string]any{
		"ID":       roleID,
		"Name":     "deployers",
		"Policies": []map[string]any{{"Name": "deploy"}},
	})
	h.nomad.JSON("/v1/acl/role/"+roleID, map[string]any{
		"ID":       roleID,
		"Name":     "deployers",
		"Policies": []map[string]any{{"Name": "deploy"}, {"Name": "read-logs"}},
	})

	out := h.ok("update_acl_role", map[string]any{
		"name":     "deployers",
		"policies": []any{"deploy", "read-logs"},
	})
	require.Equal(t, roleID, out["id"])
	require.Contains(t, out["warning"], "revoked from all of those tokens")

	sent := h.nomad.Last("/v1/acl/role/" + roleID)
	require.Equal(t, http.MethodPut, sent.Method)
	require.Contains(t, sent.Body, "read-logs")
}

func TestUpdateACLRoleRefusesAnEmptyPolicyList(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/role/name/deployers", map[string]any{
		"ID":       "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"Name":     "deployers",
		"Policies": []map[string]any{{"Name": "deploy"}},
	})

	msg := h.fails("update_acl_role", map[string]any{
		"name":     "deployers",
		"policies": []any{},
	})
	require.Contains(t, msg, "at least one policy")
}

func TestReadACLRoleRequiresAnIdentifier(t *testing.T) {
	h := newHarness(t, aclEnabled)
	require.Contains(t, h.fails("read_acl_role", nil), "either 'name' or 'id'")
}

// TestACLPermissionDeniedNamesTheCapability: Nomad's 403 body is the bare
// string "Permission denied", and a model that reads only that has nothing to
// act on. The tools supply the missing capability name.
func TestACLPermissionDeniedNamesTheCapability(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.Forbidden("/v1/acl/policies")

	msg := h.fails("list_acl_policies", nil)
	require.Contains(t, msg, "acl:read")
}

// writeACLJSON writes a Nomad-shaped JSON response for the write endpoints,
// which nomadtest's JSON helper does not cover because it ignores the method.
func writeACLJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Nomad-Index", "1")
	data, _ := json.Marshal(body)
	_, _ = w.Write(data)
}

// testProviderWithACL is testProvider with the ACL switch under the test's
// control, which testProvider itself does not offer because every other test in
// this package wants it on.
func testProviderWithACL(t *testing.T, enableACL bool) *client.Provider {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	p, err := client.New(&config.Config{
		NomadAddr:      "http://127.0.0.1:1",
		NomadNamespace: config.DefaultNomadNamespace,
		ReadOnly:       true,
		MaxLogBytes:    config.DefaultMaxLogBytes,
		EnableACL:      enableACL,
	}, logger)
	require.NoError(t, err)
	return p
}

// aclToolsAreNamedConsistently is folded into the name test in tools_test.go;
// this only pins the prefix, which is what makes them findable as a group.
func TestACLToolNamesSharePrefix(t *testing.T) {
	for _, name := range aclTools {
		require.True(t, strings.Contains(name, "_acl_"),
			"%q does not read as an ACL tool", name)
	}
}

// TestUpdateACLTokenDoesNotSendTheSecretBack: Nomad keys the update on the
// accessor ID and preserves the existing secret itself, so putting the
// credential in an outgoing request body would be gratuitous.
func TestUpdateACLTokenDoesNotSendTheSecretBack(t *testing.T) {
	h := newHarness(t, aclEnabled)
	h.nomad.JSON("/v1/acl/token/"+testAccessor, aclTokenFixture())

	h.ok("update_acl_token", map[string]any{
		"accessor_id": testAccessor,
		"policies":    []any{"read-only"},
	})

	sent := h.nomad.Last("/v1/acl/token/" + testAccessor)
	require.Equal(t, http.MethodPut, sent.Method)
	require.NotContains(t, sent.Body, testSecret,
		"the token's secret was written into the update request body")
}
