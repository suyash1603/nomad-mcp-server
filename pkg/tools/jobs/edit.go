// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// EditJob changes specific fields of a running job without resubmitting a
// whole specification.
//
// This exists because the alternative is worse. Nomad's only update path is
// registering a complete job, so "bump the image tag" otherwise means having
// the model reconstruct the entire jobspec from a read_job projection and
// submit that — and a projection is lossy, so whatever it did not surface gets
// silently dropped from the job. Here the live job object is fetched, the named
// fields are changed on it, and everything else is carried through untouched.
func EditJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("edit_job",
			mcp.WithDescription(
				"Change specific fields of an existing job — image, count, environment variables, "+
					"CPU, memory, priority or metadata — without rewriting its whole specification.\n\n"+
					"Prefer this over run_job for any change to a job that already exists. It "+
					"fetches the live job, changes only what you name, and leaves every other "+
					"field exactly as it was. run_job replaces the job with whatever specification "+
					"you submit, so anything you did not think to include is dropped: a "+
					"reconstructed jobspec loses the fields nobody remembered.\n\n"+
					"This resubmits the job, which for a service job starts a rolling replacement "+
					"of its allocations — the same disruption as any deploy. Use dry_run=true "+
					"first: it runs the change through plan_job and reports exactly what would be "+
					"created, destroyed or replaced, without touching anything.\n\n"+
					"Scope a change with task_group and task. Without them, a change that applies "+
					"per-group or per-task is applied to EVERY group or task in the job, which is "+
					"rarely what is wanted on a job with more than one. Setting count on a job with "+
					"three groups scales all three.\n\n"+
					"env and meta are merged into what is there rather than replacing it: keys you "+
					"do not mention keep their values. Use env_remove to delete one.\n\n"+
					"Confirm the change with the user before running it for real. "+evalHint),
			// Destructive: resubmitting replaces running allocations. Not
			// idempotent: a repeat submission starts another deployment.
			utils.MutatingTool(true, false),
			mcp.WithString("job_id",
				mcp.Required(),
				mcp.Description("The job to change. Use list_jobs to find it."),
			),
			mcp.WithBoolean("dry_run",
				mcp.DefaultBool(false),
				mcp.Description(
					"When true, plan the change and report what it would do without applying it. "+
						"Run this first."),
			),
			mcp.WithString("task_group",
				mcp.Description(
					"Limit group-scoped and task-scoped changes to this task group. Omit to apply "+
						"them to every group in the job."),
			),
			mcp.WithString("task",
				mcp.Description(
					"Limit task-scoped changes to this task. Omit to apply them to every task in "+
						"the selected groups."),
			),
			mcp.WithNumber("count",
				mcp.Description(
					"New allocation count for the selected task groups. Scaling down stops the "+
						"surplus allocations."),
			),
			mcp.WithString("image",
				mcp.Description(
					"New container image for the selected tasks, as in \"nginx:1.27\". Only applies "+
						"to tasks using a driver with an image setting, such as docker or podman."),
			),
			mcp.WithObject("env",
				mcp.Description(
					"Environment variables to set on the selected tasks, as a flat object of "+
						"strings. Merged into the existing environment; keys not mentioned are kept."),
			),
			mcp.WithArray("env_remove",
				mcp.Description("Environment variable names to remove from the selected tasks."),
				mcp.WithStringItems(),
			),
			mcp.WithNumber("cpu",
				mcp.Description("New CPU reservation in MHz for the selected tasks."),
			),
			mcp.WithNumber("memory_mb",
				mcp.Description(
					"New memory reservation in MB for the selected tasks. Lowering this below what "+
						"the task actually uses gets it OOM-killed."),
			),
			mcp.WithNumber("memory_max_mb",
				mcp.Description(
					"New oversubscribed memory ceiling in MB for the selected tasks. Requires "+
						"memory oversubscription to be enabled on the cluster or node pool."),
			),
			mcp.WithNumber("priority",
				mcp.Description(
					"New job priority, 1 to 100. Higher-priority jobs may preempt lower-priority "+
						"ones when preemption is enabled."),
			),
			mcp.WithObject("meta",
				mcp.Description(
					"Job-level metadata to set, as a flat object of strings. Merged into the "+
						"existing metadata."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return editJob(ctx, req, p)
		},
	}
}

