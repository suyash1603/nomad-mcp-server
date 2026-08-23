// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package enterprise

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// quotaSummary is the list projection: enough to see what exists and roughly
// what it caps, without the full per-region limit structure.
type quotaSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Regions     []string `json:"regions,omitempty"`
	Limits      []string `json:"limits,omitempty"`
}

// quotaDetail pairs a quota's limits with its current usage, because the two
// are only useful together: a limit with no usage figure does not tell you
// whether a job was rejected for exceeding it.
type quotaDetail struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Regions     []regionQuota `json:"regions"`
	Namespaces  []string      `json:"namespaces_attached,omitempty"`
	Warnings    []string      `json:"warnings,omitempty"`
	Note        string        `json:"note,omitempty"`
}

// regionQuota is one region's limit and usage side by side.
type regionQuota struct {
	Region string `json:"region"`

	CPULimit       *int `json:"cpu_limit_mhz,omitempty"`
	CPUUsed        *int `json:"cpu_used_mhz,omitempty"`
	CoresLimit     *int `json:"cores_limit,omitempty"`
	CoresUsed      *int `json:"cores_used,omitempty"`
	MemoryLimit    *int `json:"memory_limit_mb,omitempty"`
	MemoryUsed     *int `json:"memory_used_mb,omitempty"`
	MemoryMaxLimit *int `json:"memory_max_limit_mb,omitempty"`

	VariablesLimitMB   *int `json:"variables_limit_mb,omitempty"`
	HostVolumesLimitMB *int `json:"host_volumes_limit_mb,omitempty"`

	PercentUsed map[string]int `json:"percent_used,omitempty"`
}

// ListQuotas lists the resource quotas defined in the cluster.
func ListQuotas(p *client.Provider) server.ServerTool {
	tool := []mcp.ToolOption{
		mcp.WithDescription(
			"List the resource quotas defined in the cluster.\n\n" +
				"A quota caps the total CPU and memory that all jobs in the namespaces attached to " +
				"it may reserve. When a job is rejected at submission with a quota error, or a " +
				"namespace mysteriously stops accepting new work, this is where to look.\n\n" +
				"Quotas apply to what the scheduler has RESERVED, not to what workloads are " +
				"actually using — a namespace can exhaust its quota while its tasks sit idle. " +
				"Use read_quota for one quota's limits alongside its current usage.\n\n" +
				"Requires Nomad Enterprise: get_cluster_status reports which edition this cluster runs."),
		utils.ReadOnlyTool(),
		utils.EnterpriseTool(),
		utils.RegionParam(),
		utils.PrefixParam("quotas"),
	}
	tool = append(tool, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_quotas", tool...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			page := utils.PageFrom(req)
			q := page.Apply(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix: req.GetString("prefix", ""),
			})

			specs, meta, err := nomad.Quotas().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list quotas",
					Kind:       "quota",
					Address:    p.Address(),
					Capability: "quota:read",
				}, p.Redactor()))
			}

			items := make([]quotaSummary, 0, len(specs))
			for _, spec := range specs {
				if spec == nil {
					continue
				}
				s := quotaSummary{Name: spec.Name, Description: spec.Description}
				for _, limit := range spec.Limits {
					if limit == nil {
						continue
					}
					s.Regions = append(s.Regions, limit.Region)
					if d := describeLimit(limit); d != "" {
						s.Limits = append(s.Limits, limit.Region+": "+d)
					}
				}
				items = append(items, s)
			}

			result := utils.List{Count: len(items), Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if len(items) == 0 {
				result.Note = "No quotas are defined. Nothing in this cluster is capped by quota, " +
					"so a rejected job has some other cause."
			}
			return utils.JSONResult(result)
		},
	}
}

// describeLimit renders one region limit as a short phrase for the list view.
func describeLimit(l *api.QuotaLimit) string {
	if l.RegionLimit == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if v := l.RegionLimit.CPU; v != nil && *v != 0 {
		parts = append(parts, itoa(*v)+" MHz CPU")
	}
	if v := l.RegionLimit.Cores; v != nil && *v != 0 {
		parts = append(parts, itoa(*v)+" cores")
	}
	if v := l.RegionLimit.MemoryMB; v != nil && *v != 0 {
		parts = append(parts, itoa(*v)+" MB memory")
	}
	return strings.Join(parts, ", ")
}

