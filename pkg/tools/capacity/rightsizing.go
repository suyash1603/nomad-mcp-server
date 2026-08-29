// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

const (
	// sizingConcurrency is modest: each target is a call to a client node
	// rather than to the servers.
	sizingConcurrency = 6

	// maxAllocationsSampled bounds how many allocations are measured.
	maxAllocationsSampled = 40

	// sizingBudget bounds the whole analysis.
	sizingBudget = 30 * time.Second

	// overProvisionedBelow is the fraction of its reservation a task must sit
	// under before it is called over-provisioned.
	overProvisionedBelow = 0.25

	// tightAbove is the fraction above which headroom is called tight.
	tightAbove = 0.90
)

// sizingResult is the tool's output.
type sizingResult struct {
	JobID       string       `json:"job_id"`
	Namespace   string       `json:"namespace"`
	Tasks       []taskSizing `json:"tasks"`
	Sampled     int          `json:"allocations_sampled"`
	Running     int          `json:"allocations_running"`
	OOMKills    int          `json:"oom_kills_observed,omitempty"`
	Unreachable int          `json:"allocations_unreachable,omitempty"`
	Note        string       `json:"note"`
	Caveat      string       `json:"measurement_caveat"`
}

// taskSizing is one task's request against what it was observed using.
type taskSizing struct {
	Group        string `json:"task_group"`
	Task         string `json:"task"`
	Samples      int    `json:"samples"`
	CPURequested int64  `json:"cpu_mhz_requested,omitempty"`
	CPUObserved  int64  `json:"cpu_mhz_observed_max,omitempty"`
	MemRequested int64  `json:"memory_mb_requested,omitempty"`
	MemObserved  int64  `json:"memory_mb_observed_max,omitempty"`
	MemPeak      int64  `json:"memory_mb_peak,omitempty"`
	OOMKills     int    `json:"oom_kills,omitempty"`
	Verdict      string `json:"verdict"`
	Advice       string `json:"advice,omitempty"`
}

// sample is one allocation's measurements.
type sample struct {
	group string
	tasks map[string]taskSample
}

type taskSample struct {
	cpuMhz   int64
	memMB    int64
	peakMB   int64
	measured bool
}