func editJob(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return utils.ErrorResult("The 'job_id' argument is required. Use list_jobs to find one.")
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	job, _, err := nomad.Jobs().Info(jobID, &api.QueryOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	})
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
	if job == nil {
		return utils.ErrorResultf(
			"No job named %q exists in namespace %q. Use list_jobs to see what does.",
			jobID, namespace)
	}

	changes, errMsg := applyEdits(job, req)
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}
	if len(changes) == 0 {
		return utils.ErrorResult(
			"Nothing was changed, so the job was not resubmitted.\n\n" +
				"Either no change arguments were given, or the values requested are already what " +
				"the job has, or the task_group and task filters matched nothing in this job. " +
				"read_job shows the job's groups and tasks.")
	}

	// Planning first is not optional in dry-run and is offered in both, because
	// the count of replaced allocations is the number a person actually needs
	// before agreeing to a change.
	if req.GetBool("dry_run", false) {
		return planEdit(ctx, req, p, nomad, job, namespace, changes)
	}

	resp, _, err := nomad.Jobs().Register(job, writeOpts(ctx, req, p, namespace))
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "update job " + jobID,
			Kind:       "job",
			Name:       jobID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "submit-job",
		}, p.Redactor()))
	}

	out := map[string]any{
		"job_id":    jobID,
		"namespace": namespace,
		"changes":   changes,
		"note": "The job was updated with the changes above and everything else left as it was. " +
			evalHint,
	}
	if resp != nil {
		out["eval_id"] = resp.EvalID
		out["job_modify_index"] = resp.JobModifyIndex
	}
	if resp != nil && len(resp.Warnings) > 0 {
		out["nomad_warnings"] = resp.Warnings
	}
	return utils.JSONResult(out)
}

// planEdit runs the edited job through Nomad's planner and reports what it
// would do, without registering it.
func planEdit(ctx context.Context, req mcp.CallToolRequest, p *client.Provider,
	nomad *api.Client, job *api.Job, namespace string, changes []string) (*mcp.CallToolResult, error) {

	plan, _, err := nomad.Jobs().Plan(job, true, writeOpts(ctx, req, p, namespace))
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "plan the change to job " + deref(job.ID),
			Kind:       "job",
			Name:       deref(job.ID),
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "submit-job and plan-job",
		}, p.Redactor()))
	}

	out := map[string]any{
		"job_id":    deref(job.ID),
		"namespace": namespace,
		"dry_run":   true,
		"changes":   changes,
		"note": "NOTHING WAS CHANGED. This is what the edit would do. Run it again with " +
			"dry_run=false to apply it.",
	}

	if plan != nil {
		if plan.Diff != nil {
			out["diff"] = summariseDiff(plan.Diff)
		}
		effects := map[string]int{}
		annotations := map[string]*api.DesiredUpdates{}
		if plan.Annotations != nil {
			annotations = plan.Annotations.DesiredTGUpdates
		}
		for group, updates := range annotations {
			if updates == nil {
				continue
			}
			for kind, n := range map[string]uint64{
				"create":          updates.Place,
				"destroy":         updates.Stop,
				"in_place_update": updates.InPlaceUpdate,
				"replace":         updates.DestructiveUpdate,
				"migrate":         updates.Migrate,
				"unchanged":       updates.Ignore,
			} {
				if n > 0 {
					effects[group+": "+kind] = int(n)
				}
			}
		}
		if len(effects) > 0 {
			out["allocation_effects"] = effects
		}
		if len(plan.FailedTGAllocs) > 0 {
			groups := make([]string, 0, len(plan.FailedTGAllocs))
			for g := range plan.FailedTGAllocs {
				groups = append(groups, g)
			}
			out["would_not_place"] = groups
			out["warning"] = "The planner could not place allocations for " +
				strings.Join(groups, ", ") + ". Applying this edit would leave those groups " +
				"queued rather than running. Use plan_job for the full placement failure detail."
		}
	}
	return utils.JSONResult(out)
}

