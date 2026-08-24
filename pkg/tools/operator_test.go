// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package tools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/internal/nomadtest"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// --- edit_job ---------------------------------------------------------------

// registered returns the job body the fake received on the register call,
// which is the only way to prove what the edit actually sent.
func registered(t *testing.T, h *harness) map[string]any {
	t.Helper()

	req := h.nomad.Last("/v1/jobs")
	require.Equal(t, http.MethodPut, req.Method, "the job should have been registered")

	var body struct {
		Job map[string]any
	}
	require.NoError(t, json.Unmarshal([]byte(req.Body), &body))
	require.NotNil(t, body.Job, "the register call must carry a job")
	return body.Job
}

func firstTask(t *testing.T, job map[string]any) map[string]any {
	t.Helper()

	groups, ok := job["TaskGroups"].([]any)
	require.True(t, ok, "job has no task groups: %v", job)
	require.NotEmpty(t, groups)

	group, ok := groups[0].(map[string]any)
	require.True(t, ok)

	tasks, ok := group["Tasks"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, tasks)

	task, ok := tasks[0].(map[string]any)
	require.True(t, ok)
	return task
}

// The whole point of edit_job: change the one field, carry everything else
// through. A reconstructed jobspec is where the other fields get lost.
func TestEditJobChangesOnlyTheNamedFieldAndKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/jobs", &api.JobRegisterResponse{EvalID: nomadtest.EvalID})

	out := h.ok("edit_job", map[string]any{
		"job_id": nomadtest.HealthyJob,
		"image":  "nginx:1.27",
	})
	require.Equal(t, nomadtest.EvalID, out["eval_id"])

	task := firstTask(t, registered(t, h))
	config, ok := task["Config"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "nginx:1.27", config["image"], "the image should have changed")

	// Untouched fields must survive verbatim.
	env, ok := task["Env"].(map[string]any)
	require.True(t, ok, "the task's environment must be carried through")
	require.Equal(t, "info", env["LOG_LEVEL"])
	require.Equal(t, "hunter2-not-a-real-password", env["DATABASE_PASSWORD"],
		"an unrelated environment variable must not be dropped by an image edit")

	resources, ok := task["Resources"].(map[string]any)
	require.True(t, ok, "the task's resources must be carried through")
	require.EqualValues(t, 500, resources["CPU"])
	require.EqualValues(t, 256, resources["MemoryMB"])
}

func TestEditJobMergesEnvRatherThanReplacingIt(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/jobs", &api.JobRegisterResponse{EvalID: nomadtest.EvalID})

	h.ok("edit_job", map[string]any{
		"job_id": nomadtest.HealthyJob,
		"env":    map[string]any{"LOG_LEVEL": "debug", "NEW_KEY": "value"},
	})

	env := firstTask(t, registered(t, h))["Env"].(map[string]any)
	require.Equal(t, "debug", env["LOG_LEVEL"], "an existing key should be overwritten")
	require.Equal(t, "value", env["NEW_KEY"], "a new key should be added")
	require.Equal(t, "hunter2-not-a-real-password", env["DATABASE_PASSWORD"],
		"a key not mentioned must keep its value")
}

func TestEditJobRemovesEnvKeys(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/jobs", &api.JobRegisterResponse{EvalID: nomadtest.EvalID})

	h.ok("edit_job", map[string]any{
		"job_id":     nomadtest.HealthyJob,
		"env_remove": []any{"LOG_LEVEL"},
	})

	env := firstTask(t, registered(t, h))["Env"].(map[string]any)
	require.NotContains(t, env, "LOG_LEVEL")
	require.Contains(t, env, "DATABASE_PASSWORD", "only the named key should go")
}

// Environment values routinely hold credentials, so the change list must name
// the key without echoing what was set.
func TestEditJobNeverEchoesAnEnvironmentValue(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/jobs", &api.JobRegisterResponse{EvalID: nomadtest.EvalID})

	text := h.raw("edit_job", map[string]any{
		"job_id": nomadtest.HealthyJob,
		"env":    map[string]any{"API_TOKEN": "s3cret-value-here"},
	})

	require.Contains(t, text, "API_TOKEN", "the change list should name the key")
	require.NotContains(t, text, "s3cret-value-here",
		"an environment variable's value must never be echoed back")
}

