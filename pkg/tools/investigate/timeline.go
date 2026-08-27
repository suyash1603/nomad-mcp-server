// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

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
	// defaultTimelineEvents bounds the returned timeline.
	defaultTimelineEvents = 60
	maxTimelineEvents     = 300
)

// event is one thing that happened, from whichever object recorded it.
type event struct {
	Time    string  `json:"time"`
	Age     string  `json:"age,omitempty"`
	Kind    string  `json:"kind"`
	Summary string  `json:"summary"`
	Detail  string  `json:"detail,omitempty"`
	AllocID string  `json:"alloc_id,omitempty"`
	Task    string  `json:"task,omitempty"`
	Node    string  `json:"node,omitempty"`
	Version *uint64 `json:"job_version,omitempty"`

	at       time.Time
	priority int
}

// timelineResult is the tool's output.
type timelineResult struct {
	JobID     string   `json:"job_id"`
	Namespace string   `json:"namespace"`
	Events    []event  `json:"events"`
	Count     int      `json:"count"`
	Total     int      `json:"total_events"`
	Span      string   `json:"span,omitempty"`
	Sources   []string `json:"sources"`
	Missing   []string `json:"sources_unavailable,omitempty"`
	Note      string   `json:"note,omitempty"`
	Warning   string   `json:"warning"`
}

// BuildJobTimeline merges everything Nomad recorded about a job into one
// ordered narrative.
func BuildJobTimeline(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("build_job_timeline",
			mcp.WithDescription(
				"Reconstruct what happened to a job, in order, by merging its version "+
					"submissions, scheduler evaluations, deployment transitions and per-task "+
					"allocation events into one chronological list.\n\n"+
					"Use this for any question about SEQUENCE or CAUSE — \"what happened?\", \"when "+
					"did this start?\", \"did the deploy cause it?\", \"what changed just before it "+
					"broke?\". Nomad scatters this across four object types with no ordering "+
					"between them, and reading them separately makes it very easy to get the "+
					"causality backwards.\n\n"+
					"This is the natural companion to find_problems: that tool says what is wrong "+
					"NOW, this one says how it got that way. It is also what to reach for when "+
					"search_job_logs finds nothing, because Nomad records these events "+
					"independently of anything the workload writes — they survive log rotation "+
					"and they exist even for an allocation that never started.\n\n"+
					"Events are returned oldest-first by default. Times are UTC."),
			utils.ReadOnlyTool(),
			jobIDParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(defaultTimelineEvents),
				mcp.Description(
					"Maximum events to return. Defaults to 60, maximum 300. When the timeline is "+
						"trimmed the MOST RECENT events are kept, since they are usually the "+
						"relevant ones."),
			),
			mcp.WithBoolean("newest_first",
				mcp.DefaultBool(false),
				mcp.Description(
					"Return newest events first. Off by default: reading a timeline forwards is "+
						"what makes cause and effect legible."),
			),
			mcp.WithString("since",
				mcp.Description(
					"Only include events at or after this RFC3339 time, for example "+
						"\"2026-08-27T10:00:00Z\". Unlike log searching, this filter is exact — "+
						"every event here carries a real timestamp from Nomad."),
			),
			mcp.WithBoolean("all",
				mcp.DefaultBool(false),
				mcp.Description(
					"Include allocations and deployments from older job versions. Off by default. "+
						"Turn it on to see across a change rather than only since it."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return buildTimeline(ctx, req, p)
		},
	}
}

