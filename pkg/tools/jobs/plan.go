// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// aclNote is attached to every tool in this file.
//
// plan_job, validate_job and parse_job_hcl change nothing, so they are
// available in read-only mode. In ACL terms they are not read operations:
// /v1/job/:id/plan requires namespace:submit-job or namespace:plan-job, and
// /v1/jobs/parse requires namespace:parse-job. Verified against
// developer.hashicorp.com/nomad/api-docs/jobs.
//
// A token scoped to read-job alone therefore gets a 403 from these, which is
// confusing enough that the tools say so up front.
const aclNote = "\n\nNote on permissions: although this changes nothing, Nomad classifies it as a " +
	"job-submission operation. A token with only read-job will be refused; it needs submit-job, " +
	"plan-job or parse-job depending on the endpoint."

// parseJobSpec turns the jobspec argument into an *api.Job, accepting HCL2 or
// JSON. HCL is parsed server-side via /v1/jobs/parse, which is the only way to
// get identical semantics to `nomad job run`.
func parseJobSpec(nomad *api.Client, spec string, namespace string) (*api.Job, error) {
	spec = strings.TrimSpace(spec)

	// A leading brace means the caller sent the JSON job format. Nomad's parse
	// endpoint handles HCL only, so JSON goes through the client's own decoder.
	if strings.HasPrefix(spec, "{") {
		var wrapper struct {
			Job *api.Job `json:"Job"`
		}
		if err := decodeJSON(spec, &wrapper); err == nil && wrapper.Job != nil {
			return wrapper.Job, nil
		}
		var job api.Job
		if err := decodeJSON(spec, &job); err != nil {
			return nil, err
		}
		return &job, nil
	}

	job, err := nomad.Jobs().ParseHCL(spec, true)
	if err != nil {
		return nil, err
	}
	if job != nil && namespace != "" && (job.Namespace == nil || *job.Namespace == "") {
		job.Namespace = &namespace
	}
	return job, nil
}

// jobspecParam is the shared jobspec argument.
func jobspecParam() mcp.ToolOption {
	return mcp.WithString("jobspec",
		mcp.Required(),
		mcp.Description(
			"The job specification, as HCL2 (the format used in .nomad.hcl files and by `nomad job run`) "+
				"or as Nomad's JSON job format. HCL is parsed by the Nomad server itself, so the "+
				"semantics match `nomad job run` exactly, including HCL2 functions and variables."),
	)
}

// ParseJobHCL converts an HCL2 jobspec into Nomad's JSON representation.
func ParseJobHCL(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("parse_job_hcl",
			mcp.WithDescription(
				"Parse an HCL2 job specification into Nomad's JSON job structure, using the Nomad "+
					"server's own parser.\n\n"+
					"Use this to check that a jobspec is syntactically valid, or to see how HCL2 "+
					"constructs — variables, functions, heredocs — expand into concrete values before "+
					"anything is submitted. A syntax error comes back here with the parser's message "+
					"and line reference.\n\n"+
					"This only parses. It does not check whether the job would schedule: use "+
					"validate_job for semantic checks and plan_job to see what would actually happen."+
					aclNote),
			utils.ReadOnlyTool(),
			jobspecParam(),
			utils.NamespaceParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spec, err := req.RequireString("jobspec")
			if err != nil {
				return utils.ErrorResult("The 'jobspec' argument is required: pass the HCL2 or JSON job specification.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			job, err := parseJobSpec(nomad, spec, namespace)
			if err != nil {
				return utils.ErrorResult(parseFailure(err, p))
			}

			return utils.JSONResult(map[string]any{
				"parsed":     true,
				"namespace":  namespace,
				"job":        projectJob(job, namespace),
				"note":       "The job parsed successfully. This does not mean it will schedule — run plan_job next to see what Nomad would actually do with it.",
				"json_valid": true,
			})
		},
	}
}

// ValidateJob asks Nomad to validate a jobspec.
func ValidateJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("validate_job",
			mcp.WithDescription(
				"Validate a job specification against the Nomad server: check that it parses, that "+
					"required fields are present, and that its values are legal.\n\n"+
					"This catches structural mistakes — a missing driver, an unknown field, an invalid "+
					"resource value — and returns Nomad's own errors and warnings. Warnings are worth "+
					"reading even when validation passes; they usually flag deprecated syntax.\n\n"+
					"Validation does not consider cluster capacity: a job can validate cleanly and "+
					"still be impossible to place. Use plan_job for that."+aclNote),
			utils.ReadOnlyTool(),
			jobspecParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spec, err := req.RequireString("jobspec")
			if err != nil {
				return utils.ErrorResult("The 'jobspec' argument is required.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			job, err := parseJobSpec(nomad, spec, namespace)
			if err != nil {
				return utils.ErrorResult(parseFailure(err, p))
			}

			resp, _, err := nomad.Jobs().Validate(job, &api.WriteOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "validate the job specification",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "submit-job",
				}, p.Redactor()))
			}

			out := map[string]any{
				"namespace": namespace,
				"valid":     resp.Error == "" && len(resp.ValidationErrors) == 0,
			}
			if resp.Error != "" {
				out["error"] = p.Redactor().String(resp.Error)
			}
			if len(resp.ValidationErrors) > 0 {
				out["validation_errors"] = resp.ValidationErrors
			}
			if resp.Warnings != "" {
				out["warnings"] = resp.Warnings
			}
			if out["valid"] == true {
				out["note"] = "The job is valid. This does not guarantee it can be scheduled — run plan_job to see whether the cluster can actually place it."
			}

			return utils.JSONResult(out)
		},
	}
}