func TestEditJobDryRunPlansAndDoesNotRegister(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/job/"+nomadtest.HealthyJob+"/plan", &api.JobPlanResponse{
		JobModifyIndex: 7,
	})

	out := h.ok("edit_job", map[string]any{
		"job_id":  nomadtest.HealthyJob,
		"image":   "nginx:1.27",
		"dry_run": true,
	})

	require.Equal(t, true, out["dry_run"])
	require.Contains(t, out["note"], "NOTHING WAS CHANGED")

	for _, req := range h.nomad.Requests() {
		if req.Method == http.MethodPut && req.Path == "/v1/jobs" {
			t.Fatal("a dry run must not register the job")
		}
	}
	require.True(t, h.nomad.Called("/plan"), "a dry run should plan the change")
}

// A no-op edit must say so rather than starting a deployment that replaces
// every allocation to change nothing.
func TestEditJobRefusesWhenNothingWouldChange(t *testing.T) {
	h := newHarness(t)

	msg := h.fails("edit_job", map[string]any{
		"job_id": nomadtest.HealthyJob,
		"image":  "nginx:stable", // already the value in the fixture
	})
	require.Contains(t, msg, "Nothing was changed")
	for _, req := range h.nomad.Requests() {
		require.NotEqual(t, http.MethodPut, req.Method, "nothing should have been registered")
	}
}

// A filter that matches nothing is a typo, not a no-op, and the two are
// indistinguishable to the caller unless it is said out loud.
func TestEditJobReportsAFilterThatMatchedNothing(t *testing.T) {
	h := newHarness(t)

	msg := h.fails("edit_job", map[string]any{
		"job_id":     nomadtest.HealthyJob,
		"task_group": "no-such-group",
		"count":      3,
	})
	require.Contains(t, msg, "no-such-group")
	require.Contains(t, msg, "read_job")

	msg = h.fails("edit_job", map[string]any{
		"job_id": nomadtest.HealthyJob,
		"task":   "no-such-task",
		"image":  "nginx:1.27",
	})
	require.Contains(t, msg, "no-such-task")
}

func TestEditJobRejectsAnOutOfRangePriority(t *testing.T) {
	h := newHarness(t)

	msg := h.fails("edit_job", map[string]any{
		"job_id":   nomadtest.HealthyJob,
		"priority": 500,
	})
	require.Contains(t, msg, "between 1 and 100")
}

// Nomad's own error for this names neither value, so the check is worth having
// here where both are in hand.
func TestEditJobRejectsMemoryMaxBelowMemory(t *testing.T) {
	h := newHarness(t)

	msg := h.fails("edit_job", map[string]any{
		"job_id":        nomadtest.HealthyJob,
		"memory_mb":     512,
		"memory_max_mb": 256,
	})
	require.Contains(t, msg, "memory_max_mb")
	require.Contains(t, msg, "512")
}

func TestEditJobScalesOnlyTheNamedGroup(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/jobs", &api.JobRegisterResponse{EvalID: nomadtest.EvalID})

	h.ok("edit_job", map[string]any{
		"job_id":     nomadtest.HealthyJob,
		"task_group": "app",
		"count":      4,
	})

	groups := registered(t, h)["TaskGroups"].([]any)
	require.EqualValues(t, 4, groups[0].(map[string]any)["Count"])
}

// --- purge_node -------------------------------------------------------------

// Purging a node whose agent is still heartbeating accomplishes nothing: it
// re-registers on its next beat. Nomad does not stop you, so this server does.
func TestPurgeNodeRefusesALiveNode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })

	msg := h.fails("purge_node", map[string]any{"node_id": nomadtest.NodeID})

	require.Contains(t, msg, "ready")
	require.Contains(t, msg, "re-registers")
	require.Contains(t, msg, "drain_node", "the refusal should name what to do instead")
	require.False(t, h.nomad.Called("purge"), "nothing should have been sent to Nomad")
}

