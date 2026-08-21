// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package projection holds the trimmed views of Nomad types that more than one
// tool domain needs.
//
// Allocations, evaluations and deployments are each returned by their own
// tools and by the job tools that list a job's children. Keeping one projection
// per type means the same resource looks identical wherever it appears, so a
// model does not have to learn two shapes for the same thing.
package projection

import (
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// AllocStub is the list view of an allocation.
type AllocStub struct {
	ID           string      `json:"id"`
	ShortID      string      `json:"short_id"`
	Name         string      `json:"name,omitempty"`
	JobID        string      `json:"job_id,omitempty"`
	TaskGroup    string      `json:"task_group"`
	Namespace    string      `json:"namespace,omitempty"`
	NodeID       string      `json:"node_id,omitempty"`
	NodeName     string      `json:"node_name,omitempty"`
	ClientStatus string      `json:"client_status"`
	ClientDesc   string      `json:"client_description,omitempty"`
	Desired      string      `json:"desired_status,omitempty"`
	DesiredDesc  string      `json:"desired_description,omitempty"`
	JobVersion   uint64      `json:"job_version"`
	Tasks        []TaskState `json:"tasks,omitempty"`
	Reschedules  int         `json:"reschedule_attempts,omitempty"`
	EvalID       string      `json:"eval_id,omitempty"`
	Created      string      `json:"created,omitempty"`
	Modified     string      `json:"modified,omitempty"`
	Unhealthy    bool        `json:"needs_attention,omitempty"`
}

// TaskState summarises one task inside an allocation.
type TaskState struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Failed     bool   `json:"failed,omitempty"`
	Restarts   uint64 `json:"restarts,omitempty"`
	LastEvent  string `json:"last_event,omitempty"`
	LastReason string `json:"last_reason,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	OOMKilled  bool   `json:"oom_killed,omitempty"`
}

// Alloc projects an allocation list stub.
func Alloc(s *api.AllocationListStub) AllocStub {
	if s == nil {
		return AllocStub{}
	}

	out := AllocStub{
		ID:           s.ID,
		ShortID:      utils.ShortID(s.ID),
		JobID:        s.JobID,
		TaskGroup:    s.TaskGroup,
		Namespace:    s.Namespace,
		NodeID:       s.NodeID,
		NodeName:     s.NodeName,
		ClientStatus: s.ClientStatus,
		ClientDesc:   s.ClientDescription,
		Desired:      s.DesiredStatus,
		DesiredDesc:  s.DesiredDescription,
		JobVersion:   s.JobVersion,
		EvalID:       s.EvalID,
		Created:      utils.FormatTime(s.CreateTime),
		Modified:     utils.FormatTime(s.ModifyTime),
	}

	// Name is "<job>.<group>[n]", which repeats information already present.
	// Keep it only when it is not derivable.
	if !strings.HasPrefix(s.Name, s.JobID+".") {
		out.Name = s.Name
	}

	if s.RescheduleTracker != nil {
		out.Reschedules = len(s.RescheduleTracker.Events)
	}

	for name, ts := range s.TaskStates {
		out.Tasks = append(out.Tasks, taskState(name, ts))
	}
	sortTasks(out.Tasks)

	out.Unhealthy = s.ClientStatus == "failed" || s.ClientStatus == "lost" ||
		s.ClientStatus == "unknown" || anyTaskFailed(out.Tasks)

	return out
}

func taskState(name string, ts *api.TaskState) TaskState {
	out := TaskState{Name: name}
	if ts == nil {
		return out
	}

	out.State = ts.State
	out.Failed = ts.Failed
	out.Restarts = ts.Restarts

	// The last event describes where the task ended up: "Not Restarting",
	// "Terminated", "Killed". That is the right thing to report as its current
	// state.
	if n := len(ts.Events); n > 0 {
		if last := ts.Events[n-1]; last != nil {
			out.LastEvent = last.Type
			out.LastReason = eventReason(last)
		}
	}

	// The exit code, though, is not on the last event. A task that failed often
	// enough for Nomad to give up ends on "Not Restarting — Exceeded allowed
	// attempts", which carries no exit code at all; the "Terminated" event that
	// recorded the real one is further back. Reading only the last event
	// therefore loses the exit code in precisely the case anyone cares about,
	// and omitempty then hides the zero so nothing looks wrong.
	//
	// Found by the e2e suite against the batch-report example, whose "flaky"
	// group exits 1 and is not restarted.
	for i := len(ts.Events) - 1; i >= 0; i-- {
		e := ts.Events[i]
		if e == nil {
			continue
		}
		if e.ExitCode != 0 || e.Type == "Terminated" {
			out.ExitCode = e.ExitCode
			out.OOMKilled = oomKilled(e)
			break
		}
	}

	return out
}

// oomKilled decides whether a terminating event was the out-of-memory killer.
//
// Nomad reports this three different ways depending on the driver, so all three
// are checked: an explicit detail, the 137 that a SIGKILL becomes, and the
// message text for drivers that only say it in prose.
func oomKilled(e *api.TaskEvent) bool {
	if e.Details["oom_killed"] == "true" {
		return true
	}
	if e.ExitCode == 137 {
		return true
	}
	return strings.Contains(strings.ToLower(e.DisplayMessage), "out of memory")
}

// eventReason picks the most informative field of a task event. Nomad populates
// different ones depending on the event type.
func eventReason(e *api.TaskEvent) string {
	for _, candidate := range []string{
		e.DisplayMessage,
		e.Message,
		e.DriverError,
		e.SetupError,
		e.ValidationError,
		e.KillError,
		e.DownloadError,
		e.VaultError,
	} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

func anyTaskFailed(tasks []TaskState) bool {
	for _, t := range tasks {
		if t.Failed {
			return true
		}
	}
	return false
}

func sortTasks(t []TaskState) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j].Name < t[j-1].Name; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

// Eval is the projection of an evaluation.
//
// FailedTGAllocs is the important part and the reason this type exists: when a
// job will not place, the explanation is there and nowhere else. It is rendered
// into readable sentences rather than returned as the raw metric struct.
type Eval struct {
	ID              string             `json:"id"`
	ShortID         string             `json:"short_id"`
	JobID           string             `json:"job_id,omitempty"`
	Namespace       string             `json:"namespace,omitempty"`
	Status          string             `json:"status"`
	StatusDesc      string             `json:"status_description,omitempty"`
	Type            string             `json:"type,omitempty"`
	TriggeredBy     string             `json:"triggered_by,omitempty"`
	Priority        int                `json:"priority,omitempty"`
	DeploymentID    string             `json:"deployment_id,omitempty"`
	NodeID          string             `json:"node_id,omitempty"`
	BlockedEval     string             `json:"blocked_eval_id,omitempty"`
	NextEval        string             `json:"next_eval_id,omitempty"`
	PreviousEval    string             `json:"previous_eval_id,omitempty"`
	QueuedAllocs    map[string]int     `json:"queued_allocations,omitempty"`
	PlacementFailed map[string]Failure `json:"placement_failures,omitempty"`
	QuotaReached    string             `json:"quota_limit_reached,omitempty"`
	Created         string             `json:"created,omitempty"`
	Modified        string             `json:"modified,omitempty"`
	Explanation     string             `json:"explanation,omitempty"`
}

// Failure explains why one task group could not be placed.
type Failure struct {
	NodesEvaluated  int               `json:"nodes_evaluated"`
	NodesAvailable  map[string]int    `json:"nodes_available_by_datacenter,omitempty"`
	NodesFiltered   int               `json:"nodes_filtered,omitempty"`
	ClassFiltered   map[string]int    `json:"filtered_by_node_class,omitempty"`
	ConstraintFail  map[string]int    `json:"filtered_by_constraint,omitempty"`
	DimensionsExh   map[string]int    `json:"resources_exhausted,omitempty"`
	ClassExhausted  map[string]int    `json:"exhausted_by_node_class,omitempty"`
	QuotaExhausted  []string          `json:"quota_exhausted,omitempty"`
	AllocationTime  string            `json:"-"`
	CoalescedFailed string            `json:"coalesced_failures,omitempty"`
	Scores          map[string]string `json:"-"`
	Reason          string            `json:"reason"`
}

// Evaluation projects an evaluation, rendering placement failures into prose.
func Evaluation(e *api.Evaluation) Eval {
	if e == nil {
		return Eval{}
	}

	out := Eval{
		ID:           e.ID,
		ShortID:      utils.ShortID(e.ID),
		JobID:        e.JobID,
		Namespace:    e.Namespace,
		Status:       e.Status,
		StatusDesc:   e.StatusDescription,
		Type:         e.Type,
		TriggeredBy:  e.TriggeredBy,
		Priority:     e.Priority,
		DeploymentID: e.DeploymentID,
		NodeID:       e.NodeID,
		BlockedEval:  e.BlockedEval,
		NextEval:     e.NextEval,
		PreviousEval: e.PreviousEval,
		QuotaReached: e.QuotaLimitReached,
		Created:      utils.FormatTime(e.CreateTime),
		Modified:     utils.FormatTime(e.ModifyTime),
	}

	for group, count := range e.QueuedAllocations {
		if count > 0 {
			if out.QueuedAllocs == nil {
				out.QueuedAllocs = map[string]int{}
			}
			out.QueuedAllocs[group] = count
		}
	}

	var reasons []string
	for group, metric := range e.FailedTGAllocs {
		if metric == nil {
			continue
		}
		f := failure(metric)
		if out.PlacementFailed == nil {
			out.PlacementFailed = map[string]Failure{}
		}
		out.PlacementFailed[group] = f
		reasons = append(reasons, "task group \""+group+"\": "+f.Reason)
	}

	if len(reasons) > 0 {
		sortStrings(reasons)
		out.Explanation = "This evaluation could not place all of its allocations. " +
			strings.Join(reasons, " ") +
			" Placement failures are reported here rather than on the job itself, which is why a job can look healthy while nothing is running."
	}

	return out
}

// failure turns an AllocationMetric into counts plus a readable reason.
func failure(m *api.AllocationMetric) Failure {
	f := Failure{
		NodesEvaluated: m.NodesEvaluated,
		NodesFiltered:  m.NodesFiltered,
		NodesAvailable: m.NodesAvailable,
		ClassFiltered:  m.ClassFiltered,
		ConstraintFail: m.ConstraintFiltered,
		DimensionsExh:  m.DimensionExhausted,
		ClassExhausted: m.ClassExhausted,
		QuotaExhausted: m.QuotaExhausted,
	}

	var parts []string

	if m.NodesEvaluated == 0 {
		parts = append(parts,
			"no nodes were eligible for evaluation at all, which usually means the job's datacenters or node_pool match nothing in this cluster")
	}

	for constraint, count := range m.ConstraintFiltered {
		parts = append(parts, plural(count)+" filtered out by the constraint "+constraint)
	}
	for class, count := range m.ClassFiltered {
		parts = append(parts, plural(count)+" filtered out by node class "+class)
	}
	for dimension, count := range m.DimensionExhausted {
		parts = append(parts, "insufficient "+dimension+" on "+plural(count))
	}
	for class, count := range m.ClassExhausted {
		parts = append(parts, "resources exhausted on "+plural(count)+" in class "+class)
	}
	if len(m.QuotaExhausted) > 0 {
		parts = append(parts, "quota exhausted for "+strings.Join(m.QuotaExhausted, ", "))
	}

	if len(parts) == 0 {
		f.Reason = "no placement was possible, but Nomad reported no specific filter. Check node eligibility with list_nodes."
	} else {
		sortStrings(parts)
		f.Reason = strings.Join(parts, "; ") + "."
	}

	return f
}

// Deploy is the projection of a deployment.
type Deploy struct {
	ID         string                 `json:"id"`
	ShortID    string                 `json:"short_id"`
	JobID      string                 `json:"job_id,omitempty"`
	Namespace  string                 `json:"namespace,omitempty"`
	Status     string                 `json:"status"`
	StatusDesc string                 `json:"status_description,omitempty"`
	JobVersion uint64                 `json:"job_version"`
	Groups     map[string]DeployGroup `json:"task_groups,omitempty"`
	Note       string                 `json:"note,omitempty"`
}

// DeployGroup is one task group's progress within a deployment.
type DeployGroup struct {
	Desired          int    `json:"desired"`
	Placed           int    `json:"placed"`
	Healthy          int    `json:"healthy"`
	Unhealthy        int    `json:"unhealthy"`
	AutoRevert       bool   `json:"auto_revert,omitempty"`
	Promoted         bool   `json:"promoted,omitempty"`
	RequiresPromote  bool   `json:"requires_promotion,omitempty"`
	DesiredCanaries  int    `json:"desired_canaries,omitempty"`
	ProgressDeadline string `json:"progress_deadline,omitempty"`
}

// Deployment projects a deployment.
func Deployment(d *api.Deployment) Deploy {
	if d == nil {
		return Deploy{}
	}

	out := Deploy{
		ID:         d.ID,
		ShortID:    utils.ShortID(d.ID),
		JobID:      d.JobID,
		Namespace:  d.Namespace,
		Status:     d.Status,
		StatusDesc: d.StatusDescription,
		JobVersion: d.JobVersion,
	}

	needsPromotion := false
	for name, g := range d.TaskGroups {
		if g == nil {
			continue
		}
		dg := DeployGroup{
			Desired:         g.DesiredTotal,
			Placed:          g.PlacedAllocs,
			Healthy:         g.HealthyAllocs,
			Unhealthy:       g.UnhealthyAllocs,
			AutoRevert:      g.AutoRevert,
			Promoted:        g.Promoted,
			DesiredCanaries: g.DesiredCanaries,
		}
		if g.DesiredCanaries > 0 && !g.Promoted {
			dg.RequiresPromote = true
			needsPromotion = true
		}
		if !g.RequireProgressBy.IsZero() {
			dg.ProgressDeadline = g.RequireProgressBy.UTC().Format("2006-01-02T15:04:05Z")
		}
		if out.Groups == nil {
			out.Groups = map[string]DeployGroup{}
		}
		out.Groups[name] = dg
	}

	if needsPromotion {
		out.Note = "This deployment is waiting on canary promotion and will not proceed on its own. " +
			"Use promote_deployment once the canaries look healthy."
	}

	return out
}

func plural(n int) string {
	if n == 1 {
		return "1 node"
	}
	return itoa(n) + " nodes"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