type planResult struct {
	JobID           string            `json:"job_id"`
	Namespace       string            `json:"namespace"`
	Diff            *planDiff         `json:"diff,omitempty"`
	Changes         []string          `json:"changes,omitempty"`
	PlacementIssues map[string]string `json:"placement_problems,omitempty"`
	NextIndex       uint64            `json:"next_index,omitempty"`
	Warnings        string            `json:"warnings,omitempty"`
	WillPlace       map[string]int    `json:"allocations_to_create,omitempty"`
	WillDestroy     map[string]int    `json:"allocations_to_destroy,omitempty"`
	WillUpdate      map[string]int    `json:"allocations_to_update,omitempty"`
	WillIgnore      map[string]int    `json:"allocations_unchanged,omitempty"`
	Safe            bool              `json:"appears_safe"`
	Summary         string            `json:"summary"`
}

type planDiff struct {
	Type       string   `json:"type"`
	FieldsAdd  []string `json:"fields_added,omitempty"`
	FieldsEdit []string `json:"fields_changed,omitempty"`
	FieldsDel  []string `json:"fields_removed,omitempty"`
	Groups     []string `json:"task_group_changes,omitempty"`
}

// PlanJob dry-runs a job submission.
func PlanJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("plan_job",
			mcp.WithDescription(
				"Dry-run a job submission and report exactly what Nomad would do: which allocations "+
					"it would create, destroy, update or leave alone, a diff against the currently "+
					"running version, and any task group it would fail to place.\n\n"+
					"ALWAYS run this before run_job. It is the difference between knowing a change is "+
					"safe and hoping it is. It changes nothing on the cluster, so it is safe to run "+
					"even when the server is in read-only mode, and it is the correct way to answer "+
					"\"what would happen if I deployed this?\".\n\n"+
					"Pay particular attention to placement_problems: a plan that reports failures "+
					"means the job would be accepted and then sit unscheduled, which is the most "+
					"common way a deployment silently does nothing. Also check allocations_to_destroy, "+
					"which tells you whether running work would be replaced."+aclNote),
			utils.ReadOnlyTool(),
			jobspecParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
			mcp.WithBoolean("diff",
				mcp.DefaultBool(true),
				mcp.Description("Include a field-level diff against the currently running job version."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return planJob(ctx, req, p)
		},
	}
}

func planJob(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	spec, err := req.RequireString("jobspec")
	if err != nil {
		return utils.ErrorResult("The 'jobspec' argument is required: pass the HCL2 or JSON job specification you intend to run.")
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	job, err := parseJobSpec(nomad, spec, namespace)
	if err != nil {
		return utils.ErrorResult(parseFailure(err, p))
	}

	resp, _, err := nomad.Jobs().Plan(job, req.GetBool("diff", true), &api.WriteOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "plan the job",
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "submit-job or plan-job",
		}, p.Redactor()))
	}

	out := planResult{
		JobID:     deref(job.ID),
		Namespace: namespace,
		NextIndex: resp.JobModifyIndex,
		Warnings:  resp.Warnings,
		Safe:      true,
	}

	// Per-group counts of what the scheduler intends to do.
	for group, metrics := range resp.Annotations.DesiredTGUpdates {
		if metrics == nil {
			continue
		}
		addCount(&out.WillPlace, group, int(metrics.Place))
		addCount(&out.WillDestroy, group, int(metrics.Stop))
		addCount(&out.WillUpdate, group, int(metrics.InPlaceUpdate+metrics.DestructiveUpdate))
		addCount(&out.WillIgnore, group, int(metrics.Ignore))
	}

	// Placement failures. This is the part people miss.
	for group, metric := range resp.FailedTGAllocs {
		if metric == nil {
			continue
		}
		if out.PlacementIssues == nil {
			out.PlacementIssues = map[string]string{}
		}
		out.PlacementIssues[group] = describeFailure(metric)
		out.Safe = false
	}

	if resp.Diff != nil {
		out.Diff = projectDiff(resp.Diff)
		out.Changes = out.Diff.summarise()
	}

	out.Summary = planSummary(out)

	return utils.JSONResult(out)
}