func TestPurgeNodeAllowsADownNode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/node/"+nomadtest.NodeID, downNode())
	h.nomad.JSON("/v1/node/"+nomadtest.NodeID+"/purge", &api.NodePurgeResponse{})

	out := h.ok("purge_node", map[string]any{"node_id": nomadtest.NodeID})
	require.Equal(t, true, out["purged"])
	require.True(t, h.nomad.Called("purge"))
}

// --- restart_node_allocations ----------------------------------------------

func TestRestartNodeAllocationsSkipsWhatItCannotRestart(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/node/"+nomadtest.NodeID+"/allocations", []*api.Allocation{
		{ID: nomadtest.AllocID, JobID: "web", TaskGroup: "web", ClientStatus: "running"},
		{ID: nomadtest.FailedAlloc, JobID: "web", TaskGroup: "web", ClientStatus: "complete"},
	})
	h.nomad.JSON("/v1/client/allocation/"+nomadtest.AllocID+"/restart", map[string]any{})

	out := h.ok("restart_node_allocations", map[string]any{"node_id": nomadtest.NodeID})

	require.EqualValues(t, 1, out["restarted_count"])
	require.EqualValues(t, 2, out["total_on_node"])

	skipped, ok := out["skipped"].([]any)
	require.True(t, ok)
	require.Len(t, skipped, 1, "the completed allocation should be skipped, not attempted")
	require.Contains(t, skipped[0].(map[string]any)["reason"], "not running")
}

// The tool must not let anyone believe it restarted the Nomad agent, because
// no API can do that.
func TestRestartNodeAllocationsIsHonestAboutWhatItDid(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/node/"+nomadtest.NodeID+"/allocations", []*api.Allocation{})

	out := h.ok("restart_node_allocations", map[string]any{"node_id": nomadtest.NodeID})

	caveat, ok := out["caveat"].(string)
	require.True(t, ok, "the result must carry the caveat")
	require.Contains(t, caveat, "not the Nomad client agent itself")
}

// --- node pools -------------------------------------------------------------

func TestReadNodePoolExplainsAnEmptyPool(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/node/pool/gpu", &api.NodePool{Name: "gpu", Description: "GPU nodes"})
	h.nomad.JSON("/v1/node/pool/gpu/nodes", []*api.NodeListStub{})
	h.nomad.JSON("/v1/node/pool/gpu/jobs", []*api.JobListStub{})

	out := h.ok("read_node_pool", map[string]any{"name": "gpu"})

	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "no client nodes")
	require.Contains(t, warnings, "client configuration",
		"the warning should say where nodes actually join a pool")
}

func TestReadNodePoolCountsOnlyUsableNodesAsReady(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/node/pool/gpu", &api.NodePool{Name: "gpu"})
	h.nomad.JSON("/v1/node/pool/gpu/nodes", []*api.NodeListStub{
		{ID: "a", Name: "a", Status: "ready", SchedulingEligibility: "eligible"},
		{ID: "b", Name: "b", Status: "ready", SchedulingEligibility: "ineligible"},
		{ID: "c", Name: "c", Status: "ready", SchedulingEligibility: "eligible", Drain: true},
		{ID: "d", Name: "d", Status: "down", SchedulingEligibility: "eligible"},
	})
	h.nomad.JSON("/v1/node/pool/gpu/jobs", []*api.JobListStub{})

	nodes := out(t, h.ok("read_node_pool", map[string]any{"name": "gpu"}), "nodes")
	require.EqualValues(t, 4, nodes["total"])
	require.EqualValues(t, 1, nodes["ready"],
		"only the node that is up, eligible and not draining can take work")
	require.EqualValues(t, 1, nodes["draining"])
	require.EqualValues(t, 1, nodes["ineligible"])
}

func TestCreateNodePoolRefusesTheReservedName(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })

	msg := h.fails("create_node_pool", map[string]any{"name": "all"})
	require.Contains(t, msg, "built-in")
	require.False(t, h.nomad.Called("/v1/node/pool"))
}

func TestDeleteNodePoolRefusesTheBuiltins(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })

	for _, name := range []string{"all", "default"} {
		msg := h.fails("delete_node_pool", map[string]any{"name": name})
		require.Contains(t, msg, "built into Nomad")
	}
}

