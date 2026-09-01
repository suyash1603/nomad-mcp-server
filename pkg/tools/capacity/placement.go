// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package capacity

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// maxRejectedNodesReported caps the per-node detail.
const maxRejectedNodesReported = 10

// placementResult is the tool's output.
type placementResult struct {
	JobID       string           `json:"job_id"`
	Namespace   string           `json:"namespace"`
	Groups      []groupPlacement `json:"task_groups"`
	NodesTotal  int              `json:"nodes_total"`
	Unevaluated []string         `json:"constraints_not_evaluated,omitempty"`
	Note        string           `json:"note"`
}

// groupPlacement is one task group's fit against the cluster.
type groupPlacement struct {
	Name       string         `json:"task_group"`
	Count      int            `json:"count"`
	NeedsCPU   int64          `json:"needs_cpu_mhz"`
	NeedsMem   int64          `json:"needs_memory_mb"`
	Fitting    int            `json:"nodes_that_fit"`
	Capacity   int            `json:"allocations_that_fit"`
	OnePerNode bool           `json:"limited_to_one_per_node,omitempty"`
	Rejected   []rejectedNode `json:"why_the_rest_do_not,omitempty"`
	Reasons    []reasonCount  `json:"rejection_summary,omitempty"`
	Verdict    string         `json:"verdict"`
}

// rejectedNode is one node that cannot take the group, and why.
type rejectedNode struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// reasonCount is how many nodes were rejected for the same reason.
type reasonCount struct {
	Reason string `json:"reason"`
	Nodes  int    `json:"nodes"`
}

// ExplainPlacement works out, node by node, whether a job could be placed.
func ExplainPlacement(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("explain_placement",
			mcp.WithDescription(
				"Work out, node by node, whether an existing job's task groups could be placed — "+
					"and for every node that cannot take them, which specific thing rules it out: "+
					"the datacenter, the node pool, the node's state, or simply not enough free "+
					"CPU or memory.\n\n"+
					"Nomad does not tell you this. A failed evaluation reports aggregate counters "+
					"— \"12 nodes filtered\" — and never which node lacked what. That is fine when "+
					"one constraint excludes everything and useless when a job nearly fits.\n\n"+
					"Reach for this when a job is stuck queued, when read_evaluation says resources "+
					"were exhausted and you need to know by how much, or before scaling a job to "+
					"check the cluster can absorb it.\n\n"+
					"IMPORTANT — this evaluates datacenters, node pools, node state and resource "+
					"fit. It does NOT evaluate the job's constraint blocks, affinities, spread, "+
					"device requirements or host volumes; any constraints present are listed "+
					"unevaluated in the result. A node reported as fitting here may still be "+
					"filtered by one of those, so treat this as \"could it fit by size?\" rather "+
					"than a scheduler verdict. plan_job is the authoritative answer."),
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
			return explainPlacement(ctx, req, p)
		},
	}
}

func explainPlacement(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
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
	job, _, err := nomad.Jobs().Info(jobID, &api.QueryOptions{Namespace: namespace, Region: region})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read job " + jobID,
			Kind:       "job",
			Name:       jobID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "read-job",
			ListTool:   "list_jobs",
		}, p.Redactor()))
	}

	nodes, err := loadCluster(ctx, nomad, p, region)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read cluster capacity",
			Address:    p.Address(),
			Capability: "node:read",
		}, p.Redactor()))
	}

	out := placementResult{
		JobID:      jobID,
		Namespace:  namespace,
		NodesTotal: len(nodes),
	}

	wantGroup := req.GetString("task_group", "")
	unevaluated := map[string]bool{}

	for _, tg := range job.TaskGroups {
		if tg == nil || tg.Name == nil {
			continue
		}
		if wantGroup != "" && *tg.Name != wantGroup {
			continue
		}
		out.Groups = append(out.Groups, evaluateGroup(job, tg, nodes, unevaluated))
	}

	if len(out.Groups) == 0 {
		if wantGroup != "" {
			return utils.ErrorResultf(
				"Job %q has no task group named %q. Use read_job to see its task groups.", jobID, wantGroup)
		}
		return utils.ErrorResultf("Job %q has no task groups.", jobID)
	}

	out.Unevaluated = sortedKeys(unevaluated)
	out.Note = placementNote(out)
	return utils.JSONResult(out)
}

