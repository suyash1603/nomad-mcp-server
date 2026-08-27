// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestInvestigationToolsOnRealJobs drives the three investigation tools against
// a real cluster carrying a healthy job and a deliberately unplaceable one.
//
// The unplaceable jobspec is what makes this worth running: its constraint
// matches no node, so Nomad reports the failure with every counter empty and
// NodesEvaluated at zero. Unit tests with hand-built fixtures do not produce
// that shape, and an earlier version of find_problems reported it with no
// reason attached because of exactly that gap.
func TestInvestigationToolsOnRealJobs(t *testing.T) {
	c := newClient(t, "NOMAD_MCP_READ_ONLY=false")

	t.Run("submit both jobs", func(t *testing.T) {
		c.tool("run_job", map[string]any{"jobspec": example(t, "hello-service.nomad.hcl")})
		c.tool("run_job", map[string]any{"jobspec": example(t, "unplaceable.nomad.hcl")})

		eventually(t, 90*time.Second, "hello-service to be running", func() bool {
			out := c.tool("list_job_allocations", map[string]any{"job_id": "hello-service"})
			return strings.Contains(mustJSON(t, out), `"running"`)
		})
	})

	t.Run("find_problems reports the unplaceable job with a reason", func(t *testing.T) {
		var body string
		eventually(t, 60*time.Second, "find_problems to notice the unplaceable job", func() bool {
			body = mustJSON(t, c.tool("find_problems", nil))
			return strings.Contains(body, "queued-work") || strings.Contains(body, "blocked-evaluations")
		})

		if strings.Contains(body, `"looks_healthy":true`) {
			t.Error("a cluster with an unplaceable job must not be reported as healthy")
		}
		if !strings.Contains(body, "unplaceable") {
			t.Errorf("find_problems did not name the unplaceable job: %s", body)
		}
		// The reason is the whole value of the finding. Nomad reports this
		// particular failure as a set of empty counters, and turning that into
		// a sentence is what the projection layer is for.
		if !strings.Contains(body, "no nodes were eligible") {
			t.Errorf("find_problems reported a placement failure without saying why: %s", body)
		}
		if !strings.Contains(body, "next_step") {
			t.Error("findings must carry the tool to call next")
		}
	})

	t.Run("find_problems is scoped and ranked", func(t *testing.T) {
		out := c.tool("find_problems", map[string]any{"max_examples": 1})
		body := mustJSON(t, out)

		if !strings.Contains(body, `"severity":"critical"`) {
			t.Errorf("expected a critical finding: %s", body)
		}
		if !strings.Contains(body, "checks_run") {
			t.Error("the result must say how many checks ran, so a partial scan is visible")
		}
	})

	t.Run("build_job_timeline orders a real job's history", func(t *testing.T) {
		out := c.tool("build_job_timeline", map[string]any{"job_id": "hello-service"})
		body := mustJSON(t, out)

		for _, want := range []string{"job-version", "evaluation", "allocation"} {
			if !strings.Contains(body, want) {
				t.Errorf("the timeline is missing %s events: %s", want, body)
			}
		}
		if !strings.Contains(body, `"sources"`) {
			t.Error("the timeline must report which sources it read")
		}

		events, ok := out["events"].([]any)
		if !ok || len(events) == 0 {
			t.Fatalf("expected timeline events, got %v", out["events"])
		}

		// Oldest first by default, and every event carries a real timestamp.
		var previous string
		for _, raw := range events {
			e, _ := raw.(map[string]any)
			ts, _ := e["time"].(string)
			if ts == "" {
				t.Errorf("event has no timestamp: %v", e)
				continue
			}
			if previous != "" && ts < previous {
				t.Errorf("timeline is out of order: %s came after %s", ts, previous)
			}
			previous = ts
		}
	})

	t.Run("build_job_timeline can reverse and limit", func(t *testing.T) {
		out := c.tool("build_job_timeline", map[string]any{
			"job_id": "hello-service", "newest_first": true, "limit": 3,
		})
		events, _ := out["events"].([]any)
		if len(events) > 3 {
			t.Errorf("limit was not honoured: got %d events", len(events))
		}
	})

	t.Run("build_job_timeline works for a job that never placed", func(t *testing.T) {
		// There are no allocations and therefore no logs, which is exactly when
		// the timeline is the only thing that can explain anything.
		body := mustJSON(t, c.tool("build_job_timeline", map[string]any{"job_id": "unplaceable"}))
		if !strings.Contains(body, "job-version") {
			t.Errorf("a job that never placed still has a submission history: %s", body)
		}
	})

	t.Run("search_job_logs finds output across allocations", func(t *testing.T) {
		var out map[string]any
		eventually(t, 60*time.Second, "the job to have written some output", func() bool {
			out = c.tool("search_job_logs", map[string]any{
				"job_id": "hello-service", "pattern": ".",
			})
			count, _ := out["match_count"].(float64)
			return count > 0
		})

		body := mustJSON(t, out)
		if !strings.Contains(body, "warning") {
			t.Error("log output must always carry the untrusted-content warning")
		}
		if !strings.Contains(body, "allocations_searched") {
			t.Error("the result must say how many allocations were actually read")
		}
	})

	t.Run("search_job_logs is honest when nothing matches", func(t *testing.T) {
		out := c.tool("search_job_logs", map[string]any{
			"job_id": "hello-service", "pattern": "zzz-this-string-does-not-occur",
		})

		if count, _ := out["match_count"].(float64); count != 0 {
			t.Fatalf("expected no matches, got %v", count)
		}
		note, _ := out["note"].(string)
		// "No match" is not "did not happen", and a model given the first will
		// report the second unless the result says otherwise.
		if !strings.Contains(note, "not proof") {
			t.Errorf("an empty search must not read as proof of absence: %q", note)
		}
	})

	t.Run("search_job_logs rejects a bad pattern with recovery advice", func(t *testing.T) {
		msg := c.toolFails("search_job_logs", map[string]any{
			"job_id": "hello-service", "pattern": "what[",
		})
		if !strings.Contains(msg, "not a valid regular expression") {
			t.Errorf("unexpected error: %s", msg)
		}
		if !strings.Contains(msg, "backslash") {
			t.Errorf("the error must say how to fix it: %s", msg)
		}
	})

	t.Run("search_job_logs says when a time filter could not apply", func(t *testing.T) {
		out := c.tool("search_job_logs", map[string]any{
			"job_id": "hello-service", "pattern": ".",
			"since": "2020-01-01T00:00:00Z",
		})
		if _, ok := out["time_filter_note"]; !ok {
			t.Error("a search with a time window must always report whether the window applied")
		}
	})

	t.Run("search_job_logs rejects an unparseable time", func(t *testing.T) {
		msg := c.toolFails("search_job_logs", map[string]any{
			"job_id": "hello-service", "pattern": ".", "since": "yesterday",
		})
		if !strings.Contains(msg, "RFC3339") {
			t.Errorf("the error must name the expected format: %s", msg)
		}
	})
}

// TestInvestigationToolsAreReadOnly checks they run with the default safety
// settings. A tool that only reads must not need writes turned on.
func TestInvestigationToolsAreReadOnly(t *testing.T) {
	c := newClient(t) // read-only, which is the default

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"find_problems", nil},
		{"build_job_timeline", map[string]any{"job_id": "hello-service"}},
		{"search_job_logs", map[string]any{"job_id": "hello-service", "pattern": "."}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			if res := c.callTool(tc.tool, tc.args); res.IsError {
				t.Fatalf("%s was refused in read-only mode: %s", tc.tool, res.text())
			}
		})
	}
}