// AnalyzeJobResources compares what a job asked for with what it uses.
func AnalyzeJobResources(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("analyze_job_resources",
			mcp.WithDescription(
				"Compare what each task of a job RESERVED with what it is actually observed using, "+
					"across every running allocation at once, and report where the two disagree.\n\n"+
					"Two questions this answers. \"Are we wasting capacity?\" — a task reserving 2GB "+
					"and using 150MB holds 2GB against the cluster, and get_cluster_capacity counts "+
					"it as full. And \"is this task about to be killed?\" — a task sitting near its "+
					"memory limit, or one with OOM kills in its history, is under-provisioned "+
					"however healthy it looks right now.\n\n"+
					"OOM kills are read from task events, so they are found even for allocations "+
					"that already died and even when current usage looks fine.\n\n"+
					"IMPORTANT — Nomad stores NO usage history. Each measurement here is a single "+
					"instantaneous sample taken when you called this tool, not an average or a "+
					"percentile. It is enough to catch gross over-provisioning and imminent OOM, "+
					"and it is NOT enough to size a spiky workload: a task idle at the moment of "+
					"sampling looks over-provisioned whatever its peaks. For a real distribution "+
					"you need the metrics system scraping Nomad's telemetry.\n\n"+
					"Peak memory is reported only where the driver measures it. Several drivers and "+
					"platforms report no peak at all, and the result says when that is the case "+
					"rather than showing zero."),
			utils.ReadOnlyTool(),
			mcp.WithString("job_id",
				mcp.Required(),
				mcp.Description("The job's ID, exactly as returned by list_jobs."),
			),
			mcp.WithString("task_group",
				mcp.Description("Restrict the analysis to one task group by name."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return analyzeResources(ctx, req, p)
		},
	}
}

func analyzeResources(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return utils.ErrorResult("The 'job_id' argument is required. Use list_jobs to see what exists.")
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	q := &api.QueryOptions{Namespace: namespace, Region: region}

	job, _, err := nomad.Jobs().Info(jobID, q)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op: "read job " + jobID, Kind: "job", Name: jobID, Namespace: namespace,
			Address: p.Address(), Capability: "read-job", ListTool: "list_jobs",
		}, p.Redactor()))
	}

	// allAllocs=true so OOM kills on allocations that already died are still
	// found. A task that was OOM-killed and rescheduled looks perfectly healthy
	// in its current allocation, which is exactly when this is worth knowing.
	stubs, _, err := nomad.Jobs().Allocations(jobID, true, q)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op: "list allocations for job " + jobID, Kind: "job", Name: jobID,
			Namespace: namespace, Address: p.Address(), Capability: "read-job",
		}, p.Redactor()))
	}

	wantGroup := req.GetString("task_group", "")
	requested := requestedResources(job, wantGroup)
	oom := countOOMKills(stubs, wantGroup)

	var running []*api.AllocationListStub
	for _, a := range stubs {
		if a != nil && a.ClientStatus == "running" && (wantGroup == "" || a.TaskGroup == wantGroup) {
			running = append(running, a)
		}
	}

	out := sizingResult{
		JobID:     jobID,
		Namespace: namespace,
		Running:   len(running),
		Tasks:     []taskSizing{},
	}

	measured := utils.FanOut(ctx, running,
		utils.FanOutLimits{
			Concurrency: sizingConcurrency,
			MaxTargets:  maxAllocationsSampled,
			Budget:      sizingBudget,
		},
		func(ctx context.Context, a *api.AllocationListStub) (sample, error) {
			return sampleAlloc(ctx, nomad, a, namespace, region)
		})

	out.Sampled = len(measured.Items)
	out.Unreachable = measured.Failed

	out.Tasks = combine(requested, measured.Items, oom)
	for _, t := range out.Tasks {
		out.OOMKills += t.OOMKills
	}

	sort.Slice(out.Tasks, func(i, j int) bool {
		if out.Tasks[i].Group != out.Tasks[j].Group {
			return out.Tasks[i].Group < out.Tasks[j].Group
		}
		return out.Tasks[i].Task < out.Tasks[j].Task
	})

	out.Caveat = "Every observed figure is a single instantaneous sample taken during this call. " +
		"Nomad keeps no usage history, so these are not averages or percentiles. A task that " +
		"happened to be idle when sampled will look over-provisioned regardless of its peaks."
	out.Note = joinNote(measured.Note, sizingNote(out))

	return utils.JSONResult(out)
}

// requestedKey identifies one task within a job.
type requestedKey struct{ group, task string }

// requestedResources reads what the job specification asks for.
func requestedResources(job *api.Job, wantGroup string) map[requestedKey]taskSample {
	out := map[requestedKey]taskSample{}
	for _, tg := range job.TaskGroups {
		if tg == nil || tg.Name == nil {
			continue
		}
		if wantGroup != "" && *tg.Name != wantGroup {
			continue
		}
		for _, t := range tg.Tasks {
			if t == nil || t.Resources == nil {
				continue
			}
			var s taskSample
			if t.Resources.CPU != nil {
				s.cpuMhz = int64(*t.Resources.CPU)
			}
			if t.Resources.MemoryMB != nil {
				s.memMB = int64(*t.Resources.MemoryMB)
			}
			out[requestedKey{*tg.Name, t.Name}] = s
		}
	}
	return out
}