// evaluateGroup checks one task group against every node.
func evaluateGroup(job *api.Job, tg *api.TaskGroup, nodes []nodeCapacity, unevaluated map[string]bool) groupPlacement {
	g := groupPlacement{Name: *tg.Name, Count: 1}
	if tg.Count != nil {
		g.Count = *tg.Count
	}
	g.NeedsCPU, g.NeedsMem = groupNeeds(tg)

	// Datacenters can be set per job; a group inherits them.
	datacenters := job.Datacenters
	pool := ""
	if job.NodePool != nil {
		pool = *job.NodePool
	}

	recordUnevaluated(job, tg, unevaluated)

	// distinct_hosts caps a group at one allocation per node, which changes the
	// arithmetic below completely. It is the one constraint worth evaluating
	// here, because ignoring it would overstate capacity by the node count.
	g.OnePerNode = hasDistinctHosts(job, tg)

	reasons := map[string]int{}
	for _, n := range nodes {
		if reason := rejects(n, datacenters, pool, g.NeedsCPU, g.NeedsMem); reason != "" {
			reasons[reason]++
			if len(g.Rejected) < maxRejectedNodesReported {
				g.Rejected = append(g.Rejected, rejectedNode{Node: n.displayName(), Reason: reason})
			}
			continue
		}
		g.Fitting++
		// Nomad packs several allocations of the same group onto one node
		// unless something forbids it, so the question is how many fit in
		// total, not how many nodes have room for one.
		g.Capacity += allocationsThatFit(n, g.NeedsCPU, g.NeedsMem, g.OnePerNode)
	}

	g.Reasons = rankReasons(reasons)
	g.Verdict = verdict(g)
	return g
}

// groupNeeds totals what one allocation of a task group reserves.
func groupNeeds(tg *api.TaskGroup) (cpu, mem int64) {
	for _, t := range tg.Tasks {
		if t == nil || t.Resources == nil {
			continue
		}
		if t.Resources.CPU != nil {
			cpu += int64(*t.Resources.CPU)
		}
		if t.Resources.MemoryMB != nil {
			mem += int64(*t.Resources.MemoryMB)
		}
	}
	return cpu, mem
}

// rejects returns why a node cannot take the group, or "" if it can.
//
// The order matters: a node that is down should be reported as down rather than
// as short of memory, because the fix is completely different.
func rejects(n nodeCapacity, datacenters []string, pool string, cpu, mem int64) string {
	switch {
	case n.Status == "down":
		return "node is down"
	case n.Draining:
		return "node is draining"
	case !n.Eligible:
		return "node is ineligible for scheduling"
	}

	if len(datacenters) > 0 && !matchesAny(n.Datacenter, datacenters) {
		return "datacenter " + n.Datacenter + " is not in the job's datacenters"
	}
	if pool != "" && pool != "all" && n.Pool != pool {
		return "node pool " + n.Pool + " is not the job's node_pool (" + pool + ")"
	}

	if cpu > 0 && n.cpuFree() < cpu {
		return fmt.Sprintf("insufficient CPU: %d MHz free, needs %d", n.cpuFree(), cpu)
	}
	if mem > 0 && n.memFree() < mem {
		return fmt.Sprintf("insufficient memory: %d MB free, needs %d", n.memFree(), mem)
	}
	return ""
}

