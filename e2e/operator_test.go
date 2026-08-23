//go:build e2e

// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// These exercise the operator tools against a real `nomad agent -dev`.
//
// The unit tests put a fake Nomad behind these handlers, which proves the
// projection and the error mapping. What a fake cannot prove is that the
// request shape is one Nomad actually accepts — a wrong field name, a value
// Nomad validates differently, or an endpoint that moved. That is what these
// are for, and it is why they assert on Nomad's acceptance rather than on
// wording the unit tests already pin.

// check_connection is the tool people run first when setting this up, so it
// must be correct against a real agent rather than only against a fixture.
func TestCheckConnectionAgainstARealAgent(t *testing.T) {
	c := newClient(t)

	out := c.tool("check_connection", nil)

	if reached, _ := out["reached"].(bool); !reached {
		t.Fatalf("check_connection did not reach the dev agent: %s", mustJSON(t, out))
	}

	// A dev agent is Community Edition, and the probe must say so rather than
	// leaving it unknown. If this fails, the Enterprise tools are being offered
	// to a cluster that cannot serve them.
	if edition, _ := out["edition"].(string); edition != "community" {
		t.Errorf("a dev agent is Community Edition; check_connection reported %q", edition)
	}

	checks, ok := out["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatal("check_connection returned no checks")
	}

	// Every failing or warning check has to carry a fix. Without that this is
	// a status page, not a diagnostic.
	for _, raw := range checks {
		check, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		status, _ := check["status"].(string)
		if status != "fail" && status != "warn" {
			continue
		}
		if fix, _ := check["fix"].(string); strings.TrimSpace(fix) == "" {
			t.Errorf("check %q is %s but suggests nothing", check["check"], status)
		}
	}
}

// A dev agent is Community Edition, so the Enterprise tools must be absent from
// the catalog entirely. This is the end-to-end proof of the edition probe: the
// unit tests can only check the filtering given an answer, not that the answer
// is right against a real cluster.
func TestEnterpriseToolsAreAbsentOnCommunityEdition(t *testing.T) {
	c := newClient(t)

	listed := mustJSON(t, map[string]any{"tools": toolNames(t, c)})

	for _, name := range []string{
		"list_quotas", "get_license", "list_sentinel_policies", "list_recommendations",
	} {
		if strings.Contains(listed, `"`+name+`"`) {
			t.Errorf("%q is Enterprise-only and should not be offered by a Community Edition cluster", name)
		}
	}

	// And the core tools must survive the filtering.
	for _, name := range []string{"list_jobs", "read_node_pool", "check_connection", "edit_job"} {
		if !strings.Contains(listed, `"`+name+`"`) {
			t.Errorf("%q is not Enterprise-only but was dropped from the catalog", name)
		}
	}
}

func TestNodePoolLifecycleAgainstARealAgent(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	const pool = "mcp-e2e-pool"

	created := c.tool("create_node_pool", map[string]any{
		"name":        pool,
		"description": "created by the end-to-end suite",
		"meta":        map[string]any{"owner": "e2e"},
	})
	if action, _ := created["action"].(string); action != "created" {
		t.Fatalf("expected the pool to be created, got %q: %s", action, mustJSON(t, created))
	}

	read := c.tool("read_node_pool", map[string]any{"name": pool})
	if read["name"] != pool {
		t.Fatalf("read_node_pool returned the wrong pool: %s", mustJSON(t, read))
	}

	// A brand-new pool has no nodes, and the tool must say why that matters
	// rather than leaving a zero for someone to interpret.
	if !strings.Contains(mustJSON(t, read), "no client nodes") {
		t.Errorf("an empty pool should be explained, not just counted: %s", mustJSON(t, read))
	}

	deleted := c.tool("delete_node_pool", map[string]any{"name": pool})
	if ok, _ := deleted["deleted"].(bool); !ok {
		t.Fatalf("the pool was not deleted: %s", mustJSON(t, deleted))
	}

	// Deleting it really did remove it.
	if msg := c.toolFails("read_node_pool", map[string]any{"name": pool}); msg == "" {
		t.Error("reading a deleted pool should fail")
	}
}