func planSummary(r planResult) string {
	var b strings.Builder

	if len(r.PlacementIssues) > 0 {
		b.WriteString("This job would NOT schedule cleanly. ")
		b.WriteString("Nomad would accept it, but ")
		b.WriteString(itoa(len(r.PlacementIssues)))
		b.WriteString(" task group(s) could not be placed, so the work would sit queued. ")
		b.WriteString("Fix the placement problems before running it. ")
	} else {
		b.WriteString("This job would schedule. ")
	}

	if destroy := total(r.WillDestroy); destroy > 0 {
		b.WriteString("It would stop ")
		b.WriteString(itoa(destroy))
		b.WriteString(" running allocation(s). ")
	}
	if place := total(r.WillPlace); place > 0 {
		b.WriteString("It would create ")
		b.WriteString(itoa(place))
		b.WriteString(" new allocation(s). ")
	}
	if update := total(r.WillUpdate); update > 0 {
		b.WriteString("It would update ")
		b.WriteString(itoa(update))
		b.WriteString(" allocation(s). ")
	}
	if total(r.WillPlace)+total(r.WillDestroy)+total(r.WillUpdate) == 0 {
		b.WriteString("No allocations would change; the job is already in the desired state. ")
	}

	return strings.TrimSpace(b.String())
}

func projectDiff(d *api.JobDiff) *planDiff {
	out := &planDiff{Type: d.Type}

	for _, f := range d.Fields {
		if f == nil {
			continue
		}
		entry := f.Name + ": " + f.Old + " -> " + f.New
		switch f.Type {
		case "Added":
			out.FieldsAdd = append(out.FieldsAdd, f.Name+" = "+f.New)
		case "Deleted":
			out.FieldsDel = append(out.FieldsDel, f.Name)
		case "Edited":
			out.FieldsEdit = append(out.FieldsEdit, entry)
		}
	}

	for _, tg := range d.TaskGroups {
		if tg == nil || tg.Type == "None" {
			continue
		}
		out.Groups = append(out.Groups, tg.Name+": "+strings.ToLower(tg.Type))
	}

	return out
}

func (d *planDiff) summarise() []string {
	if d == nil {
		return nil
	}
	var out []string
	out = append(out, d.FieldsAdd...)
	out = append(out, d.FieldsEdit...)
	for _, f := range d.FieldsDel {
		out = append(out, "removed "+f)
	}
	out = append(out, d.Groups...)
	return out
}

// describeFailure renders a placement failure as one sentence.
func describeFailure(m *api.AllocationMetric) string {
	var parts []string

	if m.NodesEvaluated == 0 {
		parts = append(parts, "no nodes were eligible at all (check datacenters and node_pool)")
	}
	for constraint, count := range m.ConstraintFiltered {
		parts = append(parts, itoa(count)+" node(s) filtered by constraint "+constraint)
	}
	for dimension, count := range m.DimensionExhausted {
		parts = append(parts, "insufficient "+dimension+" on "+itoa(count)+" node(s)")
	}
	for class, count := range m.ClassFiltered {
		parts = append(parts, itoa(count)+" node(s) filtered by class "+class)
	}
	if len(m.QuotaExhausted) > 0 {
		parts = append(parts, "quota exhausted: "+strings.Join(m.QuotaExhausted, ", "))
	}

	if len(parts) == 0 {
		return "no placement possible; Nomad reported no specific reason"
	}
	sortStrings(parts)
	return strings.Join(parts, "; ")
}

// parseFailure explains a jobspec that would not parse.
func parseFailure(err error, p *client.Provider) string {
	msg := p.Redactor().Error(err)
	if code, body, ok := utils.StatusCode(err); ok && code == 400 {
		return "The job specification could not be parsed by Nomad: " + p.Redactor().String(body) +
			"\n\nCheck the HCL2 syntax. If this is JSON, it must use Nomad's JSON job format, " +
			"which is not the same as the HCL translated key-for-key."
	}
	return "The job specification could not be parsed: " + msg
}

func addCount(m *map[string]int, key string, v int) {
	if v == 0 {
		return
	}
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[key] = v
}

func total(m map[string]int) int {
	sum := 0
	for _, v := range m {
		sum += v
	}
	return sum
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