// sampleAlloc reads one allocation's live statistics.
func sampleAlloc(ctx context.Context, nomad *api.Client, a *api.AllocationListStub, namespace, region string) (sample, error) {
	alloc, _, err := nomad.Allocations().Info(a.ID, &api.QueryOptions{Namespace: namespace, Region: region})
	if err != nil {
		return sample{}, fmt.Errorf("reading allocation %s: %w", utils.ShortID(a.ID), err)
	}

	stats, err := nomad.Allocations().Stats(alloc, &api.QueryOptions{Namespace: namespace, Region: region})
	if err != nil {
		return sample{}, fmt.Errorf("reading stats for %s: %w", utils.ShortID(a.ID), err)
	}

	s := sample{group: a.TaskGroup, tasks: map[string]taskSample{}}
	for name, t := range stats.Tasks {
		if t == nil || t.ResourceUsage == nil {
			continue
		}
		var ts taskSample
		if c := t.ResourceUsage.CpuStats; c != nil {
			ts.cpuMhz = int64(c.TotalTicks)
		}
		if m := t.ResourceUsage.MemoryStats; m != nil {
			ts.memMB, ts.measured = memoryMB(m)
			ts.peakMB = int64(m.MaxUsage / 1024 / 1024)
		}
		s.tasks[name] = ts
	}
	return s, nil
}

// memoryMB picks the most meaningful memory figure the driver actually
// measured.
//
// Which fields a driver populates varies by platform: cgroups report Usage and
// MaxUsage, while several drivers report only RSS and leave the rest at zero.
// Reading Usage unconditionally would report every task on those drivers as
// using no memory at all, which is worse than reporting nothing.
func memoryMB(m *api.MemoryStats) (mb int64, measured bool) {
	has := func(field string) bool {
		for _, s := range m.Measured {
			if strings.EqualFold(s, field) {
				return true
			}
		}
		return false
	}

	switch {
	case m.Usage > 0 && (has("Usage") || len(m.Measured) == 0):
		return int64(m.Usage / 1024 / 1024), true
	case m.RSS > 0:
		return int64(m.RSS / 1024 / 1024), true
	case m.Usage > 0:
		return int64(m.Usage / 1024 / 1024), true
	}
	return 0, false
}

// countOOMKills scans task events for the kernel's out-of-memory kill.
func countOOMKills(stubs []*api.AllocationListStub, wantGroup string) map[requestedKey]int {
	out := map[requestedKey]int{}
	for _, a := range stubs {
		if a == nil || (wantGroup != "" && a.TaskGroup != wantGroup) {
			continue
		}
		for task, ts := range a.TaskStates {
			if ts == nil {
				continue
			}
			for _, e := range ts.Events {
				if e != nil && isOOM(e) {
					out[requestedKey{a.TaskGroup, task}]++
				}
			}
		}
	}
	return out
}

// isOOM reports whether an event is an out-of-memory kill.
func isOOM(e *api.TaskEvent) bool {
	if strings.Contains(strings.ToLower(e.DisplayMessage), "out of memory") {
		return true
	}
	if v, ok := e.Details["oom_killed"]; ok && v == "true" {
		return true
	}
	// Exit code 137 is SIGKILL, which is what the kernel's OOM killer sends.
	return e.ExitCode == 137
}

