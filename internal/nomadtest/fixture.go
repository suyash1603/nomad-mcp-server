// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package nomadtest

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hashicorp/nomad/api"
)

// The fixture is built out of the real api types rather than hand-written JSON.
//
// That is the whole reason it can be trusted. A map literal encodes field names
// by hand, so a typo produces a zero value and a test that passes while
// asserting nothing — and Nomad's JSON names are not always the obvious ones
// (DimensionExhausted, ConstraintFiltered, ClientDescription). Marshalling the
// same structs the client will unmarshal makes that class of mistake
// impossible.

// The default fixture is a small but coherent cluster:
//
//	one server, one healthy client node
//	"web"   — a service job running one healthy allocation
//	"stuck" — a service job with zero allocations and a blocked evaluation
//	          whose placement failure is a constraint nothing satisfies
//
// The two jobs exist so that the same test file can cover the healthy path and
// the placement-failure path, which are the two halves of every troubleshooting
// tool in this server.
func (s *Server) loadDefaults() {
	now := time.Now()
	nano := now.UnixNano()

	s.JSON("/v1/status/leader", "10.0.0.1:4647")
	s.JSON("/v1/regions", []string{"global"})

	s.JSON("/v1/agent/members", &api.ServerMembers{
		ServerName:   "server-1",
		ServerRegion: "global",
		ServerDC:     "dc1",
		Members: []*api.AgentMember{{
			Name: "server-1.global", Addr: "10.0.0.1", Port: 4648, Status: "alive",
			Tags: map[string]string{
				"build": "1.9.0", "dc": "dc1", "region": "global", "role": "nomad",
			},
		}},
	})

	s.JSON("/v1/namespaces", []*api.Namespace{
		{Name: "default", Description: "Default shared namespace"},
	})
	s.JSON("/v1/namespace/default", &api.Namespace{
		Name: "default", Description: "Default shared namespace",
	})
	s.JSON("/v1/node/pools", []*api.NodePool{
		{Name: "default", Description: "Default node pool"},
	})

	// --- nodes -----------------------------------------------------------

	nodeResources := &api.NodeResources{
		Cpu:    api.NodeCpuResources{CpuShares: 8000},
		Memory: api.NodeMemoryResources{MemoryMB: 16000},
		Disk:   api.NodeDiskResources{DiskMB: 200000},
	}

	s.JSON("/v1/nodes", []*api.NodeListStub{{
		ID: NodeID, Name: NodeName, Address: "10.0.0.2",
		Datacenter: "dc1", NodePool: "default", NodeClass: "",
		Status: "ready", SchedulingEligibility: "eligible", Drain: false,
		Version: "1.9.0", NodeResources: nodeResources,
		Drivers: map[string]*api.DriverInfo{
			"docker":   {Detected: true, Healthy: true},
			"exec":     {Detected: true, Healthy: true},
			"raw_exec": {Detected: false, Healthy: false},
		},
	}})

	s.JSON("/v1/node/"+NodeID, &api.Node{
		ID: NodeID, Name: NodeName, Datacenter: "dc1", NodePool: "default",
		Status: "ready", SchedulingEligibility: "eligible", Drain: false,
		NodeResources: nodeResources,
		Attributes: map[string]string{
			"nomad.version":  "1.9.0",
			"kernel.name":    "linux",
			"os.name":        "ubuntu",
			"cpu.numcores":   "8",
			"unique.storage": "irrelevant",
		},
		Drivers: map[string]*api.DriverInfo{
			"docker": {Detected: true, Healthy: true},
			"exec":   {Detected: true, Healthy: true},
		},
		Events: []*api.NodeEvent{{
			Message: "Node registered", Subsystem: "Cluster", Timestamp: now,
		}},
	})

	// --- jobs ------------------------------------------------------------

	webSummary := &api.JobSummary{
		JobID: HealthyJob, Namespace: "default",
		Summary: map[string]api.TaskGroupSummary{
			"app": {Running: 1},
		},
	}
	stuckSummary := &api.JobSummary{
		JobID: StuckJob, Namespace: "default",
		Summary: map[string]api.TaskGroupSummary{
			"impossible": {Queued: 1},
		},
	}

	s.JSON("/v1/jobs", []*api.JobListStub{
		{
			ID: HealthyJob, Name: HealthyJob, Namespace: "default", Type: "service",
			Status: "running", Priority: 50, Datacenters: []string{"dc1"},
			JobSummary: webSummary, SubmitTime: nano,
		},
		{
			ID: StuckJob, Name: StuckJob, Namespace: "default", Type: "service",
			Status: "pending", Priority: 50, Datacenters: []string{"dc1"},
			JobSummary: stuckSummary, SubmitTime: nano,
		},
	})

	s.JSON("/v1/job/"+HealthyJob, job(HealthyJob, "app", "running", nano))
	s.JSON("/v1/job/"+StuckJob, stuckJob(nano))

	s.JSON("/v1/job/"+HealthyJob+"/summary", webSummary)
	s.JSON("/v1/job/"+StuckJob+"/summary", stuckSummary)

	s.JSON("/v1/job/"+HealthyJob+"/allocations", []*api.AllocationListStub{
		allocStub(AllocID, HealthyJob, "app", "running", nano),
	})
	s.JSON("/v1/job/"+StuckJob+"/allocations", []*api.AllocationListStub{})

	s.JSON("/v1/job/"+HealthyJob+"/evaluations", []*api.Evaluation{
		{
			ID: EvalID, JobID: HealthyJob, Namespace: "default", Status: "complete",
			Type: "service", TriggeredBy: "job-register", Priority: 50,
			CreateTime: nano, ModifyTime: nano,
		},
	})
	s.JSON("/v1/job/"+StuckJob+"/evaluations", []*api.Evaluation{blockedEval(nano)})

	s.JSON("/v1/job/"+HealthyJob+"/deployments", []*api.Deployment{deployment(nano)})
	s.JSON("/v1/job/"+StuckJob+"/deployments", []*api.Deployment{})

	s.JSON("/v1/job/"+HealthyJob+"/versions", &api.JobVersionsResponse{
		Versions: []*api.Job{job(HealthyJob, "app", "running", nano)},
	})

	// --- allocations -----------------------------------------------------

	s.JSON("/v1/allocations", []*api.AllocationListStub{
		allocStub(AllocID, HealthyJob, "app", "running", nano),
		allocStub(FailedAlloc, HealthyJob, "app", "failed", nano),
	})
	s.JSON("/v1/allocation/"+AllocID, allocation(AllocID, "running", nano))
	s.JSON("/v1/allocation/"+FailedAlloc, allocation(FailedAlloc, "failed", nano))

	// --- evaluations and deployments -------------------------------------

	s.JSON("/v1/evaluations", []*api.Evaluation{blockedEval(nano)})
	s.JSON("/v1/evaluation/"+BlockedEval, blockedEval(nano))
	s.JSON("/v1/evaluation/"+BlockedEval+"/allocations", []*api.AllocationListStub{})

	s.JSON("/v1/deployments", []*api.Deployment{deployment(nano)})
	s.JSON("/v1/deployment/"+DeploymentID, deployment(nano))
	s.JSON("/v1/deployment/allocations/"+DeploymentID, []*api.AllocationListStub{
		allocStub(AllocID, HealthyJob, "app", "running", nano),
	})

	// --- variables -------------------------------------------------------

	s.JSON("/v1/vars", []*api.VariableMetadata{{
		Namespace: "default", Path: "app/config",
		CreateTime: nano, ModifyTime: nano,
	}})
	s.JSON("/v1/var/app/config", &api.Variable{
		Namespace: "default", Path: "app/config",
		CreateTime: nano, ModifyTime: nano,
		Items: api.VariableItems{
			"db_password": "hunter2-not-a-real-password",
			"api_url":     "https://example.invalid/api",
		},
	})

	// --- services and volumes --------------------------------------------

	s.JSON("/v1/services", []*api.ServiceRegistrationListStub{{
		Namespace: "default",
		Services:  []*api.ServiceRegistrationStub{{ServiceName: "web", Tags: []string{"http"}}},
	}})
	s.JSON("/v1/volumes", []*api.CSIVolumeListStub{})

	// Enterprise-only endpoints answer the way a Community Edition agent does.
	// Several tools have to distinguish "this cluster cannot do that" from
	// "that failed", and this is where that gets exercised.
	for _, path := range []string{"/v1/quotas", "/v1/sentinel/policies", "/v1/recommendations"} {
		s.EnterpriseOnly(path)
	}
}

