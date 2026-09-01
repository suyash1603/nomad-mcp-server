// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestClusterCapacityAgainstARealAgent checks the arithmetic holds against a
// real node rather than a fixture, and that the headline distinction survives.
func TestClusterCapacityAgainstARealAgent(t *testing.T) {
	c := newClient(t)

	out := c.tool("get_cluster_capacity", nil)
	body := mustJSON(t, out)

	usable, ok := out["usable_capacity"].(map[string]any)
	if !ok {
		t.Fatalf("no usable_capacity in result: %s", body)
	}

	total, _ := usable["memory_mb_total"].(float64)
	if total <= 0 {
		t.Errorf("a running agent must report some memory: %s", body)
	}

	alloc, _ := usable["memory_mb_allocated"].(float64)
	free, _ := usable["memory_mb_free"].(float64)
	if alloc+free != total {
		t.Errorf("allocated + free should equal total: %v + %v != %v", alloc, free, total)
	}

	largest, ok := out["largest_placeable_on_one_node"].(map[string]any)
	if !ok {
		t.Fatalf("the per-node figure is the point of this tool: %s", body)
	}
	// It can never exceed the cluster-wide free figure.
	if lm, _ := largest["memory_mb"].(float64); lm > free {
		t.Errorf("largest placeable (%v) exceeds cluster free (%v)", lm, free)
	}

	note, _ := out["note"].(string)
	if !strings.Contains(note, "RESERVATIONS") {
		t.Errorf("the note must say these are reservations, not measured use: %q", note)
	}
}

func TestClusterCapacityGrouping(t *testing.T) {
	c := newClient(t)

	for _, by := range []string{"node_pool", "datacenter", "node_class"} {
		t.Run(by, func(t *testing.T) {
			out := c.tool("get_cluster_capacity", map[string]any{"group_by": by})
			if got, _ := out["grouped_by"].(string); got != by {
				t.Errorf("grouped_by = %q, want %q", got, by)
			}
			if groups, _ := out["groups"].([]any); len(groups) == 0 {
				t.Error("expected at least one group on a cluster with a node")
			}
		})
	}

	t.Run("none", func(t *testing.T) {
		out := c.tool("get_cluster_capacity", map[string]any{"group_by": "none"})
		if groups, _ := out["groups"].([]any); len(groups) != 0 {
			t.Errorf("group_by=none should return no groups, got %d", len(groups))
		}
	})
}

// TestExplainPlacementOnRealJobs is the one that matters: a job that fits and
// a job that cannot, against a real cluster.
func TestExplainPlacementOnRealJobs(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})
	c.tool("run_job", map[string]any{"jobspec": example(t, "unplaceable.nomad.hcl")})

	eventually(t, 90*time.Second, "hello-service to be running", func() bool {
		out := c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})
		return strings.Contains(mustJSON(t, out), `"running"`)
	})

	t.Run("a job that fits reports room for its allocations", func(t *testing.T) {
		out := c.tool("explain_placement", map[string]any{"job_id": "hello-service"})
		body := mustJSON(t, out)

		groups, _ := out["task_groups"].([]any)
		if len(groups) == 0 {
			t.Fatalf("no task groups analysed: %s", body)
		}
		g, _ := groups[0].(map[string]any)

		fitting, _ := g["nodes_that_fit"].(float64)
		if fitting < 1 {
			t.Errorf("this job is actually running, so a node must fit it: %s", body)
		}

		// Nomad packs several allocations of a group onto one node, so capacity
		// is counted in allocations. A single-node cluster running count=2
		// proves nodes are the wrong unit.
		capacity, _ := g["allocations_that_fit"].(float64)
		count, _ := g["count"].(float64)
		if capacity < count {
			t.Errorf("the job is running %v allocations, so at least that many must fit: %s", count, body)
		}
		if verdict, _ := g["verdict"].(string); strings.Contains(verdict, "stay queued") {
			t.Errorf("a running job must not be reported as queued: %q", verdict)
		}
	})

	t.Run("an unplaceable job reports why and flags unevaluated constraints", func(t *testing.T) {
		out := c.tool("explain_placement", map[string]any{"job_id": "unplaceable"})
		body := mustJSON(t, out)

		groups, _ := out["task_groups"].([]any)
		g, _ := groups[0].(map[string]any)

		if fitting, _ := g["nodes_that_fit"].(float64); fitting != 0 {
			t.Errorf("no node should fit the unplaceable job: %s", body)
		}
		if !strings.Contains(body, "rejection_summary") {
			t.Errorf("a rejection must be explained: %s", body)
		}

		// The jobspec carries a node.class constraint this tool does not
		// evaluate, and saying so is what keeps a clean result honest.
		if !strings.Contains(body, "constraints_not_evaluated") {
			t.Errorf("the node.class constraint should be listed unevaluated: %s", body)
		}
		if note, _ := out["note"].(string); !strings.Contains(note, "plan_job") {
			t.Errorf("the note must point at the authoritative answer: %q", note)
		}
	})

	t.Run("an unknown task group is rejected clearly", func(t *testing.T) {
		msg := c.toolFails("explain_placement", map[string]any{
			"job_id": "hello-service", "task_group": "no-such-group",
		})
		if !strings.Contains(msg, "read_job") {
			t.Errorf("unexpected error: %s", msg)
		}
	})
}

// TestAnalyzeJobResourcesAgainstARealJob checks the measurement caveat travels
// with every result, since the numbers are single samples.
func TestAnalyzeJobResourcesAgainstARealJob(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})
	eventually(t, 90*time.Second, "hello-service to be running", func() bool {
		out := c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})
		return strings.Contains(mustJSON(t, out), `"running"`)
	})

	out := c.tool("analyze_job_resources", map[string]any{"job_id": "hello-service"})
	body := mustJSON(t, out)

	caveat, _ := out["measurement_caveat"].(string)
	if !strings.Contains(caveat, "instantaneous sample") {
		t.Errorf("every result must carry the sampling caveat: %q", caveat)
	}
	if !strings.Contains(caveat, "not averages or percentiles") {
		t.Errorf("the caveat must rule out percentiles explicitly: %q", caveat)
	}

	tasks, _ := out["tasks"].([]any)
	if len(tasks) == 0 {
		t.Fatalf("expected a task in the result: %s", body)
	}
	task, _ := tasks[0].(map[string]any)

	if _, ok := task["verdict"]; !ok {
		t.Errorf("each task needs a verdict: %s", body)
	}
	if _, ok := task["memory_mb_requested"]; !ok {
		t.Errorf("the reservation must be reported: %s", body)
	}
}

// TestCapacityToolsAreReadOnly — none of these should need writes enabled.
func TestCapacityToolsAreReadOnly(t *testing.T) {
	c := newClient(t)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_cluster_capacity", nil},
		{"explain_placement", map[string]any{"job_id": "hello-service"}},
		{"analyze_job_resources", map[string]any{"job_id": "hello-service"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			if res := c.callTool(tc.tool, tc.args); res.IsError {
				t.Fatalf("%s was refused in read-only mode: %s", tc.tool, res.text())
			}
		})
	}
}

func TestCapacityToolsetMembership(t *testing.T) {
	got := listedTools(t, "NOMAD_MCP_TOOLSETS=capacity")

	for _, name := range []string{"get_cluster_capacity", "explain_placement", "analyze_job_resources"} {
		if !has(got, name) {
			t.Errorf("the capacity toolset does not offer %s", name)
		}
	}
	if has(got, "list_jobs") {
		t.Error("the capacity toolset should not offer tools from other toolsets")
	}
}