func buildTimeline(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	jobID, namespace, region, nomad, errMsg := resolveJob(ctx, req, p)
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	var since *time.Time
	if raw := strings.TrimSpace(req.GetString("since", "")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return utils.ErrorResultf(
				"The 'since' argument %q is not a valid RFC3339 timestamp. "+
					"Use a form like \"2026-08-27T10:00:00Z\".", raw)
		}
		since = &t
	}

	limit := req.GetInt("limit", defaultTimelineEvents)
	switch {
	case limit <= 0:
		limit = defaultTimelineEvents
	case limit > maxTimelineEvents:
		limit = maxTimelineEvents
	}

	all := req.GetBool("all", false)
	q := &api.QueryOptions{Namespace: namespace, Region: region}

	// The four sources are independent reads. One of them failing — a token
	// that can read allocations but not versions, say — should cost that
	// source's events and nothing else, so each records itself as unavailable
	// rather than failing the call.
	g := &gatherer{jobID: jobID, nomad: nomad, q: q, all: all, redactor: p.Redactor()}

	out := utils.FanOut(ctx, g.sources(),
		utils.FanOutLimits{Concurrency: 4, MaxTargets: 4, Budget: problemScanBudget},
		func(ctx context.Context, s timelineSource) (sourceResult, error) {
			events, err := s.collect(ctx)
			if err != nil {
				// Recorded, not raised: a partial timeline that names its gap
				// is more useful than no timeline.
				return sourceResult{name: s.name, err: err.Error()}, nil
			}
			return sourceResult{name: s.name, events: events}, nil
		})

	result := timelineResult{
		JobID:     jobID,
		Namespace: namespace,
		Events:    []event{},
		Warning:   untrustedNote,
	}

	var events []event
	for _, s := range out.Items {
		if s.err != "" {
			result.Missing = append(result.Missing, s.name+" ("+s.err+")")
			continue
		}
		result.Sources = append(result.Sources, s.name)
		events = append(events, s.events...)
	}
	sort.Strings(result.Sources)
	sort.Strings(result.Missing)

	if since != nil {
		kept := events[:0]
		for _, e := range events {
			if !e.at.Before(*since) {
				kept = append(kept, e)
			}
		}
		events = kept
	}

	sortEvents(events)
	result.Total = len(events)

	// Trim from the front: when a job has more history than fits, the recent
	// end is the part that explains the current state.
	if len(events) > limit {
		events = events[len(events)-limit:]
	}

	for i := range events {
		if !events[i].at.IsZero() {
			events[i].Age = utils.RelativeAge(events[i].at)
		}
	}

	if req.GetBool("newest_first", false) {
		reverse(events)
	}

	result.Events = events
	result.Count = len(events)
	result.Span = describeSpan(events)
	result.Note = joinNote(out.Note, timelineNote(result, limit))

	return utils.JSONResult(result)
}

// sourceResult is one source's contribution, or the reason it has none.
type sourceResult struct {
	name   string
	events []event
	err    string
}

// timelineSource is one object type contributing events.
type timelineSource struct {
	name    string
	collect func(context.Context) ([]event, error)
}

// gatherer collects events from each Nomad object type.
type gatherer struct {
	jobID    string
	nomad    *api.Client
	q        *api.QueryOptions
	all      bool
	redactor *utils.Redactor
}

func (g *gatherer) sources() []timelineSource {
	return []timelineSource{
		{"job versions", g.versions},
		{"evaluations", g.evaluations},
		{"deployments", g.deployments},
		{"allocation events", g.allocEvents},
	}
}