// A pool created with no Enterprise settings must not send an empty scheduler
// block, which Community Edition rejects for a setting nobody asked for.
func TestCreateNodePoolOmitsTheSchedulerBlockWhenNotAsked(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/node/pools", []*api.NodePool{})
	h.nomad.Handle("/v1/node/pool", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	h.ok("create_node_pool", map[string]any{"name": "gpu", "description": "GPU nodes"})

	body := h.nomad.Last("/v1/node/pool").Body
	require.Contains(t, body, `"SchedulerConfiguration":null`,
		"an Enterprise-only block must be sent as null, not as an empty object")
}

// --- deployments ------------------------------------------------------------

func TestPauseDeploymentSendsThePauseFlag(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/deployment/pause/"+nomadtest.DeploymentID,
		&api.DeploymentUpdateResponse{EvalID: nomadtest.EvalID})

	out := h.ok("pause_deployment", map[string]any{
		"deployment_id": nomadtest.DeploymentID,
		"pause":         false,
	})

	require.Equal(t, false, out["paused"])
	require.Contains(t, h.nomad.Last("/deployment/pause").Body, `"Pause":false`)
	require.Contains(t, out["note"], "resumed")
}

func TestPromoteDeploymentPromotesNamedGroupsOnly(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/deployment/promote/"+nomadtest.DeploymentID,
		&api.DeploymentUpdateResponse{EvalID: nomadtest.EvalID})

	out := h.ok("promote_deployment", map[string]any{
		"deployment_id": nomadtest.DeploymentID,
		"task_groups":   []any{"api"},
	})

	body := h.nomad.Last("/deployment/promote").Body
	require.Contains(t, body, `"Groups":["api"]`)
	require.Contains(t, body, `"All":false`)
	require.Contains(t, out["note"], "still holding at its canaries")
}

func TestSetDeploymentAllocHealthNeedsAtLeastOneAllocation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })

	msg := h.fails("set_deployment_alloc_health", map[string]any{
		"deployment_id": nomadtest.DeploymentID,
	})
	require.Contains(t, msg, "at least one allocation ID")
	require.False(t, h.nomad.Called("allocation-health"))
}

// --- scheduler configuration ------------------------------------------------