func TestBuiltinNodePoolsAreProtected(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	for _, name := range []string{"all", "default"} {
		msg := c.toolFails("delete_node_pool", map[string]any{"name": name})
		if !strings.Contains(msg, "built into Nomad") {
			t.Errorf("deleting the %q pool should be refused here, not by Nomad; got: %s", name, msg)
		}
	}
}

// edit_job is the tool whose whole value is that it does not lose fields, so
// the round trip against real Nomad is the assertion that matters.
func TestEditJobPreservesUnrelatedFieldsAgainstARealAgent(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})

	before := c.tool("read_job", map[string]any{"job_id": "hello-service"})

	// Plan the change first, exactly as the tool's own description tells the
	// model to. A dry run must not touch the job.
	planned := c.tool("edit_job", map[string]any{
		"job_id":  "hello-service",
		"count":   3,
		"dry_run": true,
	})
	if dry, _ := planned["dry_run"].(bool); !dry {
		t.Fatalf("the dry run did not report itself: %s", mustJSON(t, planned))
	}

	if strings.Contains(mustJSON(t, c.tool("read_job", map[string]any{"job_id": "hello-service"})), `"count":3`) {
		t.Error("the dry run changed the job")
	}

	applied := c.tool("edit_job", map[string]any{
		"job_id": "hello-service",
		"count":  3,
	})
	if applied["eval_id"] == nil {
		t.Fatalf("the edit returned no evaluation: %s", mustJSON(t, applied))
	}

	after := c.tool("read_job", map[string]any{"job_id": "hello-service"})

	// The job must still be recognisably the same job. A rebuilt-from-scratch
	// spec is exactly what this tool exists to avoid, and the symptom would be
	// fields present before and missing after.
	beforeJSON, afterJSON := mustJSON(t, before), mustJSON(t, after)
	for _, field := range []string{"hello-service", "web", "server"} {
		if strings.Contains(beforeJSON, field) && !strings.Contains(afterJSON, field) {
			t.Errorf("%q was present before the edit and is gone after it", field)
		}
	}

	// And the change itself landed.
	if !strings.Contains(afterJSON, "3") {
		t.Errorf("the count does not appear to have changed: %s", afterJSON)
	}

	c.tool("stop_job", map[string]any{"job_id": "hello-service", "purge": true})
}

// A no-op edit must not start a deployment that replaces every allocation to
// change nothing at all.
func TestEditJobRefusesANoOpAgainstARealAgent(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})
	defer c.tool("stop_job", map[string]any{"job_id": "hello-service", "purge": true})

	read := c.tool("read_job", map[string]any{"job_id": "hello-service"})

	msg := c.toolFails("edit_job", map[string]any{
		"job_id":   "hello-service",
		"priority": priorityOf(t, read),
	})
	if !strings.Contains(msg, "Nothing was changed") {
		t.Errorf("a no-op edit should say so; got: %s", msg)
	}
}

func TestSchedulerConfigRoundTripsAgainstARealAgent(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	before := c.tool("get_scheduler_config", nil)
	was, _ := before["preemption_batch_jobs"].(bool)

	changed := c.tool("set_scheduler_config", map[string]any{"preemption_batch_jobs": !was})
	if ok, _ := changed["changed"].(bool); !ok {
		t.Fatalf("the change was not applied: %s", mustJSON(t, changed))
	}

	after := c.tool("get_scheduler_config", nil)
	if now, _ := after["preemption_batch_jobs"].(bool); now == was {
		t.Errorf("preemption_batch_jobs did not change: still %v", now)
	}

	// Restore, and confirm the settings that were never mentioned survived the
	// round trip — the API replaces the whole document, so this is the property
	// that breaks quietly.
	c.tool("set_scheduler_config", map[string]any{"preemption_batch_jobs": was})

	restored := c.tool("get_scheduler_config", nil)
	for _, field := range []string{"scheduler_algorithm", "preemption_service_jobs"} {
		if restored[field] != before[field] {
			t.Errorf("%s changed from %v to %v across a round trip that never mentioned it",
				field, before[field], restored[field])
		}
	}
}