// ReadQuota reads one quota's limits together with its usage.
func ReadQuota(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_quota",
			mcp.WithDescription(
				"Read one resource quota: its limit in each region, how much of that limit is "+
					"currently consumed, and which namespaces it is attached to.\n\n"+
					"This is the tool for \"why was my job rejected\" when the rejection mentions a "+
					"quota. The percent_used figures say immediately whether the quota is the "+
					"binding constraint.\n\n"+
					"Usage counts what the scheduler has RESERVED for running and pending "+
					"allocations, not what tasks are actually consuming. Stopping a job frees its "+
					"reservation; a task using less memory than it asked for does not.\n\n"+
					"A limit of zero means unlimited for that resource, and a negative limit means "+
					"the resource is disallowed entirely. Requires Nomad Enterprise."),
			utils.ReadOnlyTool(),
			utils.EnterpriseTool(),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The quota's name. Use list_quotas to see what exists."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult(
					"The 'name' argument is required. Use list_quotas to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}

			spec, _, err := nomad.Quotas().Info(name, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read quota " + name,
					Kind:       "quota",
					Name:       name,
					Address:    p.Address(),
					Capability: "quota:read",
					ListTool:   "list_quotas",
				}, p.Redactor()))
			}
			if spec == nil {
				return utils.ErrorResultf(
					"No quota named %q exists. Use list_quotas to see what does.", name)
			}

			out := quotaDetail{Name: spec.Name, Description: spec.Description}

			// Usage is a separate call and a separate capability. Without it
			// the limits are still worth returning, so its failure is a warning.
			var used map[string]*api.QuotaLimit
			if usage, _, err := nomad.Quotas().Usage(name, q); err == nil && usage != nil {
				used = usage.Used
			} else if err != nil {
				out.Warnings = append(out.Warnings,
					"Could not read this quota's current usage, so only the limits are shown: "+
						utils.MapError(err, utils.ErrorContext{
							Op:         "read quota usage",
							Address:    p.Address(),
							Capability: "quota:read",
						}, p.Redactor()))
			}

			for _, limit := range spec.Limits {
				if limit == nil {
					continue
				}
				out.Regions = append(out.Regions, projectRegionQuota(limit, used, &out))
			}

			// Namespace attachment is what makes a quota take effect at all, and
			// a quota attached to nothing is a common and silent misconfiguration.
			if namespaces, _, err := nomad.Namespaces().List(q); err == nil {
				for _, ns := range namespaces {
					if ns != nil && ns.Quota == name && p.Config().NamespaceAllowed(ns.Name) {
						out.Namespaces = append(out.Namespaces, ns.Name)
					}
				}
				if len(out.Namespaces) == 0 {
					out.Warnings = append(out.Warnings,
						"No namespace is attached to this quota, so it currently constrains nothing. "+
							"A quota takes effect only through a namespace's quota setting; set it "+
							"with create_namespace.")
				}
			}

			out.Note = "Usage is what the scheduler has reserved, not what tasks are consuming. " +
				"A limit of 0 means unlimited; a negative limit disallows the resource entirely."

			return utils.JSONResult(out)
		},
	}
}

