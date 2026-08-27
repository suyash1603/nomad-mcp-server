// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestHandshakeAndCatalog is the smoke test. If this fails, nothing below it
// means anything.
func TestHandshakeAndCatalog(t *testing.T) {
	c := newClient(t)

	raw := c.request("tools/list", nil)

	var listed struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Annotations struct {
				ReadOnlyHint *bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}

	if len(listed.Tools) < 50 {
		t.Fatalf("expected the full catalog, got %d tools", len(listed.Tools))
	}

	// Mutating tools are listed even in read-only mode, on purpose: tools/list
	// should describe the server honestly and a blocked call should explain
	// itself rather than looking like an unknown tool.
	var mutating int
	for _, tool := range listed.Tools {
		if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
			mutating++
		}
	}
	if mutating == 0 {
		t.Fatal("no mutating tools were listed; read-only mode must not hide them")
	}
	t.Logf("catalog: %d tools, %d of them mutating", len(listed.Tools), mutating)
}

func TestClusterStatusAgainstARealAgent(t *testing.T) {
	c := newClient(t)

	out := c.tool("get_cluster_status", nil)

	leader, _ := out["leader"].(string)
	if leader == "" {
		t.Fatal("a running dev agent must report a leader")
	}
	t.Logf("leader: %s", leader)
}

// TestReadOnlyModeRefusesARealWrite is the safety property, checked against the
// shipped binary rather than the gate in isolation.
func TestReadOnlyModeRefusesARealWrite(t *testing.T) {
	c := newClient(t) // NOMAD_MCP_READ_ONLY=true is the harness default

	// A name no other test submits. Every test in this package shares one
	// Nomad agent, so asserting "this job does not exist" against a name
	// another test legitimately creates makes the result depend on the order
	// the files happen to be compiled in.
	const probe = "readonly-refusal-probe"
	jobspec := strings.Replace(
		example(t, "hello-service.nomad.hcl"), `job "hello-service"`, `job "`+probe+`"`, 1)
	if !strings.Contains(jobspec, probe) {
		t.Fatal("the example jobspec no longer declares job \"hello-service\"; update this test")
	}

	msg := c.toolFails("run_job", map[string]any{"jobspec": jobspec})

	for _, want := range []string{"read-only", "NOMAD_MCP_READ_ONLY", "do not retry"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q; got:\n%s", want, msg)
		}
	}

	// And the job must genuinely not exist afterwards.
	jobs := c.tool("list_jobs", nil)
	if strings.Contains(mustJSON(t, jobs), probe) {
		t.Fatal("a refused run_job created the job anyway")
	}
}

// TestFullTroubleshootingPathOnRealJobs is the test this whole suite exists for.
//
// It submits the two real example jobspecs through the MCP layer and then walks
// the exact chain the troubleshoot_failing_job prompt prescribes, asserting the
// chain actually terminates in an explanation. Everything is done through the
// server: the only thing that talks to Nomad directly is the readiness check.
func TestFullTroubleshootingPathOnRealJobs(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	t.Run("submit the healthy job", func(t *testing.T) {
		out := c.tool("run_job", map[string]any{
			"jobspec": example(t, "hello-service.nomad.hcl"),
		})
		if out["eval_id"] == "" || out["eval_id"] == nil {
			t.Fatal("run_job must return the evaluation it created")
		}
		t.Logf("hello-service: %v, eval %v", out["action"], out["eval_id"])
	})

	t.Run("submit the unplaceable job", func(t *testing.T) {
		c.tool("run_job", map[string]any{
			"jobspec": example(t, "unplaceable.nomad.hcl"),
		})
	})

	t.Run("both jobs appear in list_jobs", func(t *testing.T) {
		eventually(t, 30*time.Second, "both jobs to be listed", func() bool {
			body := mustJSON(t, c.tool("list_jobs", nil))
			return strings.Contains(body, "hello-service") && strings.Contains(body, "unplaceable")
		})
	})

	t.Run("the healthy job actually runs", func(t *testing.T) {
		eventually(t, 90*time.Second, "hello-service to have a running allocation", func() bool {
			out := c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})
			for _, item := range itemsOf(out) {
				if item["client_status"] == "running" {
					return true
				}
			}
			return false
		})
	})

	t.Run("the summary flags the stuck job as unhealthy", func(t *testing.T) {
		eventually(t, 30*time.Second, "unplaceable to be summarised as unhealthy", func() bool {
			out := c.tool("read_job_summary", map[string]any{"job_id": "unplaceable"})
			healthy, _ := out["healthy"].(bool)
			return !healthy
		})

		out := c.tool("read_job_summary", map[string]any{"job_id": "unplaceable"})
		note, _ := out["note"].(string)
		if !strings.Contains(note, "list_job_evaluations") {
			t.Errorf("the summary should send the reader to the evaluations; note was %q", note)
		}
	})

	t.Run("the stuck job has no allocations, and says why", func(t *testing.T) {
		out := c.tool("list_job_allocations", map[string]any{"job_id": "unplaceable"})
		if count, _ := out["count"].(float64); count != 0 {
			t.Fatalf("expected zero allocations, got %v", count)
		}
		note, _ := out["note"].(string)
		if !strings.Contains(note, "list_job_evaluations") {
			t.Errorf("zero allocations is the placement-failure signal and must say so; got %q", note)
		}
	})

	t.Run("the evaluation explains the placement failure", func(t *testing.T) {
		var explanation string

		eventually(t, 30*time.Second, "a placement failure to be explained", func() bool {
			evals := c.tool("list_job_evaluations", map[string]any{"job_id": "unplaceable"})
			for _, e := range itemsOf(evals) {
				id, _ := e["id"].(string)
				if id == "" {
					continue
				}
				full := c.tool("read_evaluation", map[string]any{"eval_id": id})
				if s, _ := full["explanation"].(string); s != "" {
					explanation = s
					return true
				}
			}
			return false
		})

		t.Logf("explanation: %s", explanation)

		// This is the payoff. Nomad reports the failure as counters; the server
		// has to turn them into something a model can act on.
		if !strings.Contains(explanation, "impossible") {
			t.Errorf("the explanation should name the failing task group; got %q", explanation)
		}
		lowered := strings.ToLower(explanation)
		if !strings.Contains(lowered, "constraint") && !strings.Contains(lowered, "eligible") {
			t.Errorf("the explanation should name what blocked placement; got %q", explanation)
		}
	})

	t.Run("logs come back from the healthy allocation", func(t *testing.T) {
		var allocID string
		for _, item := range itemsOf(c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})) {
			if item["client_status"] == "running" {
				allocID, _ = item["id"].(string)
				break
			}
		}
		if allocID == "" {
			t.Skip("no running allocation to read logs from")
		}

		res := c.callTool("read_allocation_logs", map[string]any{
			"alloc_id": allocID,
			"log_type": "stdout",
		})
		if res.IsError {
			t.Fatalf("reading logs failed: %s", res.text())
		}
		if !strings.Contains(strings.ToLower(res.text()), "untrusted") {
			t.Error("log output must be labelled as untrusted; it is written by the workload")
		}
	})

	t.Run("resources resolve for a real job", func(t *testing.T) {
		raw := c.request("resources/read", map[string]any{
			"uri": "nomad://jobs/default/hello-service",
		})

		var out struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Contents) != 1 {
			t.Fatalf("expected one document, got %d", len(out.Contents))
		}
		if !strings.Contains(out.Contents[0].Text, "hello-service") {
			t.Error("the resource should contain the job it names")
		}

		// And it must match what the tool returns, byte for byte.
		viaTool := c.callTool("read_job", map[string]any{"job_id": "hello-service"})
		if viaTool.text() != out.Contents[0].Text {
			t.Error("a resource read and the equivalent tool call must agree exactly")
		}
	})

	t.Run("stop both jobs", func(t *testing.T) {
		for _, id := range []string{"hello-service", "unplaceable"} {
			out := c.tool("stop_job", map[string]any{"job_id": id, "purge": true})
			t.Logf("stopped %s: %v", id, out["action"])
		}

		eventually(t, 30*time.Second, "both jobs to be purged", func() bool {
			body := mustJSON(t, c.tool("list_jobs", nil))
			return !strings.Contains(body, "hello-service") && !strings.Contains(body, "unplaceable")
		})
	})
}