// combine folds the requests, the samples and the OOM history together.
func combine(requested map[requestedKey]taskSample, samples []sample, oom map[requestedKey]int) []taskSizing {
	type agg struct {
		count          int
		maxCPU, maxMem int64
		maxPeak        int64
		anyMeasured    bool
	}
	observed := map[requestedKey]*agg{}

	for _, s := range samples {
		for task, ts := range s.tasks {
			k := requestedKey{s.group, task}
			a := observed[k]
			if a == nil {
				a = &agg{}
				observed[k] = a
			}
			a.count++
			if ts.cpuMhz > a.maxCPU {
				a.maxCPU = ts.cpuMhz
			}
			if ts.memMB > a.maxMem {
				a.maxMem = ts.memMB
			}
			if ts.peakMB > a.maxPeak {
				a.maxPeak = ts.peakMB
			}
			if ts.measured {
				a.anyMeasured = true
			}
		}
	}

	// Every task in the specification appears, even one with no live
	// allocation: "this task has no running allocation to measure" is itself
	// worth saying, and silently omitting it would read as nothing to report.
	keys := map[requestedKey]bool{}
	for k := range requested {
		keys[k] = true
	}
	for k := range observed {
		keys[k] = true
	}
	for k := range oom {
		keys[k] = true
	}

	out := make([]taskSizing, 0, len(keys))
	for k := range keys {
		t := taskSizing{Group: k.group, Task: k.task, OOMKills: oom[k]}
		if r, ok := requested[k]; ok {
			t.CPURequested = r.cpuMhz
			t.MemRequested = r.memMB
		}
		if a := observed[k]; a != nil {
			t.Samples = a.count
			t.CPUObserved = a.maxCPU
			t.MemPeak = a.maxPeak
			if a.anyMeasured {
				t.MemObserved = a.maxMem
			}
		}
		t.Verdict, t.Advice = judge(t)
		out = append(out, t)
	}
	return out
}

// judge turns one task's numbers into a verdict.
//
// OOM history outranks everything: a task that was killed for memory is
// under-provisioned however comfortable its current sample looks, and that is
// precisely the case a single sample would otherwise miss.
func judge(t taskSizing) (verdict, advice string) {
	switch {
	case t.OOMKills > 0:
		return "under-provisioned", fmt.Sprintf(
			"%d out-of-memory kill%s recorded. The memory limit of %d MB is too low for this "+
				"task's peaks whatever the current sample shows. Raise it before trusting any "+
				"other number here.", t.OOMKills, plural(t.OOMKills), t.MemRequested)

	case t.Samples == 0:
		return "not measured", "No running allocation to sample. If this job should be running, " +
			"find_problems will say why it is not."

	case t.MemRequested > 0 && t.MemObserved == 0:
		return "not measured", "This task's driver did not report memory usage, so only its " +
			"reservation is known. Peak memory is unavailable on several drivers and platforms."

	case t.MemRequested > 0 && float64(t.MemObserved) > float64(t.MemRequested)*tightAbove:
		return "tight", fmt.Sprintf(
			"using %d MB of a %d MB limit at the moment of sampling. A task this close to its "+
				"limit is a candidate for an out-of-memory kill on any spike.",
			t.MemObserved, t.MemRequested)

	case t.MemRequested > 0 && float64(t.MemObserved) < float64(t.MemRequested)*overProvisionedBelow:
		return "possibly over-provisioned", fmt.Sprintf(
			"reserving %d MB and observed at %d MB. That reservation is held against the cluster "+
				"in full. Confirm against a real distribution before cutting it: a single sample "+
				"cannot see a spiky workload's peaks.",
			t.MemRequested, t.MemObserved)
	}

	return "reasonable", ""
}

// sizingNote frames the whole result.
func sizingNote(r sizingResult) string {
	var parts []string

	if r.Running == 0 {
		return "This job has no running allocations, so nothing could be measured. Any OOM kills " +
			"below come from task events on allocations that have since stopped."
	}

	if r.OOMKills > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d out-of-memory kill%s found in this job's task events. Those are facts from history, "+
				"not samples, and they outrank every other reading here.", r.OOMKills, plural(r.OOMKills)))
	}

	var over, tight int
	for _, t := range r.Tasks {
		switch t.Verdict {
		case "possibly over-provisioned":
			over++
		case "tight":
			tight++
		}
	}
	if tight > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d task%s sitting close to its memory limit.", tight, plural(tight)))
	}
	if over > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d task%s may be over-provisioned. Their reservations are counted in full by "+
				"get_cluster_capacity, so trimming them is what actually returns capacity.",
			over, plural(over)))
	}
	if len(parts) == 0 {
		parts = append(parts, "Nothing looks obviously mis-sized in this sample.")
	}

	return joinNote(parts...)
}