// projectRegionQuota pairs one region's limit with its usage and flags any
// resource that is close to its cap.
func projectRegionQuota(limit *api.QuotaLimit, used map[string]*api.QuotaLimit, out *quotaDetail) regionQuota {
	r := regionQuota{Region: limit.Region, PercentUsed: map[string]int{}}

	if rl := limit.RegionLimit; rl != nil {
		r.CPULimit = rl.CPU
		r.CoresLimit = rl.Cores
		r.MemoryLimit = rl.MemoryMB
		r.MemoryMaxLimit = rl.MemoryMaxMB
		if s := rl.Storage; s != nil {
			r.VariablesLimitMB = utils.IntPtr(s.VariablesMB)
			r.HostVolumesLimitMB = utils.IntPtr(s.HostVolumesMB)
		}
	}

	u := used[limit.Region]
	if u == nil || u.RegionLimit == nil {
		return r
	}
	r.CPUUsed = u.RegionLimit.CPU
	r.CoresUsed = u.RegionLimit.Cores
	r.MemoryUsed = u.RegionLimit.MemoryMB

	track := func(resource string, allowed, consumed *int) {
		// Zero means unlimited in Nomad's quota model, so a percentage of it
		// would be both undefined and misleading.
		if allowed == nil || consumed == nil || *allowed <= 0 {
			return
		}
		pct := *consumed * 100 / *allowed
		r.PercentUsed[resource] = pct
		switch {
		case pct >= 100:
			out.Warnings = append(out.Warnings,
				"Region "+r.Region+" is at or over its "+resource+" quota ("+itoa(*consumed)+
					" of "+itoa(*allowed)+"). New jobs in the attached namespaces will be rejected "+
					"until something is stopped or the quota is raised.")
		case pct >= 90:
			out.Warnings = append(out.Warnings,
				"Region "+r.Region+" has used "+itoa(pct)+"% of its "+resource+" quota.")
		}
	}
	track("CPU", r.CPULimit, r.CPUUsed)
	track("cores", r.CoresLimit, r.CoresUsed)
	track("memory", r.MemoryLimit, r.MemoryUsed)

	return r
}

// CreateQuota creates or updates a resource quota.
func CreateQuota(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("create_quota",
			mcp.WithDescription(
				"Create a resource quota, or update an existing one's limits.\n\n"+
					"A quota caps the total CPU and memory that jobs in its attached namespaces may "+
					"reserve. Creating one changes nothing on its own: a quota only takes effect "+
					"once a namespace points at it, which is the quota setting on create_namespace. "+
					"Say so rather than reporting the cap as applied.\n\n"+
					"Lowering a limit below what is already reserved does NOT stop anything "+
					"running. Existing allocations keep their reservations; what changes is that "+
					"no new work can be placed in those namespaces until usage falls back under "+
					"the cap. That can be surprising, so check read_quota for current usage before "+
					"lowering a limit.\n\n"+
					"This is an upsert: calling it with an existing quota's name REPLACES that "+
					"quota's limits rather than merging into them. Any region limit you do not "+
					"pass is dropped. Read the current state with read_quota first if you are "+
					"modifying rather than creating.\n\n"+
					"A limit of 0 means unlimited. Requires Nomad Enterprise."),
			// Not destructive: no running work is stopped or discarded. It can
			// block future placements, which the description explains.
			utils.MutatingTool(false, true),
			utils.EnterpriseTool(),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The quota's name."),
			),
			mcp.WithString("description",
				mcp.Description("A human-readable description of what this quota is for."),
			),
			mcp.WithString("limit_region",
				mcp.Description(
					"The region the limit applies in. Defaults to the region this server targets. "+
						"On a federated cluster a quota may hold one limit per region, but this tool "+
						"sets one at a time."),
			),
			mcp.WithNumber("cpu_mhz",
				mcp.Description("Total CPU in MHz that attached namespaces may reserve. 0 means unlimited."),
			),
			mcp.WithNumber("cores",
				mcp.Description("Total reserved CPU cores attached namespaces may use. 0 means unlimited."),
			),
			mcp.WithNumber("memory_mb",
				mcp.Description("Total memory in MB that attached namespaces may reserve. 0 means unlimited."),
			),
			mcp.WithNumber("memory_max_mb",
				mcp.Description(
					"Total oversubscribed memory in MB, for tasks using memory_max. 0 means unlimited."),
			),
			mcp.WithNumber("variables_mb",
				mcp.Description(
					"Total size in MB of all Nomad Variables in attached namespaces. 0 means unlimited."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return utils.ErrorResult("The 'name' argument cannot be empty.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			limitRegion := strings.TrimSpace(req.GetString("limit_region", ""))
			if limitRegion == "" {
				limitRegion = region
			}
			// Nomad requires a region on the limit. When neither the tool
			// argument nor the server configuration names one, ask the cluster
			// rather than guessing "global".
			if limitRegion == "" {
				if r, err := nomad.Agent().Region(); err == nil {
					limitRegion = r
				}
			}
			if limitRegion == "" {
				return utils.ErrorResult(
					"A quota limit must name the region it applies in, and the region could not be " +
						"determined automatically. Pass 'limit_region'; list_regions shows what exists.")
			}

			resources := &api.QuotaResources{}
			if v, ok := intArg(req, "cpu_mhz"); ok {
				resources.CPU = &v
			}
			if v, ok := intArg(req, "cores"); ok {
				resources.Cores = &v
			}
			if v, ok := intArg(req, "memory_mb"); ok {
				resources.MemoryMB = &v
			}
			if v, ok := intArg(req, "memory_max_mb"); ok {
				resources.MemoryMaxMB = &v
			}
			if v, ok := intArg(req, "variables_mb"); ok {
				resources.Storage = &api.QuotaStorageResources{VariablesMB: v}
			}

			if resources.CPU == nil && resources.Cores == nil && resources.MemoryMB == nil &&
				resources.MemoryMaxMB == nil && resources.Storage == nil {
				return utils.ErrorResult(
					"A quota with no limits would cap nothing. Give at least one of cpu_mhz, cores, " +
						"memory_mb, memory_max_mb or variables_mb.")
			}

			existing, _, infoErr := nomad.Quotas().Info(name, &api.QueryOptions{Region: region})
			isUpdate := infoErr == nil && existing != nil

			spec := &api.QuotaSpec{
				Name:        name,
				Description: req.GetString("description", ""),
				Limits: []*api.QuotaLimit{{
					Region:      limitRegion,
					RegionLimit: resources,
				}},
			}

			if _, err := nomad.Quotas().Register(spec, &api.WriteOptions{Region: region}); err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "create quota " + name,
					Kind:       "quota",
					Name:       name,
					Address:    p.Address(),
					Capability: "quota:write",
				}, p.Redactor()))
			}

			action, note := "created", "The quota exists but constrains nothing yet: a quota only "+
				"applies to namespaces whose quota setting names it. Attach it with "+
				"create_namespace, then confirm with read_quota."
			if isUpdate {
				action = "updated"
				note = "The quota's limits were replaced. Nothing already running was stopped — " +
					"existing allocations keep their reservations. If usage now exceeds the new " +
					"limit, new placements in the attached namespaces are blocked until it falls " +
					"back under; read_quota shows where it stands."
			}

			return utils.JSONResult(map[string]any{
				"name":         name,
				"action":       action,
				"limit_region": limitRegion,
				"note":         note,
			})
		},
	}
}