// summariseDiff reduces Nomad's diff tree to the field-level changes, which is
// the part worth reading. The full tree is large and mostly unchanged fields.
func summariseDiff(diff *api.JobDiff) []string {
	var out []string
	for _, f := range diff.Fields {
		if f != nil && f.Type != "None" {
			out = append(out, "job."+f.Name+": "+f.Old+" -> "+f.New)
		}
	}
	for _, tg := range diff.TaskGroups {
		if tg == nil {
			continue
		}
		for _, f := range tg.Fields {
			if f != nil && f.Type != "None" {
				out = append(out, tg.Name+"."+f.Name+": "+f.Old+" -> "+f.New)
			}
		}
		for _, t := range tg.Tasks {
			if t == nil {
				continue
			}
			for _, f := range t.Fields {
				if f != nil && f.Type != "None" {
					out = append(out, tg.Name+"."+t.Name+"."+f.Name+": "+f.Old+" -> "+f.New)
				}
			}
			for _, o := range t.Objects {
				if o != nil && o.Type != "None" {
					out = append(out, tg.Name+"."+t.Name+"."+o.Name+" changed")
				}
			}
		}
	}
	return out
}

// applyEdits mutates job in place and returns a human-readable list of what it
// changed. An empty list means the request was a no-op.
func applyEdits(job *api.Job, req mcp.CallToolRequest) ([]string, string) {
	args := req.GetArguments()
	var changes []string

	if _, ok := args["priority"]; ok {
		v := req.GetInt("priority", 0)
		if v < 1 || v > 100 {
			return nil, fmt.Sprintf("Invalid priority %d: Nomad requires a value between 1 and 100.", v)
		}
		if job.Priority == nil || *job.Priority != v {
			from := "unset"
			if job.Priority != nil {
				from = itoa(*job.Priority)
			}
			changes = append(changes, "job priority: "+from+" -> "+itoa(v))
			job.Priority = &v
		}
	}

	if meta := utils.StringMap(req, "meta"); len(meta) > 0 {
		if job.Meta == nil {
			job.Meta = map[string]string{}
		}
		for k, v := range meta {
			if job.Meta[k] != v {
				changes = append(changes, "job meta."+k+" set")
				job.Meta[k] = v
			}
		}
	}

	groupFilter := strings.TrimSpace(req.GetString("task_group", ""))
	taskFilter := strings.TrimSpace(req.GetString("task", ""))

	// A filter that matches nothing must be reported rather than silently
	// producing a no-op edit, because the two are indistinguishable to the
	// caller and only one of them is a mistake.
	var groupMatched, taskMatched bool

	for _, tg := range job.TaskGroups {
		if tg == nil {
			continue
		}
		if groupFilter != "" && deref(tg.Name) != groupFilter {
			continue
		}
		groupMatched = true

		if _, ok := args["count"]; ok {
			v := req.GetInt("count", 0)
			if v < 0 {
				return nil, "Invalid count: it cannot be negative. Use 0 to stop every allocation " +
					"in the group while leaving the job registered."
			}
			if tg.Count == nil || *tg.Count != v {
				from := "unset"
				if tg.Count != nil {
					from = itoa(*tg.Count)
				}
				changes = append(changes, deref(tg.Name)+" count: "+from+" -> "+itoa(v))
				count := v
				tg.Count = &count
			}
		}

		for _, task := range tg.Tasks {
			if task == nil {
				continue
			}
			if taskFilter != "" && task.Name != taskFilter {
				continue
			}
			taskMatched = true

			prefix := deref(tg.Name) + "." + task.Name

			if image := strings.TrimSpace(req.GetString("image", "")); image != "" {
				if task.Config == nil {
					task.Config = map[string]any{}
				}
				current, _ := task.Config["image"].(string)
				if current != image {
					// A driver with no image setting would silently accept the
					// key and ignore it, which looks like the edit worked.
					if current == "" && !driverUsesImage(task.Driver) {
						return nil, fmt.Sprintf(
							"Task %s uses the %q driver, which has no image setting, so setting an "+
								"image would have no effect. Use run_job if this task really needs a "+
								"different kind of change.", prefix, task.Driver)
					}
					changes = append(changes, prefix+" image: "+orNone(current)+" -> "+image)
					task.Config["image"] = image
				}
			}

			if env := utils.StringMap(req, "env"); len(env) > 0 {
				if task.Env == nil {
					task.Env = map[string]string{}
				}
				for k, v := range env {
					if task.Env[k] != v {
						// The value is deliberately not echoed: environment
						// variables routinely hold credentials, and this
						// output goes into the model's context.
						changes = append(changes, prefix+" env."+k+" set")
						task.Env[k] = v
					}
				}
			}
			for _, k := range utils.StringSlice(req, "env_remove") {
				if _, present := task.Env[k]; present {
					changes = append(changes, prefix+" env."+k+" removed")
					delete(task.Env, k)
				}
			}

			if msg := applyResourceEdits(task, req, prefix, &changes); msg != "" {
				return nil, msg
			}
		}
	}

	if groupFilter != "" && !groupMatched {
		return nil, fmt.Sprintf(
			"This job has no task group named %q, so nothing was changed. read_job lists its groups.",
			groupFilter)
	}
	if taskFilter != "" && !taskMatched {
		return nil, fmt.Sprintf(
			"No task named %q was found in the selected task groups, so nothing was changed. "+
				"read_job lists the tasks in each group.", taskFilter)
	}

	return changes, ""
}