func job(id, group, status string, nano int64) *api.Job {
	return &api.Job{
		ID: strPtr(id), Name: strPtr(id), Namespace: strPtr("default"),
		Type: strPtr("service"), Status: strPtr(status),
		Priority: intPtr(50), Datacenters: []string{"dc1"},
		SubmitTime: &nano, Version: uint64Ptr(0),
		TaskGroups: []*api.TaskGroup{{
			Name: strPtr(group), Count: intPtr(1),
			Tasks: []*api.Task{{
				Name: "server", Driver: "docker",
				Config: map[string]any{"image": "nginx:stable"},
				Env: map[string]string{
					// Present so a test can prove read_job lists env by key and
					// never by value.
					"DATABASE_PASSWORD": "hunter2-not-a-real-password",
					"LOG_LEVEL":         "info",
				},
				Resources: &api.Resources{CPU: intPtr(500), MemoryMB: intPtr(256)},
			}},
		}},
	}
}

func stuckJob(nano int64) *api.Job {
	j := job(StuckJob, "impossible", "pending", nano)
	j.TaskGroups[0].Constraints = []*api.Constraint{{
		LTarget: "${node.class}", RTarget: "gpu-node-that-does-not-exist", Operand: "=",
	}}
	return j
}

func allocStub(id, jobID, group, clientStatus string, nano int64) *api.AllocationListStub {
	stub := &api.AllocationListStub{
		ID: id, Name: jobID + "." + group + "[0]", Namespace: "default",
		JobID: jobID, JobType: "service", TaskGroup: group,
		NodeID: NodeID, NodeName: NodeName,
		ClientStatus: clientStatus, DesiredStatus: "run",
		EvalID: EvalID, CreateTime: nano, ModifyTime: nano,
		TaskStates: taskStates(clientStatus),
	}
	if clientStatus == "failed" {
		stub.ClientDescription = "Failed tasks"
	}
	return stub
}