// DeleteQuota deletes a resource quota.
func DeleteQuota(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("delete_quota",
			mcp.WithDescription(
				"Delete a resource quota permanently.\n\n"+
					"This is irreversible, and it removes a limit rather than imposing one: the "+
					"namespaces attached to this quota become uncapped, free to consume whatever "+
					"the cluster has. On a shared cluster that is how one tenant starves the "+
					"others, so it is a bigger change than it looks.\n\n"+
					"Nomad refuses to delete a quota that namespaces still reference. Detach it "+
					"from each of them first — read_quota lists which they are.\n\n"+
					"Confirm with the user, naming the quota, before calling this."),
			utils.MutatingTool(true, true),
			utils.EnterpriseTool(),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The quota to delete. Must not be referenced by any namespace."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.Quotas().Delete(name, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				msg := utils.MapError(err, utils.ErrorContext{
					Op:         "delete quota " + name,
					Kind:       "quota",
					Name:       name,
					Address:    p.Address(),
					Capability: "quota:write",
					ListTool:   "list_quotas",
				}, p.Redactor())
				if strings.Contains(strings.ToLower(msg), "in use") {
					msg += "\n\nNomad will not delete a quota that namespaces still reference. " +
						"read_quota lists them; point each one at a different quota, or at none, first."
				}
				return utils.ErrorResult(msg)
			}

			return utils.JSONResult(map[string]any{
				"name":    name,
				"deleted": true,
				"note": "The quota was deleted permanently. Any namespace that referenced it is now " +
					"uncapped and may consume whatever cluster capacity is available.",
			})
		},
	}
}

// intArg reads an optional numeric argument, reporting whether it was supplied
// at all. The distinction matters for quota limits: an omitted limit and a
// limit of zero mean different things, and zero means unlimited.
func intArg(req mcp.CallToolRequest, name string) (int, bool) {
	if _, present := req.GetArguments()[name]; !present {
		return 0, false
	}
	return req.GetInt(name, 0), true
}
