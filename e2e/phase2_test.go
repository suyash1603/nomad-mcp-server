// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestCSIPluginToolsOnAClusterWithout checks the empty path, which is the one
// a dev agent can actually exercise — and the one most likely to be wrong,
// because an empty list and a broken lookup look the same to a model unless the
// result says which it is.
func TestCSIPluginToolsOnAClusterWithout(t *testing.T) {
	c := newClient(t)

	t.Run("list_csi_plugins explains an empty cluster", func(t *testing.T) {
		out := c.tool("list_csi_plugins", nil)

		if count, _ := out["count"].(float64); count != 0 {
			t.Fatalf("this agent should have no CSI plugins, got %v", count)
		}
		note, _ := out["note"].(string)
		if !strings.Contains(note, "plugins are themselves Nomad jobs") {
			t.Errorf("an empty plugin list must say why that might be: %q", note)
		}
	})

	t.Run("read_csi_plugin on a plugin that does not exist", func(t *testing.T) {
		msg := c.toolFails("read_csi_plugin", map[string]any{"plugin_id": "no-such-plugin"})
		if !strings.Contains(msg, "list_csi_plugins") {
			t.Errorf("the error should point at the tool that lists what exists: %s", msg)
		}
	})

	t.Run("read_csi_plugin requires its argument", func(t *testing.T) {
		msg := c.toolFails("read_csi_plugin", nil)
		if !strings.Contains(msg, "plugin_id") {
			t.Errorf("unexpected error: %s", msg)
		}
	})
}

// TestDiagnoseVolumeOnAMissingVolume checks the not-found path names the other
// volume type, since that is the most common reason a volume "does not exist".
func TestDiagnoseVolumeOnAMissingVolume(t *testing.T) {
	c := newClient(t)

	msg := c.toolFails("diagnose_volume", map[string]any{"volume_id": "no-such-volume"})
	if !strings.Contains(msg, "list_volumes") {
		t.Errorf("the error should point at list_volumes: %s", msg)
	}

	if msg := c.toolFails("diagnose_volume", nil); !strings.Contains(msg, "volume_id") {
		t.Errorf("unexpected error: %s", msg)
	}
}

// TestDiagnoseIntegrationsAgainstARealAgent runs the scan on a cluster with
// neither Vault nor Consul doing anything, which is exactly when the tool must
// not invent findings.
func TestDiagnoseIntegrationsAgainstARealAgent(t *testing.T) {
	c := newClient(t)

	out := c.tool("diagnose_integrations", nil)
	body := mustJSON(t, out)

	if count, _ := out["finding_count"].(float64); count != 0 {
		t.Errorf("a cluster with no integration failures must report none, got: %s", body)
	}

	note, _ := out["note"].(string)
	// Two claims that must always be present: an empty scan is not proof, and
	// the tool says where its boundary is.
	if !strings.Contains(note, "not proof nothing ever went wrong") {
		t.Errorf("an empty scan must not read as proof of absence: %q", note)
	}
	if !strings.Contains(note, "reads Nomad only") {
		t.Errorf("the tool must state that it does not query Vault or Consul: %q", note)
	}

	// It must never echo anything credential-shaped out of the agent config.
	for _, forbidden := range []string{"Token", "TLSCertFile", "TLSKeyFile", "\"Addr\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the result exposes %q from the agent config: %s", forbidden, body)
		}
	}
}

// TestAllocationChecksAgainstARealJob covers the case that motivated the tool:
// an allocation with no Nomad-registered checks, where "no checks" and "no
// failing checks" look identical.
func TestAllocationChecksAgainstARealJob(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})

	var allocID string
	eventually(t, 90*time.Second, "an allocation to exist", func() bool {
		out := c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})
		items, _ := out["items"].([]any)
		for _, raw := range items {
			a, _ := raw.(map[string]any)
			if id, _ := a["id"].(string); id != "" {
				allocID = id
				return true
			}
		}
		return false
	})

	out := c.tool("get_allocation_checks", map[string]any{"alloc_id": allocID})
	note, _ := out["note"].(string)

	count, _ := out["count"].(float64)
	if count == 0 && !strings.Contains(note, "does NOT mean it") {
		t.Errorf("an allocation with no checks must not read as healthy: %q", note)
	}
	if _, ok := out["all_passing"]; !ok {
		t.Error("the result must state whether everything is passing")
	}
}

// TestPhase2ToolsAreReadOnly — none of these should need writes enabled.
func TestPhase2ToolsAreReadOnly(t *testing.T) {
	c := newClient(t) // read-only, the default

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"list_csi_plugins", nil},
		{"diagnose_integrations", nil},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			if res := c.callTool(tc.tool, tc.args); res.IsError {
				t.Fatalf("%s was refused in read-only mode: %s", tc.tool, res.text())
			}
		})
	}
}

// TestPhase2ToolsetMembership checks the new tools land in the toolsets the
// documentation claims, since that is what an operator narrowing the catalog
// relies on.
func TestPhase2ToolsetMembership(t *testing.T) {
	cases := map[string][]string{
		"catalog":     {"list_csi_plugins", "read_csi_plugin"},
		"allocs":      {"get_allocation_checks"},
		"investigate": {"diagnose_volume", "diagnose_integrations"},
	}

	for toolset, want := range cases {
		t.Run(toolset, func(t *testing.T) {
			got := listedTools(t, "NOMAD_MCP_TOOLSETS="+toolset)
			for _, name := range want {
				if !has(got, name) {
					t.Errorf("the %q toolset does not offer %s", toolset, name)
				}
			}
		})
	}
}
