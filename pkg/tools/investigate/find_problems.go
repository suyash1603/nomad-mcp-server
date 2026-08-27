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
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/projection"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

const (
	// problemScanBudget bounds the whole scan. Every check runs concurrently,
	// so this is close to the slowest single check rather than their sum.
	problemScanBudget = 30 * time.Second

	// defaultExamplesPerFinding is how many concrete IDs a finding carries.
	// Enough to act on, not enough to bury the finding itself.
	defaultExamplesPerFinding = 5

	// scanPageSize bounds each underlying list call.
	scanPageSize = 200
)

// finding is one thing that looks wrong.
type finding struct {
	Severity  string   `json:"severity"`
	Category  string   `json:"category"`
	Summary   string   `json:"summary"`
	Count     int      `json:"count"`
	Namespace string   `json:"namespace,omitempty"`
	Examples  []string `json:"examples,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	NextStep  string   `json:"next_step,omitempty"`

	sev severity
}

// problemsResult is the tool's output.
type problemsResult struct {
	Namespace    string              `json:"namespace_scope"`
	Findings     []finding           `json:"findings"`
	Count        int                 `json:"finding_count"`
	ChecksRun    int                 `json:"checks_run"`
	ChecksFailed int                 `json:"checks_failed,omitempty"`
	Errors       []utils.FanOutError `json:"errors,omitempty"`
	Healthy      bool                `json:"looks_healthy"`
	Note         string              `json:"note,omitempty"`
}

// check is one independent thing to look at.
type check struct {
	name string
	run  func(context.Context) ([]finding, error)
}

// FindProblems scans the cluster for everything currently wrong.
func FindProblems(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("find_problems",
			mcp.WithDescription(
				"Scan the cluster for everything that currently looks wrong, and return one ranked "+
					"list: failed and lost allocations, evaluations that could not place work, "+
					"stuck deployments, jobs with work still queued, and nodes that are down, "+
					"draining or ineligible.\n\n"+
					"START HERE for any open-ended question — \"is anything broken?\", \"why is the "+
					"cluster unhealthy?\", \"what happened?\" — and for a health check before or "+
					"after a change. It replaces the sequence of list_jobs, list_allocations, "+
					"list_evaluations, list_deployments and list_nodes that answering those "+
					"questions otherwise takes, and it correlates across them.\n\n"+
					"Each finding carries a severity, a count, example IDs and the specific tool to "+
					"call next. Findings are ranked, so the first one is the most likely answer.\n\n"+
					"Results are capped per finding. The counts are the true totals even when the "+
					"examples are trimmed, so trust count over the length of examples."),
			utils.ReadOnlyTool(),
			utils.NamespaceParam(),
			utils.RegionParam(),
			mcp.WithNumber("max_examples",
				mcp.DefaultNumber(defaultExamplesPerFinding),
				mcp.Description(
					"How many example IDs each finding carries. Defaults to 5. The count field is "+
						"always the true total regardless of this."),
			),
			mcp.WithBoolean("include_nodes",
				mcp.DefaultBool(true),
				mcp.Description(
					"Include client node health. On by default. Node checks are not namespaced, so "+
						"they run whatever namespace scope applies to the rest."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return findProblems(ctx, req, p)
		},
	}
}

func findProblems(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	namespace, err := resolveScanNamespace(ctx, req, p)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	maxExamples := req.GetInt("max_examples", defaultExamplesPerFinding)
	if maxExamples <= 0 {
		maxExamples = defaultExamplesPerFinding
	}

	s := &scanner{
		nomad:       nomad,
		p:           p,
		namespace:   namespace,
		region:      region,
		maxExamples: maxExamples,
	}

	checks := []check{
		{"allocations", s.checkAllocations},
		{"evaluations", s.checkEvaluations},
		{"deployments", s.checkDeployments},
		{"jobs", s.checkJobs},
	}
	if req.GetBool("include_nodes", true) {
		checks = append(checks, check{"nodes", s.checkNodes})
	}

	// The checks are independent reads, so they run concurrently under one
	// budget. A check that fails — a token without node:read, most often —
	// removes its findings but must not cost the caller the others.
	out := utils.FanOut(ctx, checks,
		utils.FanOutLimits{
			Concurrency: len(checks),
			MaxTargets:  len(checks),
			Budget:      problemScanBudget,
		},
		func(ctx context.Context, c check) ([]finding, error) {
			found, err := c.run(ctx)
			if err != nil {
				return nil, fmt.Errorf("the %s check failed: %w", c.name, err)
			}
			return found, nil
		})

	result := problemsResult{
		Namespace:    namespace,
		Findings:     []finding{},
		ChecksRun:    out.Visited,
		ChecksFailed: out.Failed,
		Errors:       out.Errors,
	}
	for _, found := range out.Items {
		result.Findings = append(result.Findings, found...)
	}

	sortFindings(result.Findings)
	result.Count = len(result.Findings)
	result.Healthy = result.Count == 0 && out.Failed == 0
	result.Note = joinNote(out.Note, problemsNote(result, namespace))

	return utils.JSONResult(result)
}

// resolveScanNamespace picks the scope for a cluster-wide scan.
//
// A scan wants every namespace, but "*" is refused outright when an allowlist
// is configured, and returning that error would make the tool useless on
// exactly the locked-down servers it should still work on. So an explicit
// argument wins, an unrestricted server scans everything, and a restricted one
// falls back to its configured default with the scope reported in the result.
func resolveScanNamespace(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (string, error) {
	if requested := strings.TrimSpace(req.GetString("namespace", "")); requested != "" {
		return p.ResolveNamespace(ctx, requested)
	}
	if len(p.Config().AllowedNamespaces) == 0 {
		return "*", nil
	}
	return p.ResolveNamespace(ctx, "")
}

// scanner holds what every check needs.
type scanner struct {
	nomad       *api.Client
	p           *client.Provider
	namespace   string
	region      string
	maxExamples int
}

func (s *scanner) query() *api.QueryOptions {
	return &api.QueryOptions{
		Namespace: s.namespace,
		Region:    s.region,
		PerPage:   scanPageSize,
	}
}

// checkAllocations finds allocations that failed, were lost, or are stuck
// waiting to start.
func (s *scanner) checkAllocations(_ context.Context) ([]finding, error) {
	stubs, _, err := s.nomad.Allocations().List(s.query())
	if err != nil {
		return nil, err
	}

	type bucket struct {
		ids   []string
		jobs  map[string]bool
		nodes map[string]bool
	}
	buckets := map[string]*bucket{}

	add := func(kind string, a *api.AllocationListStub) {
		b := buckets[kind]
		if b == nil {
			b = &bucket{jobs: map[string]bool{}, nodes: map[string]bool{}}
			buckets[kind] = b
		}
		b.ids = append(b.ids, a.ID)
		b.jobs[a.JobID] = true
		if a.NodeName != "" {
			b.nodes[a.NodeName] = true
		}
	}

	for _, a := range stubs {
		if a == nil {
			continue
		}
		switch a.ClientStatus {
		case "failed":
			add("failed", a)
		case "lost":
			add("lost", a)
		case "pending":
			add("pending", a)
		}
	}

	var out []finding

	if b := buckets["failed"]; b != nil {
		f := finding{
			sev:      sevCritical,
			Category: "failed-allocations",
			Count:    len(b.ids),
			Examples: shortIDs(b.ids, s.maxExamples),
			NextStep: "read_allocation on one of these for its task events, then " +
				"search_job_logs on the job to see the error across every replica.",
		}
		f.Summary = fmt.Sprintf("%d allocation%s failed, across %d job%s",
			len(b.ids), plural(len(b.ids)), len(b.jobs), plural(len(b.jobs)))
		// One node carrying every failure is a different problem from failures
		// spread across the cluster, and it is the single most useful thing
		// this scan can notice.
		if len(b.nodes) == 1 && len(b.ids) > 1 {
			f.Detail = "Every one of these is on the same node (" + firstKey(b.nodes) +
				"). That points at the node rather than the jobs — check read_node and get_node_stats."
		}
		out = append(out, f)
	}

	if b := buckets["lost"]; b != nil {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "lost-allocations",
			Count:    len(b.ids),
			Summary: fmt.Sprintf("%d allocation%s lost, which means Nomad lost contact with the client running them",
				len(b.ids), plural(len(b.ids))),
			Examples: shortIDs(b.ids, s.maxExamples),
			Detail: "Lost allocations usually mean a node went away rather than a job failing. " +
				"Check the nodes findings in this same result first.",
			NextStep: "list_nodes with a filter on Status to find down clients.",
		})
	}

	// Pending is only interesting when it persists; a freshly submitted job is
	// briefly pending as a matter of course. Reported at a lower severity for
	// that reason.
	if b := buckets["pending"]; b != nil {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "pending-allocations",
			Count:    len(b.ids),
			Summary: fmt.Sprintf("%d allocation%s still pending — placed on a node but not yet started",
				len(b.ids), plural(len(b.ids))),
			Examples: shortIDs(b.ids, s.maxExamples),
			Detail: "Brief on a job that was just submitted. Persistent pending usually means an " +
				"image pull, a failing template render (a Vault or Consul dependency), or a " +
				"volume that will not mount.",
			NextStep: "read_allocation on one of these — the task events name the stage it is stuck at.",
		})
	}

	return out, nil
}

// checkEvaluations finds work the scheduler could not place.
func (s *scanner) checkEvaluations(_ context.Context) ([]finding, error) {
	evals, _, err := s.nomad.Evaluations().List(s.query())
	if err != nil {
		return nil, err
	}

	var blocked, failed []string
	reasons := map[string]bool{}

	for _, e := range evals {
		if e == nil {
			continue
		}
		if e.Status == "blocked" {
			blocked = append(blocked, e.ID)
		}
		if len(e.FailedTGAllocs) == 0 {
			continue
		}
		failed = append(failed, e.ID)

		// The reason comes from the shared projection rather than being read
		// out of the metric here. Nomad reports a placement failure as a set of
		// counters, and turning those into a sentence is exactly what that
		// layer exists for — including the case this scan first got wrong,
		// where every counter is empty because no node was evaluated at all.
		for _, f := range projection.Evaluation(e).PlacementFailed {
			if f.Reason != "" {
				reasons[f.Reason] = true
			}
		}
	}

	var out []finding

	if len(blocked) > 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "blocked-evaluations",
			Count:    len(blocked),
			Summary: fmt.Sprintf("%d evaluation%s blocked: Nomad wants to place work and cannot",
				len(blocked), plural(len(blocked))),
			Examples: shortIDs(blocked, s.maxExamples),
			Detail: "A blocked evaluation is the scheduler waiting for capacity or for a " +
				"constraint to become satisfiable. Nothing will run until it clears.",
			NextStep: "read_evaluation on one of these — its explanation says exactly which " +
				"constraint or resource filtered every node out.",
		})
	}

	if len(failed) > 0 {
		f := finding{
			sev:      sevCritical,
			Category: "placement-failures",
			Count:    len(failed),
			Summary: fmt.Sprintf("%d evaluation%s recorded placement failures",
				len(failed), plural(len(failed))),
			Examples: shortIDs(failed, s.maxExamples),
			NextStep: "read_evaluation for the full breakdown per task group.",
		}
		if len(reasons) > 0 {
			f.Detail = "Why nothing could be placed: " + strings.Join(sortedKeys(reasons, 3), " | ")
		}
		out = append(out, f)
	}

	return out, nil
}

// checkDeployments finds rollouts that are not progressing.
func (s *scanner) checkDeployments(_ context.Context) ([]finding, error) {
	deps, _, err := s.nomad.Deployments().List(s.query())
	if err != nil {
		return nil, err
	}

	var stuck, awaitingPromotion, failed []string

	for _, d := range deps {
		if d == nil {
			continue
		}
		switch d.Status {
		case "failed":
			failed = append(failed, d.ID)
			continue
		case "running":
		default:
			continue
		}

		var needsPromotion, unhealthy bool
		for _, g := range d.TaskGroups {
			if g == nil {
				continue
			}
			if g.DesiredCanaries > 0 && !g.Promoted {
				needsPromotion = true
			}
			if g.UnhealthyAllocs > 0 {
				unhealthy = true
			}
		}

		switch {
		case needsPromotion:
			awaitingPromotion = append(awaitingPromotion, d.ID)
		case unhealthy:
			stuck = append(stuck, d.ID)
		}
	}

	var out []finding

	if len(failed) > 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "failed-deployments",
			Count:    len(failed),
			Summary:  fmt.Sprintf("%d deployment%s failed", len(failed), plural(len(failed))),
			Examples: shortIDs(failed, s.maxExamples),
			Detail:   "The job is most likely still serving its previous version.",
			NextStep: "read_deployment for which task group failed, then list_job_versions to see what changed.",
		})
	}

	if len(stuck) > 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "stuck-deployments",
			Count:    len(stuck),
			Summary: fmt.Sprintf("%d deployment%s running with unhealthy allocations",
				len(stuck), plural(len(stuck))),
			Examples: shortIDs(stuck, s.maxExamples),
			Detail: "New allocations are being placed but are not becoming healthy, which usually " +
				"means they are failing their health checks rather than failing to start.",
			NextStep: "read_deployment, then search_job_logs on the job for the startup error.",
		})
	}

	if len(awaitingPromotion) > 0 {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "awaiting-promotion",
			Count:    len(awaitingPromotion),
			Summary: fmt.Sprintf("%d deployment%s waiting for a canary to be promoted",
				len(awaitingPromotion), plural(len(awaitingPromotion))),
			Examples: shortIDs(awaitingPromotion, s.maxExamples),
			Detail: "This is not a fault — the rollout is paused by design until someone promotes " +
				"it. It will stay here indefinitely otherwise.",
			NextStep: "read_deployment to check canary health, then promote_deployment when satisfied.",
		})
	}

	return out, nil
}

// checkJobs finds jobs whose desired state is not met.
func (s *scanner) checkJobs(_ context.Context) ([]finding, error) {
	jobs, _, err := s.nomad.Jobs().List(s.query())
	if err != nil {
		return nil, err
	}

	var queued []string
	var totalQueued int

	for _, j := range jobs {
		if j == nil || j.JobSummary == nil || j.Stop {
			continue
		}
		var q int
		for _, tg := range j.JobSummary.Summary {
			q += tg.Queued
		}
		if q > 0 {
			queued = append(queued, j.ID)
			totalQueued += q
		}
	}

	if len(queued) == 0 {
		return nil, nil
	}

	return []finding{{
		sev:      sevCritical,
		Category: "queued-work",
		Count:    len(queued),
		Summary: fmt.Sprintf("%d job%s ha%s %d allocation%s queued and unplaced",
			len(queued), plural(len(queued)), has(len(queued)), totalQueued, plural(totalQueued)),
		Examples: firstN(queued, s.maxExamples),
		Detail: "Queued means the work is wanted but no node was suitable. There is no allocation " +
			"and therefore no logs to read — the reason lives in the evaluation.",
		NextStep: "list_job_evaluations on one of these jobs, then read_evaluation.",
	}}, nil
}

// checkNodes finds clients that cannot take work.
func (s *scanner) checkNodes(_ context.Context) ([]finding, error) {
	nodes, _, err := s.nomad.Nodes().List(&api.QueryOptions{Region: s.region, PerPage: scanPageSize})
	if err != nil {
		return nil, err
	}

	var down, draining, ineligible []string
	var ready int

	for _, n := range nodes {
		if n == nil {
			continue
		}
		switch {
		case n.Status == "down":
			down = append(down, n.Name)
		case n.Drain:
			draining = append(draining, n.Name)
		case n.SchedulingEligibility == "ineligible":
			ineligible = append(ineligible, n.Name)
		default:
			ready++
		}
	}

	var out []finding

	if len(down) > 0 {
		f := finding{
			sev:      sevCritical,
			Category: "nodes-down",
			Count:    len(down),
			Summary:  fmt.Sprintf("%d client node%s down", len(down), plural(len(down))),
			Examples: firstN(down, s.maxExamples),
			NextStep: "read_node for the last heartbeat. Allocations that were on these nodes " +
				"appear as lost.",
		}
		if ready == 0 {
			f.Detail = "No node is ready. Nothing can be scheduled anywhere on this cluster."
		}
		out = append(out, f)
	}

	if len(draining) > 0 {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "nodes-draining",
			Count:    len(draining),
			Summary:  fmt.Sprintf("%d node%s draining", len(draining), plural(len(draining))),
			Examples: firstN(draining, s.maxExamples),
			Detail: "Deliberate if someone is doing maintenance. A drain that never finishes " +
				"usually means the work on it cannot be placed anywhere else.",
			NextStep: "list_node_allocations to see what is still on them.",
		})
	}

	if len(ineligible) > 0 {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "nodes-ineligible",
			Count:    len(ineligible),
			Summary: fmt.Sprintf("%d node%s marked ineligible for scheduling",
				len(ineligible), plural(len(ineligible))),
			Examples: firstN(ineligible, s.maxExamples),
			Detail: "These are up and running their existing work, but will not accept new " +
				"placements. Capacity that looks available is not.",
			NextStep: "set_node_eligibility to bring one back, once you know why it was set.",
		})
	}

	return out, nil
}

// sortFindings ranks by severity, then by count, then by category so the order
// is stable between calls.
func sortFindings(f []finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].sev != f[j].sev {
			return f[i].sev > f[j].sev
		}
		if f[i].Count != f[j].Count {
			return f[i].Count > f[j].Count
		}
		return f[i].Category < f[j].Category
	})
	for i := range f {
		f[i].Severity = f[i].sev.String()
	}
}

// problemsNote summarises the scan in words.
func problemsNote(r problemsResult, namespace string) string {
	scope := "namespace " + namespace
	if namespace == "*" {
		scope = "every namespace"
	}

	if r.ChecksFailed > 0 {
		return fmt.Sprintf(
			"%d of %d checks could not run — see errors. Whatever they would have covered is "+
				"UNKNOWN, not healthy. A missing capability on the token is the usual cause.",
			r.ChecksFailed, r.ChecksRun)
	}

	if r.Count == 0 {
		return "No problems found in " + scope + ". Every allocation is running or complete, " +
			"no evaluation is blocked, no deployment is stuck, and every node is ready. " +
			"Note that this reflects current state only — something that failed and was " +
			"rescheduled successfully will not appear here; build_job_timeline shows history."
	}

	return fmt.Sprintf(
		"%d finding%s in %s, most severe first. Work down from the top: the later findings are "+
			"often consequences of the first one — lost allocations follow a node going down, and "+
			"queued work follows a blocked evaluation.",
		r.Count, plural(r.Count), scope)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func has(n int) string {
	if n == 1 {
		return "s"
	}
	return "ve"
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func firstKey(m map[string]bool) string {
	for k := range m {
		return k
	}
	return ""
}

func sortedKeys(m map[string]bool, max int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return firstN(out, max)
}