// The API replaces the whole configuration, so an omitted argument must be
// carried through from the current state rather than reset to its zero value.
func TestSetSchedulerConfigPreservesSettingsItWasNotAskedingToChange(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/scheduler/configuration", &api.SchedulerConfigurationResponse{
		SchedulerConfig: &api.SchedulerConfiguration{
			SchedulerAlgorithm:            "spread",
			MemoryOversubscriptionEnabled: true,
			PreemptionConfig:              api.PreemptionConfig{SystemSchedulerEnabled: true},
			ModifyIndex:                   9,
		},
	})

	h.nomad.Handle("PUT /v1/operator/scheduler/configuration",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"Updated":true}`))
		})

	h.ok("set_scheduler_config", map[string]any{"preemption_batch_jobs": true})

	body := h.nomad.Last("/v1/operator/scheduler/configuration").Body
	require.Contains(t, body, `"SchedulerAlgorithm":"spread"`,
		"an unmentioned setting must keep its value")
	require.Contains(t, body, `"MemoryOversubscriptionEnabled":true`)
	require.Contains(t, body, `"BatchSchedulerEnabled":true`)
}

func TestSetSchedulerConfigReportsANoOp(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/scheduler/configuration", &api.SchedulerConfigurationResponse{
		SchedulerConfig: &api.SchedulerConfiguration{RejectJobRegistration: true},
	})

	out := h.ok("set_scheduler_config", map[string]any{"reject_job_registration": true})
	require.Equal(t, false, out["changed"])
	for _, req := range h.nomad.Requests() {
		require.NotEqual(t, http.MethodPut, req.Method,
			"a no-op must not write the configuration back")
	}
}

func TestGetSchedulerConfigWarnsAboutTheSilentKillers(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/scheduler/configuration", &api.SchedulerConfigurationResponse{
		SchedulerConfig: &api.SchedulerConfiguration{
			RejectJobRegistration: true,
			PauseEvalBroker:       true,
		},
	})

	warnings := strings.Join(stringsOf(t, h.ok("get_scheduler_config", nil)["warnings"]), " ")
	require.Contains(t, warnings, "reject_job_registration is ON")
	require.Contains(t, warnings, "pause_eval_broker is ON")
	require.Contains(t, warnings, "never placed",
		"the warning must say what the symptom looks like from outside")
}

// --- check_connection -------------------------------------------------------

func TestCheckConnectionReportsPostureAndEdition(t *testing.T) {
	h := newHarness(t)

	out := h.ok("check_connection", nil)

	require.Equal(t, true, out["reached"])
	require.Equal(t, "community", out["edition"], "the fixture agent reports a Community version")

	posture, ok := out["server_posture"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, posture["read_only"])
	require.Contains(t, posture["summary"], "Read-only")
}

// Every check has to carry a fix, or this is just a status page.
func TestCheckConnectionGivesAFixForEveryFailure(t *testing.T) {
	h := newHarness(t)

	for _, check := range checksOf(t, h.ok("check_connection", nil)) {
		if check["status"] == "fail" || check["status"] == "warn" {
			require.NotEmpty(t, check["fix"],
				"check %q failed without saying what to do about it", check["check"])
		}
	}
}

// The localhost warning is the single most common setup mistake, so it must
// fire on the default address rather than only when something has broken.
func TestCheckConnectionWarnsAboutLocalhostInAContainer(t *testing.T) {
	h := newHarness(t)

	var found bool
	for _, check := range checksOf(t, h.ok("check_connection", nil)) {
		if check["check"] == "address" {
			found = true
			require.Equal(t, "warn", check["status"])
			require.Contains(t, check["fix"], "host.docker.internal")
			require.Contains(t, check["fix"], "localhost is the")
		}
	}
	require.True(t, found, "the address check must always run")
}

func TestCheckConnectionSkipsWhatItCannotReach(t *testing.T) {
	h := newHarness(t)
	h.nomad.Status("/v1/status/leader", http.StatusInternalServerError, "boom")

	out := h.ok("check_connection", nil)
	require.Equal(t, false, out["reached"])
	require.Contains(t, out["summary"], "could not be contacted")

	var skipped int
	for _, check := range checksOf(t, out) {
		if check["status"] == "skip" {
			skipped++
		}
	}
	require.NotZero(t, skipped, "checks that cannot mean anything yet must be skipped, not guessed")
}

// --- Enterprise against Community Edition -----------------------------------

// The fixture answers the Enterprise endpoints with a 501, as Community
// Edition does. The tool must translate that rather than surfacing the code.
func TestEnterpriseToolsExplainThemselvesOnCommunityEdition(t *testing.T) {
	h := newHarness(t)

	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"list_quotas", nil},
		{"get_license", nil},
		{"list_sentinel_policies", nil},
		{"list_recommendations", nil},
	} {
		t.Run(tool.name, func(t *testing.T) {
			msg := h.fails(tool.name, tool.args)
			require.Contains(t, msg, "Nomad Enterprise")
			require.Contains(t, msg, "Community Edition")
			require.NotContains(t, msg, "501",
				"the model should be told what happened, not given a status code")
		})
	}
}

// --- helpers ----------------------------------------------------------------

func downNode() *api.Node {
	return &api.Node{
		ID: nomadtest.NodeID, Name: nomadtest.NodeName,
		Status: "down", SchedulingEligibility: "eligible",
		Datacenter: "dc1", NodePool: "default",
	}
}

func out(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()

	v, ok := m[key].(map[string]any)
	require.True(t, ok, "expected an object at %q, got %v", key, m[key])
	return v
}

func stringsOf(t *testing.T, v any) []string {
	t.Helper()

	raw, ok := v.([]any)
	require.True(t, ok, "expected an array, got %v", v)

	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s, ok := r.(string)
		require.True(t, ok)
		out = append(out, s)
	}
	return out
}

// objectsOf reads an array of objects held under a key other than "items",
// which is what the non-list projections use.
func objectsOf(t *testing.T, v any) []map[string]any {
	t.Helper()

	raw, ok := v.([]any)
	require.True(t, ok, "expected an array, got %v", v)

	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		require.True(t, ok, "expected an object in the array, got %v", r)
		out = append(out, m)
	}
	return out
}

func checksOf(t *testing.T, report map[string]any) []map[string]string {
	t.Helper()

	raw, ok := report["checks"].([]any)
	require.True(t, ok, "the report must carry checks")

	out := make([]map[string]string, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		require.True(t, ok)

		flat := map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				flat[k] = s
			}
		}
		out = append(out, flat)
	}
	return out
}

// --- autopilot ---------------------------------------------------------------

func TestGetAutopilotConfigRendersDurationsReadably(t *testing.T) {
	h := newHarness(t)

	out := h.ok("get_autopilot_config", nil)

	require.Equal(t, true, out["cleanup_dead_servers"])
	require.Equal(t, "200ms", out["last_contact_threshold"],
		"durations must be the form Nomad's own configuration uses, not nanoseconds")
	require.Equal(t, "10s", out["server_stabilization_time"])
	require.EqualValues(t, 250, out["max_trailing_logs"])
}

// cleanup_dead_servers = false is the setting behind a cluster that lost quorum
// after a rolling replacement, so the tool has to name that symptom rather than
// just report the flag.
func TestGetAutopilotConfigWarnsWhenDeadServersAreNeverPruned(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers:      false,
		LastContactThreshold:    200 * time.Millisecond,
		ServerStabilizationTime: 10 * time.Second,
	})

	warnings := strings.Join(stringsOf(t, h.ok("get_autopilot_config", nil)["warnings"]), " ")
	require.Contains(t, warnings, "cleanup_dead_servers is OFF")
	require.Contains(t, warnings, "quorum")
	require.Contains(t, warnings, "min_quorum is 0",
		"a zero floor on pruning is its own finding")
}

func TestGetAutopilotConfigExplainsALongStabilizationTime(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers:      true,
		MinQuorum:               3,
		ServerStabilizationTime: 30 * time.Minute,
	})

	warnings := strings.Join(stringsOf(t, h.ok("get_autopilot_config", nil)["warnings"]), " ")
	require.Contains(t, warnings, "30m0s")
	require.Contains(t, warnings, "NON-VOTER",
		"the warning must explain the symptom, not restate the setting")
}

func TestGetAutopilotHealthSummarisesTheFleet(t *testing.T) {
	h := newHarness(t)

	out := h.ok("get_autopilot_health", nil)

	require.Equal(t, true, out["healthy"])
	require.EqualValues(t, 1, out["server_count"])
	require.EqualValues(t, 1, out["voter_count"])

	servers := objectsOf(t, out["servers"])
	require.Len(t, servers, 1)
	require.Equal(t, "server-1.global", servers[0]["name"])
	require.Equal(t, true, servers[0]["voter"])
	require.Equal(t, true, servers[0]["leader"])

	// A single-server cluster tolerates no loss at all, and saying so is the
	// point of the tool even when every server is currently healthy.
	require.Equal(t, true, out["degraded"])
	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "failure_tolerance is 0")
}

func TestGetAutopilotHealthNamesUnhealthyServersAndTheThresholdsBehindThem(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/autopilot/health", &api.OperatorHealthReply{
		Healthy:          false,
		FailureTolerance: 1,
		Leader:           nomadtest.ServerID,
		Servers: []api.ServerHealth{
			{Name: "server-1", Healthy: true, Voter: true, Leader: true, Version: "1.9.0"},
			{Name: "server-2", Healthy: true, Voter: true, Version: "1.9.0"},
			{Name: "server-3", Healthy: false, Voter: true, Version: "1.9.0",
				SerfStatus: "failed", LastContact: 45 * time.Second},
		},
	})

	out := h.ok("get_autopilot_health", nil)
	require.Equal(t, true, out["degraded"])

	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "server-3", "the unhealthy server must be named")
	require.NotContains(t, warnings, "server-2", "healthy voters are not a finding")
	require.Contains(t, warnings, "last_contact_threshold",
		"the warning must point at the setting that produced the verdict")

	servers := objectsOf(t, out["servers"])
	require.Len(t, servers, 3)
	require.Equal(t, "45s", servers[2]["last_contact"])
}

// Healthy but not voting is the "why is my new server not counting toward
// quorum?" case, and is a different finding from unhealthy.
func TestGetAutopilotHealthDistinguishesStabilisingFromUnhealthy(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/autopilot/health", &api.OperatorHealthReply{
		Healthy:          true,
		FailureTolerance: 1,
		Leader:           nomadtest.ServerID,
		Servers: []api.ServerHealth{
			{Name: "server-1", Healthy: true, Voter: true, Leader: true},
			{Name: "server-2", Healthy: true, Voter: true},
			{Name: "server-3", Healthy: true, Voter: true},
			{Name: "server-4", Healthy: true, Voter: false},
		},
	})

	out := h.ok("get_autopilot_health", nil)
	require.EqualValues(t, 4, out["server_count"])
	require.EqualValues(t, 3, out["voter_count"])
	require.Equal(t, false, out["degraded"],
		"a server still stabilising is not a degraded cluster")

	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "server-4")
	require.Contains(t, warnings, "server_stabilization_time")
	require.NotContains(t, warnings, "UNHEALTHY")
}

func TestGetAutopilotHealthFlagsAMixedVersionFleet(t *testing.T) {
	h := newHarness(t)
	h.nomad.JSON("/v1/operator/autopilot/health", &api.OperatorHealthReply{
		Healthy:          true,
		FailureTolerance: 1,
		Leader:           nomadtest.ServerID,
		Servers: []api.ServerHealth{
			{Name: "server-1", Healthy: true, Voter: true, Leader: true, Version: "1.9.0"},
			{Name: "server-2", Healthy: true, Voter: true, Version: "1.9.0"},
			{Name: "server-3", Healthy: true, Voter: true, Version: "1.10.1"},
		},
	})

	out := h.ok("get_autopilot_health", nil)
	require.Equal(t, []string{"1.10.1", "1.9.0"}, stringsOf(t, out["versions"]))

	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "rolling upgrade")
}

// Community Edition reports no optimistic tolerance, and emitting a bare 0
// there would read as an alarm rather than an absent Enterprise field.
func TestGetAutopilotHealthOmitsEnterpriseOnlyFieldsOnCommunity(t *testing.T) {
	h := newHarness(t)

	out := h.ok("get_autopilot_health", nil)

	require.NotContains(t, out, "optimistic_failure_tolerance")
	require.NotContains(t, out, "redundancy_zones")
	require.NotContains(t, out, "upgrade")
}

// autopilotAccepts makes the fake answer the CAS write, and reports what the
// caller sent. `ok` is what Nomad's compare-and-set returns: false means the
// modify index did not match.
func autopilotAccepts(h *harness, ok bool) {
	body := "false"
	if ok {
		body = "true"
	}
	h.nomad.Handle("PUT /v1/operator/autopilot/configuration",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})
}

// The API replaces the whole document, so an omitted argument must be carried
// through from the current state. Getting this wrong here resets the settings
// that decide who stays in the Raft peer set.
func TestSetAutopilotConfigPreservesSettingsItWasNotAskedToChange(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers:      true,
		LastContactThreshold:    750 * time.Millisecond,
		MaxTrailingLogs:         1000,
		MinQuorum:               3,
		ServerStabilizationTime: 42 * time.Second,
		ModifyIndex:             11,
	})
	autopilotAccepts(h, true)

	out := h.ok("set_autopilot_config", map[string]any{"max_trailing_logs": 500})
	require.Equal(t, true, out["changed"])

	body := h.nomad.Last("/v1/operator/autopilot/configuration").Body
	require.Contains(t, body, `"MaxTrailingLogs":500`)
	require.Contains(t, body, `"LastContactThreshold":"750ms"`,
		"an unmentioned duration must keep its value")
	require.Contains(t, body, `"ServerStabilizationTime":"42s"`)
	require.Contains(t, body, `"MinQuorum":3`)
	require.Contains(t, body, `"CleanupDeadServers":true`)
}

// The compare-and-set index is what stops this call overwriting a change
// another operator made between the read and the write.
func TestSetAutopilotConfigSendsTheModifyIndexAsACAS(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers: true,
		ModifyIndex:        17,
	})
	autopilotAccepts(h, true)

	h.ok("set_autopilot_config", map[string]any{"min_quorum": 5})

	require.Equal(t, "17", h.nomad.Last("/v1/operator/autopilot/configuration").Query.Get("cas"),
		"the write must be conditional on the index it read")
}

func TestSetAutopilotConfigRefusesWhenTheConfigChangedUnderneathIt(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers: true,
		ModifyIndex:        3,
	})
	autopilotAccepts(h, false)

	msg := h.fails("set_autopilot_config", map[string]any{"min_quorum": 5})
	require.Contains(t, msg, "NOT updated")
	require.Contains(t, msg, "changed underneath")
	require.Contains(t, msg, "get_autopilot_config")
}

func TestSetAutopilotConfigReportsANoOp(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers: true,
		MinQuorum:          3,
	})

	out := h.ok("set_autopilot_config", map[string]any{
		"cleanup_dead_servers": true,
		"min_quorum":           3,
	})
	require.Equal(t, false, out["changed"])
	for _, req := range h.nomad.Requests() {
		require.NotEqual(t, http.MethodPut, req.Method,
			"a no-op must not write the configuration back")
	}
}

func TestSetAutopilotConfigParsesDurationsAndRejectsBareNumbers(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		LastContactThreshold:    200 * time.Millisecond,
		ServerStabilizationTime: 10 * time.Second,
	})
	autopilotAccepts(h, true)

	out := h.ok("set_autopilot_config", map[string]any{"last_contact_threshold": "2s"})
	changes, ok := out["changes"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "200ms -> 2s", changes["last_contact_threshold"],
		"the change list should show both sides in the form Nomad uses")
	require.Contains(t, h.nomad.Last("/v1/operator/autopilot/configuration").Body,
		`"LastContactThreshold":"2s"`)

	// A bare number is the mistake a model makes here, and the units it meant
	// are unknowable, so it must be refused rather than guessed at.
	msg := h.fails("set_autopilot_config", map[string]any{"server_stabilization_time": "30"})
	require.Contains(t, msg, "duration string")
	require.Contains(t, msg, "server_stabilization_time")
}

func TestSetAutopilotConfigRejectsThresholdsThatWouldFailEveryServer(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		LastContactThreshold: 200 * time.Millisecond,
		MaxTrailingLogs:      250,
	})

	msg := h.fails("set_autopilot_config", map[string]any{"last_contact_threshold": "0s"})
	require.Contains(t, msg, "greater than zero")
	require.Contains(t, msg, "unhealthy")

	msg = h.fails("set_autopilot_config", map[string]any{"max_trailing_logs": 0})
	require.Contains(t, msg, "greater than zero")

	for _, req := range h.nomad.Requests() {
		require.NotEqual(t, http.MethodPut, req.Method,
			"a rejected value must never reach Nomad")
	}
}

// Turning cleanup on hands Autopilot permission to evict Raft peers, which is
// the consequence the caller most needs told back to them.
func TestSetAutopilotConfigWarnsWhenItEnablesPeerRemoval(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ReadOnly = false })
	h.nomad.JSON("/v1/operator/autopilot/configuration", &api.AutopilotConfiguration{
		CleanupDeadServers: false,
		MinQuorum:          3,
	})
	autopilotAccepts(h, true)

	out := h.ok("set_autopilot_config", map[string]any{"cleanup_dead_servers": true})

	warnings := strings.Join(stringsOf(t, out["warnings"]), " ")
	require.Contains(t, warnings, "cleanup_dead_servers is now ON")
	require.Contains(t, warnings, "Raft peer set")
	require.Contains(t, warnings, "min_quorum = 3",
		"the floor that limits the pruning should be stated alongside it")
}