// versions records each time the job specification was submitted.
func (g *gatherer) versions(_ context.Context) ([]event, error) {
	versions, diffs, _, err := g.nomad.Jobs().Versions(g.jobID, true, g.q)
	if err != nil {
		return nil, err
	}

	out := make([]event, 0, len(versions))
	for i, v := range versions {
		if v == nil || v.SubmitTime == nil {
			continue
		}
		e := event{
			at:       time.Unix(0, *v.SubmitTime),
			Kind:     "job-version",
			priority: 0,
			Summary:  fmt.Sprintf("job version %d submitted", derefU64(v.Version)),
			Version:  v.Version,
		}
		// diffs[i] describes the change from versions[i+1] to versions[i],
		// which is what makes "what changed here" answerable inline.
		if i < len(diffs) && diffs[i] != nil {
			if changed := diffFields(diffs[i]); changed != "" {
				e.Detail = "changed: " + changed
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// evaluations records the scheduler's attempts, and why they failed.
func (g *gatherer) evaluations(_ context.Context) ([]event, error) {
	evals, _, err := g.nomad.Jobs().Evaluations(g.jobID, g.q)
	if err != nil {
		return nil, err
	}

	out := make([]event, 0, len(evals))
	for _, e := range evals {
		if e == nil {
			continue
		}
		ev := event{
			at:       time.Unix(0, e.CreateTime),
			Kind:     "evaluation",
			priority: 1,
			Summary:  fmt.Sprintf("evaluation %s (%s), triggered by %s", utils.ShortID(e.ID), e.Status, e.TriggeredBy),
		}
		if e.StatusDescription != "" {
			ev.Detail = e.StatusDescription
		}
		for tg, m := range e.FailedTGAllocs {
			if m == nil {
				continue
			}
			var why []string
			for c := range m.ConstraintFiltered {
				why = append(why, c)
			}
			sort.Strings(why)
			ev.Detail = "could not place task group " + tg
			if len(why) > 0 {
				ev.Detail += "; filtered by " + strings.Join(firstN(why, 3), ", ")
			}
			ev.priority = 1
			break
		}
		out = append(out, ev)
	}
	return out, nil
}

// deployments records rollout starts and how they ended.
func (g *gatherer) deployments(_ context.Context) ([]event, error) {
	deps, _, err := g.nomad.Jobs().Deployments(g.jobID, g.all, g.q)
	if err != nil {
		return nil, err
	}

	out := make([]event, 0, len(deps))
	for _, d := range deps {
		if d == nil {
			continue
		}
		v := d.JobVersion

		out = append(out, event{
			at:       time.Unix(0, d.CreateTime),
			Kind:     "deployment",
			priority: 2,
			Summary:  fmt.Sprintf("deployment %s started for job version %d", utils.ShortID(d.ID), v),
			Version:  &v,
		})

		// A deployment that has finished gets a second entry at the time it
		// finished. Both ends matter on a timeline: the gap between them is
		// how long the rollout took, and a rollout that is still open has no
		// second entry at all, which is itself the answer to "is it stuck?".
		if isTerminalDeployment(d.Status) && d.ModifyTime > d.CreateTime {
			out = append(out, event{
				at:       time.Unix(0, d.ModifyTime),
				Kind:     "deployment",
				priority: 2,
				Summary:  fmt.Sprintf("deployment %s %s", utils.ShortID(d.ID), d.Status),
				Detail:   d.StatusDescription,
				Version:  &v,
			})
		}
	}
	return out, nil
}

// isTerminalDeployment reports whether a deployment has stopped changing.
func isTerminalDeployment(status string) bool {
	switch status {
	case "successful", "failed", "cancelled":
		return true
	}
	return false
}

// allocEvents records every task state change Nomad observed.
func (g *gatherer) allocEvents(_ context.Context) ([]event, error) {
	stubs, _, err := g.nomad.Jobs().Allocations(g.jobID, g.all, g.q)
	if err != nil {
		return nil, err
	}

	var out []event
	for _, a := range stubs {
		if a == nil {
			continue
		}

		out = append(out, event{
			at:       time.Unix(0, a.CreateTime),
			Kind:     "allocation",
			priority: 3,
			Summary:  fmt.Sprintf("allocation %s created on %s", utils.ShortID(a.ID), nodeLabel(a)),
			AllocID:  a.ID,
			Node:     a.NodeName,
			Version:  &a.JobVersion,
		})

		for task, ts := range a.TaskStates {
			if ts == nil {
				continue
			}
			for _, te := range ts.Events {
				if te == nil || te.Time == 0 {
					continue
				}
				out = append(out, event{
					at:       time.Unix(0, te.Time),
					Kind:     "task-event",
					priority: 4,
					Summary:  fmt.Sprintf("%s/%s: %s", utils.ShortID(a.ID), task, te.Type),
					Detail:   g.redactor.String(eventDetail(te)),
					AllocID:  a.ID,
					Task:     task,
					Node:     a.NodeName,
				})
			}
		}
	}
	return out, nil
}

// sortEvents orders the timeline.
//
// Ties break on priority, which keeps a job version ahead of the evaluation it
// triggered, and that ahead of the allocation it placed — the order those
// things actually happen in. Nomad records all four object types to the
// nanosecond but they routinely share a second, so without this the causal
// chain reads in an arbitrary order.
//
// An event whose source reported no timestamp at all sorts to the front and is
// returned with an empty time rather than a fabricated one. Nothing produces
// that today; it is a guard against a future source, or old data, quietly
// getting placed at the epoch.
func sortEvents(e []event) {
	sort.SliceStable(e, func(i, j int) bool {
		if !e[i].at.Equal(e[j].at) {
			return e[i].at.Before(e[j].at)
		}
		return e[i].priority < e[j].priority
	})

	for i := range e {
		if e[i].at.IsZero() {
			e[i].Time = ""
			continue
		}
		e[i].Time = e[i].at.UTC().Format(time.RFC3339)
	}
}

// eventDetail picks the most informative text a task event carries.
func eventDetail(te *api.TaskEvent) string {
	for _, s := range []string{te.DisplayMessage, te.Message, te.DriverError, te.SetupError, te.VaultError, te.KillError} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	if te.ExitCode != 0 {
		return fmt.Sprintf("exit code %d", te.ExitCode)
	}
	return ""
}

// diffFields names the fields a job version changed.
func diffFields(d *api.JobDiff) string {
	var changed []string
	for _, f := range d.Fields {
		if f != nil {
			changed = append(changed, f.Name)
		}
	}
	for _, tg := range d.TaskGroups {
		if tg == nil {
			continue
		}
		for _, t := range tg.Tasks {
			if t == nil {
				continue
			}
			for _, f := range t.Fields {
				if f != nil {
					changed = append(changed, t.Name+"."+f.Name)
				}
			}
		}
	}
	sort.Strings(changed)
	changed = dedupe(changed)
	return strings.Join(firstN(changed, 6), ", ")
}

func timelineNote(r timelineResult, limit int) string {
	var parts []string

	if len(r.Missing) > 0 {
		parts = append(parts,
			"Some sources could not be read ("+strings.Join(r.Missing, "; ")+
				"), so this timeline is incomplete. What they would have contributed is unknown.")
	}
	if r.Total > r.Count {
		parts = append(parts, fmt.Sprintf(
			"%d events exist; the %d most recent are shown. Raise limit or set since to see further back.",
			r.Total, r.Count))
	}
	if r.Total == 0 {
		parts = append(parts,
			"No events at all. Either the job ID is wrong — check list_jobs — or the job was "+
				"only just submitted.")
	}

	return joinNote(parts...)
}

func describeSpan(e []event) string {
	var first, last time.Time
	for _, ev := range e {
		if ev.at.IsZero() {
			continue
		}
		if first.IsZero() || ev.at.Before(first) {
			first = ev.at
		}
		if ev.at.After(last) {
			last = ev.at
		}
	}
	if first.IsZero() {
		return ""
	}
	return first.UTC().Format(time.RFC3339) + " to " + last.UTC().Format(time.RFC3339) +
		" (" + utils.RelativeAge(first) + " to " + utils.RelativeAge(last) + ")"
}

func nodeLabel(a *api.AllocationListStub) string {
	if a.NodeName != "" {
		return a.NodeName
	}
	if a.NodeID != "" {
		return utils.ShortID(a.NodeID)
	}
	return "an unknown node"
}

func derefU64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

func reverse(e []event) {
	for i, j := 0, len(e)-1; i < j; i, j = i+1, j-1 {
		e[i], e[j] = e[j], e[i]
	}
}

func dedupe(s []string) []string {
	if len(s) == 0 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
