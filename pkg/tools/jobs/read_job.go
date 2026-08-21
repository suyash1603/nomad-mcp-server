// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// jobDetail is the projection returned by read_job.
//
// The api.Job struct canonicalizes to several hundred lines of mostly-default
// fields — every unset timeout, every zero-valued update block, every empty
// map. Returning it raw is the single easiest way to burn a model's context on
// nothing. This keeps the fields that describe intent and drops the defaults.
type jobDetail struct {
	ID          string            `json:"id"`
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	Description string            `json:"status_description,omitempty"`
	Priority    int               `json:"priority,omitempty"`
	Region      string            `json:"region,omitempty"`
	Datacenters []string          `json:"datacenters,omitempty"`
	NodePool    string            `json:"node_pool,omitempty"`
	Version     uint64            `json:"version"`
	Stopped     bool              `json:"stopped,omitempty"`
	Periodic    *periodicInfo     `json:"periodic,omitempty"`
	Parameter   *parameterInfo    `json:"parameterized,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
	Constraints []string          `json:"constraints,omitempty"`
	Groups      []groupDetail     `json:"task_groups"`
	Submitted   string            `json:"submitted,omitempty"`
	Note        string            `json:"note,omitempty"`
}

type periodicInfo struct {
	Cron            string `json:"cron,omitempty"`
	TimeZone        string `json:"timezone,omitempty"`
	ProhibitOverlap bool   `json:"prohibit_overlap,omitempty"`
}

type parameterInfo struct {
	Payload      string   `json:"payload,omitempty"`
	MetaRequired []string `json:"meta_required,omitempty"`
	MetaOptional []string `json:"meta_optional,omitempty"`
}

type groupDetail struct {
	Name        string       `json:"name"`
	Count       int          `json:"count"`
	Constraints []string     `json:"constraints,omitempty"`
	Networks    []string     `json:"networks,omitempty"`
	Services    []string     `json:"services,omitempty"`
	Volumes     []string     `json:"volumes,omitempty"`
	RestartMode string       `json:"restart_policy,omitempty"`
	Reschedule  string       `json:"reschedule_policy,omitempty"`
	Tasks       []taskDetail `json:"tasks"`
}

type taskDetail struct {
	Name        string            `json:"name"`
	Driver      string            `json:"driver"`
	Image       string            `json:"image,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	CPU         int               `json:"cpu_mhz,omitempty"`
	Memory      int               `json:"memory_mb,omitempty"`
	MemoryMax   int               `json:"memory_max_mb,omitempty"`
	Env         []string          `json:"env_keys,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
	Constraints []string          `json:"constraints,omitempty"`
	Templates   int               `json:"template_count,omitempty"`
	Leader      bool              `json:"leader,omitempty"`
	Lifecycle   string            `json:"lifecycle,omitempty"`
}

// ReadJob returns one job's definition.
func ReadJob(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_job",
			mcp.WithDescription(
				"Read a single Nomad job's definition: its type, status, task groups, and for each "+
					"task the driver, image or command, resource requests and constraints.\n\n"+
					"Use this to understand how a job is configured — what it runs, how much it asks "+
					"for, where it is allowed to run. Combine it with list_job_evaluations when a job "+
					"is not being placed, since the reason for a placement failure is in the evaluation "+
					"rather than in the job itself.\n\n"+
					"This is a trimmed view: Nomad fills a job with hundreds of defaulted fields, and "+
					"returning them all would be noise. Environment variables are listed by key only, "+
					"never by value, because they routinely carry credentials. If you need the exact "+
					"submitted specification, that is what list_job_versions is for."),
			utils.ReadOnlyTool(),
			mcp.WithString("job_id",
				mcp.Required(),
				mcp.Description("The job's ID, exactly as returned by list_jobs. Not a prefix — use search to resolve a partial ID."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return readJob(ctx, req, p)
		},
	}
}

func readJob(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
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

	return utils.JSONResult(projectJob(job, namespace))
}

// projectJob trims an api.Job down to what is worth reading.
func projectJob(job *api.Job, namespace string) jobDetail {
	out := jobDetail{
		Namespace: namespace,
		Meta:      job.Meta,
	}

	out.ID = deref(job.ID)
	if n := deref(job.Name); n != out.ID {
		out.Name = n
	}
	out.Type = deref(job.Type)
	out.Status = deref(job.Status)
	out.Description = deref(job.StatusDescription)
	out.Region = deref(job.Region)
	out.NodePool = deref(job.NodePool)
	out.Datacenters = job.Datacenters
	out.Constraints = constraintStrings(job.Constraints)

	if job.Priority != nil {
		out.Priority = *job.Priority
	}
	if job.Version != nil {
		out.Version = *job.Version
	}
	if job.Stop != nil {
		out.Stopped = *job.Stop
	}
	if job.Namespace != nil && *job.Namespace != "" {
		out.Namespace = *job.Namespace
	}
	if job.SubmitTime != nil {
		out.Submitted = utils.FormatTime(*job.SubmitTime)
	}

	if job.Periodic != nil {
		out.Periodic = &periodicInfo{
			Cron:     firstCron(job.Periodic),
			TimeZone: deref(job.Periodic.TimeZone),
		}
		if job.Periodic.ProhibitOverlap != nil {
			out.Periodic.ProhibitOverlap = *job.Periodic.ProhibitOverlap
		}
		out.Note = "This is a periodic job: it is a template that Nomad instantiates on a schedule. " +
			"Its own status will not show running allocations; the dispatched child jobs have those. " +
			"Use list_jobs to find children, whose IDs are prefixed with this job's ID."
	}

	if job.ParameterizedJob != nil {
		out.Parameter = &parameterInfo{
			Payload:      job.ParameterizedJob.Payload,
			MetaRequired: job.ParameterizedJob.MetaRequired,
			MetaOptional: job.ParameterizedJob.MetaOptional,
		}
		out.Note = "This is a parameterized job: it does not run on its own and must be dispatched " +
			"with dispatch_parameterized_job to create a child job that actually runs."
	}

	for _, tg := range job.TaskGroups {
		if tg == nil {
			continue
		}
		out.Groups = append(out.Groups, projectGroup(tg))
	}

	return out
}

func projectGroup(tg *api.TaskGroup) groupDetail {
	g := groupDetail{
		Name:        deref(tg.Name),
		Constraints: constraintStrings(tg.Constraints),
	}
	if tg.Count != nil {
		g.Count = *tg.Count
	}
	if tg.RestartPolicy != nil && tg.RestartPolicy.Mode != nil {
		g.RestartMode = *tg.RestartPolicy.Mode
	}
	if tg.ReschedulePolicy != nil && tg.ReschedulePolicy.Unlimited != nil && *tg.ReschedulePolicy.Unlimited {
		g.Reschedule = "unlimited"
	}

	for _, n := range tg.Networks {
		for _, port := range n.DynamicPorts {
			g.Networks = append(g.Networks, "dynamic:"+port.Label)
		}
		for _, port := range n.ReservedPorts {
			g.Networks = append(g.Networks, "static:"+port.Label)
		}
	}
	for _, svc := range tg.Services {
		if svc != nil {
			g.Services = append(g.Services, svc.Name)
		}
	}
	for name := range tg.Volumes {
		g.Volumes = append(g.Volumes, name)
	}

	for _, task := range tg.Tasks {
		if task == nil {
			continue
		}
		g.Tasks = append(g.Tasks, projectTask(task))
	}
	return g
}

func projectTask(task *api.Task) taskDetail {
	t := taskDetail{
		Name:        task.Name,
		Driver:      task.Driver,
		Meta:        task.Meta,
		Constraints: constraintStrings(task.Constraints),
		Templates:   len(task.Templates),
		Leader:      task.Leader,
	}

	if task.Lifecycle != nil {
		t.Lifecycle = task.Lifecycle.Hook
	}
	if task.Resources != nil {
		if task.Resources.CPU != nil {
			t.CPU = *task.Resources.CPU
		}
		if task.Resources.MemoryMB != nil {
			t.Memory = *task.Resources.MemoryMB
		}
		if task.Resources.MemoryMaxMB != nil {
			t.MemoryMax = *task.Resources.MemoryMaxMB
		}
	}

	// Driver config is a free-form map. Surface only the two keys that say
	// what actually runs; the rest is driver-specific noise.
	if v, ok := task.Config["image"].(string); ok {
		t.Image = v
	}
	if v, ok := task.Config["command"].(string); ok {
		t.Command = v
	}
	if raw, ok := task.Config["args"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				t.Args = append(t.Args, s)
			}
		}
	}

	// Keys only. Task environment blocks routinely hold credentials, and a
	// job spec is one of the places people paste them.
	for k := range task.Env {
		t.Env = append(t.Env, k)
	}
	sortStrings(t.Env)

	return t
}

// constraintStrings renders constraints in the form people write them.
func constraintStrings(cs []*api.Constraint) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c == nil {
			continue
		}
		op := c.Operand
		if op == "" {
			op = "="
		}
		out = append(out, c.LTarget+" "+op+" "+c.RTarget)
	}
	return out
}

func firstCron(p *api.PeriodicConfig) string {
	if p == nil {
		return ""
	}
	if len(p.Specs) > 0 {
		return p.Specs[0]
	}
	return deref(p.Spec)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