// Purging a node whose agent is alive achieves nothing, because it re-registers
// on its next heartbeat. Nomad does not stop you; this server does.
func TestPurgeNodeRefusesTheLiveDevNode(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	nodes := c.tool("list_nodes", nil)
	nodeID := firstNodeID(t, nodes)

	msg := c.toolFails("purge_node", map[string]any{"node_id": nodeID})
	for _, want := range []string{"ready", "re-registers", "drain_node"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q; got:\n%s", want, msg)
		}
	}
}

// The destructive tier is a second gate, so it has to be checked end to end:
// a non-destructive write must still run while a destructive one is refused.
func TestDestructiveTierAgainstARealAgent(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false", "NOMAD_MCP_ALLOW_DESTRUCTIVE=false")

	nodes := c.tool("list_nodes", nil)
	nodeID := firstNodeID(t, nodes)

	// Non-destructive: permitted.
	out := c.tool("set_node_eligibility", map[string]any{
		"node_id":  nodeID,
		"eligible": true,
	})
	if eligible, _ := out["eligible"].(bool); !eligible {
		t.Fatalf("a non-destructive write should run with the destructive tier closed: %s",
			mustJSON(t, out))
	}

	// Destructive: refused, and the refusal must name the right flag.
	msg := c.toolFails("drain_node", map[string]any{"node_id": nodeID})
	for _, want := range []string{"NOMAD_MCP_ALLOW_DESTRUCTIVE=true", "Writes in general ARE enabled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q; got:\n%s", want, msg)
		}
	}
}

func TestNodeStatsAgainstARealAgent(t *testing.T) {
	c := newClient(t)

	nodeID := firstNodeID(t, c.tool("list_nodes", nil))

	out := c.tool("get_node_stats", map[string]any{"node_id": nodeID})

	memory, ok := out["memory"].(map[string]any)
	if !ok {
		t.Fatalf("a live node must report memory: %s", mustJSON(t, out))
	}
	if total, _ := memory["total_mb"].(float64); total <= 0 {
		t.Errorf("total_mb should be positive, got %v", memory["total_mb"])
	}
}

func TestDrainPromptRendersAgainstARealServer(t *testing.T) {
	c := newClient(t)

	nodeID := firstNodeID(t, c.tool("list_nodes", nil))

	raw := c.request("prompts/get", map[string]any{
		"name":      "drain_node_safely",
		"arguments": map[string]any{"node_id": nodeID},
	})
	if !strings.Contains(string(raw), nodeID) {
		t.Error("the prompt should name the node it is about")
	}
	if !strings.Contains(string(raw), "set_node_eligibility") {
		t.Error("the prompt should reach for the gentle step before the drain")
	}
}

// --- helpers ----------------------------------------------------------------

func toolNames(t *testing.T, c *mcpClient) []string {
	t.Helper()

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.request("tools/list", nil), &listed); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func firstNodeID(t *testing.T, out map[string]any) string {
	t.Helper()

	items, ok := out["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("the dev agent should have one client node: %s", mustJSON(t, out))
	}
	node, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected node shape: %s", mustJSON(t, out))
	}
	id, _ := node["node_id"].(string)
	if id == "" {
		id, _ = node["id"].(string)
	}
	if id == "" {
		t.Fatalf("no node ID in %s", mustJSON(t, node))
	}
	return id
}

func priorityOf(t *testing.T, job map[string]any) int {
	t.Helper()

	if p, ok := job["priority"].(float64); ok {
		return int(p)
	}
	// The projection may not surface it; 50 is Nomad's default and is what the
	// example jobspec leaves it at.
	return 50
}