func allocation(id, clientStatus string, nano int64) *api.Allocation {
	return &api.Allocation{
		ID: id, Name: "web.app[0]", Namespace: "default",
		JobID: HealthyJob, TaskGroup: "app",
		NodeID: NodeID, NodeName: NodeName,
		ClientStatus: clientStatus, DesiredStatus: "run",
		EvalID: EvalID, CreateTime: nano, ModifyTime: nano,
		TaskStates: taskStates(clientStatus),
		Job:        job(HealthyJob, "app", "running", nano),
		Resources:  &api.Resources{CPU: intPtr(500), MemoryMB: intPtr(256)},
	}
}

func taskStates(clientStatus string) map[string]*api.TaskState {
	if clientStatus == "failed" {
		// The event order matters and is the order Nomad really produces. A
		// task that failed enough times to stop being restarted ends on "Not
		// Restarting", which carries no exit code — the Terminated event that
		// recorded the real one is behind it. A projection that reads only the
		// last event loses the exit code exactly here.
		return map[string]*api.TaskState{"server": {
			State: "dead", Failed: true, Restarts: 3,
			Events: []*api.TaskEvent{
				{
					Type:           "Terminated",
					DisplayMessage: "Exit Code: 1",
					ExitCode:       1,
					Details:        map[string]string{"exit_code": "1", "oom_killed": "false"},
				},
				{
					Type:           "Not Restarting",
					DisplayMessage: `Exceeded allowed attempts 3 in interval 24h0m0s and mode is "fail"`,
				},
			},
		}}
	}
	return map[string]*api.TaskState{"server": {
		State: "running",
		Events: []*api.TaskEvent{{
			Type: "Started", DisplayMessage: "Task started by client",
		}},
	}}
}

// blockedEval is the interesting one: a placement failure with the counters
// Nomad actually sets when a constraint matches no node. projection.Evaluation
// has to turn these into a sentence.
func blockedEval(nano int64) *api.Evaluation {
	return &api.Evaluation{
		ID: BlockedEval, JobID: StuckJob, Namespace: "default",
		Status: "blocked", Type: "service", TriggeredBy: "job-register",
		Priority:   50,
		CreateTime: nano, ModifyTime: nano,
		StatusDescription: "created to place remaining allocations",
		QueuedAllocations: map[string]int{"impossible": 1},
		FailedTGAllocs: map[string]*api.AllocationMetric{
			"impossible": {
				NodesEvaluated: 1,
				NodesFiltered:  1,
				NodesAvailable: map[string]int{"dc1": 1},
				ConstraintFiltered: map[string]int{
					"${node.class} = gpu-node-that-does-not-exist": 1,
				},
				CoalescedFailures: 0,
			},
		},
	}
}

func deployment(nano int64) *api.Deployment {
	return &api.Deployment{
		ID: DeploymentID, JobID: HealthyJob, Namespace: "default",
		Status: "successful", StatusDescription: "Deployment completed successfully",
		JobVersion: 0, CreateTime: nano, ModifyTime: nano,
		TaskGroups: map[string]*api.DeploymentState{
			"app": {DesiredTotal: 1, PlacedAllocs: 1, HealthyAllocs: 1, Promoted: true},
		},
	}
}

// StuckDeployment installs a deployment that is waiting on canary promotion,
// which is the case read_deployment has to diagnose as "waiting on a human".
func (s *Server) StuckDeployment() {
	nano := time.Now().UnixNano()
	d := deployment(nano)
	d.Status = "running"
	d.StatusDescription = "Deployment is running but requires manual promotion"
	d.TaskGroups["app"] = &api.DeploymentState{
		DesiredTotal: 2, DesiredCanaries: 1, PlacedAllocs: 1,
		HealthyAllocs: 1, Promoted: false, PlacedCanaries: []string{AllocID},
	}
	s.JSON("/v1/deployment/"+DeploymentID, d)
	s.JSON("/v1/deployments", []*api.Deployment{d})
}

// Logs installs a fake log stream for an allocation task.
//
// Nomad streams logs as a sequence of JSON StreamFrames rather than plain text,
// so a test that just returns the body would exercise nothing the real client
// does.
func (s *Server) Logs(allocID, task, content string) {
	s.Handle("/v1/client/fs/logs/"+allocID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeFrames(w, content)
	})
}

// writeFrames encodes content the way Nomad's log endpoint does: a stream of
// JSON StreamFrame objects written back to back, with the payload in Data as
// base64. Returning the text directly would skip the decoding the real client
// performs, which is exactly the part worth testing.
func writeFrames(w http.ResponseWriter, content string) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(api.StreamFrame{
		File:   "alloc/logs/server.stderr.0",
		Offset: int64(len(content)),
		Data:   []byte(content),
	})
}

func strPtr(s string) *string    { return &s }
func intPtr(i int) *int          { return &i }
func uint64Ptr(u uint64) *uint64 { return &u }