// applyResourceEdits changes a task's CPU and memory reservations.
func applyResourceEdits(task *api.Task, req mcp.CallToolRequest, prefix string, changes *[]string) string {
	args := req.GetArguments()

	_, hasCPU := args["cpu"]
	_, hasMem := args["memory_mb"]
	_, hasMemMax := args["memory_max_mb"]

	if !hasCPU && !hasMem && !hasMemMax {
		return ""
	}
	if task.Resources == nil {
		task.Resources = &api.Resources{}
	}

	set := func(name string, field **int, value int) string {
		if value <= 0 {
			return fmt.Sprintf(
				"Invalid %s for %s: it must be greater than zero. Nomad rejects a reservation of "+
					"zero, and there is no way to express \"unlimited\" here.", name, prefix)
		}
		if *field == nil || **field != value {
			from := "unset"
			if *field != nil {
				from = itoa(**field)
			}
			*changes = append(*changes, prefix+" "+name+": "+from+" -> "+itoa(value))
			v := value
			*field = &v
		}
		return ""
	}

	if hasCPU {
		if msg := set("cpu", &task.Resources.CPU, req.GetInt("cpu", 0)); msg != "" {
			return msg
		}
	}
	if hasMem {
		if msg := set("memory_mb", &task.Resources.MemoryMB, req.GetInt("memory_mb", 0)); msg != "" {
			return msg
		}
	}
	if hasMemMax {
		if msg := set("memory_max_mb", &task.Resources.MemoryMaxMB, req.GetInt("memory_max_mb", 0)); msg != "" {
			return msg
		}
	}

	// Nomad rejects a memory_max below memory, and the error it gives does not
	// name either value.
	if task.Resources.MemoryMB != nil && task.Resources.MemoryMaxMB != nil &&
		*task.Resources.MemoryMaxMB < *task.Resources.MemoryMB {
		return fmt.Sprintf(
			"For %s, memory_max_mb (%d) would be below memory_mb (%d). The maximum is a ceiling "+
				"for oversubscription, so it has to be at least the reservation.",
			prefix, *task.Resources.MemoryMaxMB, *task.Resources.MemoryMB)
	}
	return ""
}

// driverUsesImage reports whether a task driver has an "image" config setting.
// Setting one on a driver without it is accepted and ignored, so the edit would
// appear to succeed and change nothing.
func driverUsesImage(driver string) bool {
	switch driver {
	case "docker", "podman", "containerd-driver", "ecs":
		return true
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