// matchesAny supports the "*" wildcard Nomad allows in a datacenter list.
func matchesAny(dc string, patterns []string) bool {
	for _, p := range patterns {
		if p == "*" || p == dc {
			return true
		}
		// Nomad permits a trailing wildcard, as in "eu-west-*".
		if strings.HasSuffix(p, "*") && strings.HasPrefix(dc, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// recordUnevaluated notes every requirement this tool does not check, so the
// caller is never left thinking a clean result means the scheduler will agree.
func recordUnevaluated(job *api.Job, tg *api.TaskGroup, into map[string]bool) {
	for _, c := range job.Constraints {
		into[describeConstraint("job", c)] = true
	}
	for _, c := range tg.Constraints {
		into[describeConstraint("group "+*tg.Name, c)] = true
	}
	for _, t := range tg.Tasks {
		if t == nil {
			continue
		}
		for _, c := range t.Constraints {
			into[describeConstraint("task "+t.Name, c)] = true
		}
		if t.Resources != nil && len(t.Resources.Devices) > 0 {
			into["task "+t.Name+": device requirements"] = true
		}
	}
	if len(tg.Affinities) > 0 || len(job.Affinities) > 0 {
		into["affinities (these influence ranking, not feasibility)"] = true
	}
	if len(tg.Spreads) > 0 || len(job.Spreads) > 0 {
		into["spread blocks"] = true
	}
	if len(tg.Volumes) > 0 {
		into["group volume requirements — use diagnose_volume for those"] = true
	}
}

func describeConstraint(where string, c *api.Constraint) string {
	if c == nil {
		return where + ": constraint"
	}
	op := c.Operand
	if op == "" {
		op = "="
	}
	return fmt.Sprintf("%s: %s %s %s", where, c.LTarget, op, c.RTarget)
}

// rankReasons orders rejection reasons by how many nodes they account for.
func rankReasons(in map[string]int) []reasonCount {
	out := make([]reasonCount, 0, len(in))
	for reason, n := range in {
		out = append(out, reasonCount{Reason: reason, Nodes: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Nodes != out[j].Nodes {
			return out[i].Nodes > out[j].Nodes
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// unboundedFit stands in for "as many as you like" when a group reserves
// neither CPU nor memory. Deliberately finite, so the totals stay meaningful
// numbers rather than overflowing.
const unboundedFit = 1000

// allocationsThatFit is how many allocations of a group one node could hold.
//
// A zero reservation means unbounded on that dimension rather than a division
// by zero: a task with no CPU reservation is limited by memory alone.
func allocationsThatFit(n nodeCapacity, needCPU, needMem int64, onePerNode bool) int {
	if onePerNode {
		return 1
	}

	fit := -1
	if needCPU > 0 {
		fit = int(n.cpuFree() / needCPU)
	}
	if needMem > 0 {
		byMem := int(n.memFree() / needMem)
		if fit < 0 || byMem < fit {
			fit = byMem
		}
	}
	if fit < 0 {
		return unboundedFit
	}
	return fit
}

// hasDistinctHosts reports whether the group is capped at one allocation per
// node by a distinct_hosts constraint.
func hasDistinctHosts(job *api.Job, tg *api.TaskGroup) bool {
	check := func(cs []*api.Constraint) bool {
		for _, c := range cs {
			if c == nil || !strings.EqualFold(c.Operand, "distinct_hosts") {
				continue
			}
			// Nomad treats an empty RTarget on this operand as true.
			if c.RTarget == "" || strings.EqualFold(c.RTarget, "true") {
				return true
			}
		}
		return false
	}
	return check(job.Constraints) || check(tg.Constraints)
}

// verdict states the outcome for one group.
//
// It counts ALLOCATIONS rather than nodes. Nomad packs several allocations of
// the same group onto one node unless distinct_hosts forbids it, so reporting
// "only 1 node can take it, so 1 of 2 will stay queued" is simply wrong on a
// small cluster that is in fact running both.
func verdict(g groupPlacement) string {
	switch {
	case g.Fitting == 0:
		return "NO node can take this task group by size and eligibility alone"

	case g.Capacity < g.Count && g.OnePerNode:
		return fmt.Sprintf(
			"%d node%s can take it, but distinct_hosts allows only one allocation per node and "+
				"count is %d — %d would stay queued",
			g.Fitting, plural(g.Fitting), g.Count, g.Count-g.Capacity)

	case g.Capacity < g.Count:
		return fmt.Sprintf(
			"room for %d allocation%s across %d node%s, but count is %d — %d would stay queued",
			g.Capacity, plural(g.Capacity), g.Fitting, plural(g.Fitting), g.Count, g.Count-g.Capacity)

	default:
		return fmt.Sprintf(
			"room for %d allocation%s across %d node%s, and count is %d",
			g.Capacity, plural(g.Capacity), g.Fitting, plural(g.Fitting), g.Count)
	}
}

// placementNote frames the whole result.
func placementNote(r placementResult) string {
	var parts []string

	blocked := 0
	for _, g := range r.Groups {
		if g.Fitting == 0 {
			blocked++
		}
	}

	switch {
	case blocked > 0:
		parts = append(parts, fmt.Sprintf(
			"%d of %d task group%s cannot be placed on any node. Read rejection_summary: the "+
				"reason accounting for the most nodes is the one to fix first.",
			blocked, len(r.Groups), plural(len(r.Groups))))
	default:
		parts = append(parts,
			"Every task group has at least one node that could take it by size and eligibility.")
	}

	if len(r.Unevaluated) > 0 {
		// This is the caveat that keeps the tool honest. A clean result here is
		// not a scheduler verdict, and a model that forgets that will report a
		// job as placeable when a constraint excludes every node.
		parts = append(parts, fmt.Sprintf(
			"%d requirement%s %s NOT evaluated (see constraints_not_evaluated). A node listed as "+
				"fitting may still be filtered by one of them. Run plan_job for the scheduler's "+
				"own answer.",
			len(r.Unevaluated), plural(len(r.Unevaluated)), wasWere(len(r.Unevaluated))))
	} else {
		parts = append(parts,
			"This job declares no constraints, affinities or volumes, so size and eligibility are "+
				"the whole story and this result should match the scheduler.")
	}

	parts = append(parts,
		"Free capacity here is what is unreserved, not what is unused: a task reserving 2GB and "+
			"using 200MB still occupies 2GB.")

	return joinNote(parts...)
}

// wasWere agrees the verb with the count.
func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