// TestBatchJobFailureIsDiagnosed uses the third example, whose "flaky" group
// exits non-zero on purpose, to check the runtime-failure half of the fork.
func TestBatchJobFailureIsDiagnosed(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	c.tool("run_job", map[string]any{
		"jobspec": example(t, "batch-report.nomad.hcl"),
	})
	t.Cleanup(func() {
		c.callTool("stop_job", map[string]any{"job_id": "batch-report", "purge": true})
	})

	var failed map[string]any

	eventually(t, 120*time.Second, "the flaky task group to fail", func() bool {
		for _, item := range itemsOf(c.tool("list_job_allocations", map[string]any{"job_id": "batch-report"})) {
			if item["task_group"] != "flaky" {
				continue
			}
			id, _ := item["id"].(string)
			if id == "" {
				continue
			}
			full := c.tool("read_allocation", map[string]any{"alloc_id": id})
			if full["client_status"] == "failed" {
				failed = full
				return true
			}
		}
		return false
	})

	diagnosis, _ := failed["diagnosis"].(string)
	t.Logf("diagnosis: %s", diagnosis)
	if diagnosis == "" {
		t.Error("a failed allocation should carry a plain-language diagnosis")
	}

	body := mustJSON(t, failed)
	if !strings.Contains(body, "exit_code") {
		t.Error("the exit code is the first thing anyone looks at on a failed task")
	}
}

// TestNamespaceAllowlistOnARealCluster proves the allowlist is enforced by the
// binary, not merely by the package that implements it.
func TestNamespaceAllowlistOnARealCluster(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_ALLOWED_NAMESPACES=production")

	msg := c.toolFails("list_jobs", map[string]any{"namespace": "default"})
	if !strings.Contains(msg, "production") {
		t.Errorf("the refusal should say which namespaces are allowed; got:\n%s", msg)
	}
}

// TestVariableReadsAreOffByDefault checks the second gate end to end.
func TestVariableReadsAreOffByDefault(t *testing.T) {
	c := newClient(t)

	msg := c.toolFails("read_variable", map[string]any{"path": "anything/at/all"})
	if !strings.Contains(msg, "NOMAD_MCP_ALLOW_VARIABLE_READS") {
		t.Errorf("the refusal should name the setting that would enable it; got:\n%s", msg)
	}
}

func TestPromptsRenderAgainstARealServer(t *testing.T) {
	c := newClient(t)

	raw := c.request("prompts/get", map[string]any{
		"name":      "troubleshoot_failing_job",
		"arguments": map[string]any{"job_id": "unplaceable"},
	})

	var out struct {
		Description string `json:"description"`
		Messages    []struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected one message, got %d", len(out.Messages))
	}

	text := out.Messages[0].Content.Text
	for _, want := range []string{"unplaceable", "READ-ONLY", "list_job_evaluations"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt should mention %q", want)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func itemsOf(out map[string]any) []map[string]any {
	raw, ok := out["items"].([]any)
	if !ok {
		return nil
	}

	list := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			list = append(list, m)
		}
	}
	return list
}
