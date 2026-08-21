// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package nodes holds the tools that read and manage Nomad client nodes.
package nodes

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/projection"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// NodeIDParam declares the node ID argument, shared with the write tools.
func NodeIDParam() mcp.ToolOption {
	return mcp.WithString("node_id",
		mcp.Required(),
		mcp.Description(
			"The node's full ID, as returned by list_nodes. This is a UUID, not the node's name — "+
				"use list_nodes or search to resolve a name or short ID first."),
	)
}

type nodeStub struct {
	ID          string   `json:"id"`
	ShortID     string   `json:"short_id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	StatusDesc  string   `json:"status_description,omitempty"`
	Datacenter  string   `json:"datacenter,omitempty"`
	NodePool    string   `json:"node_pool,omitempty"`
	NodeClass   string   `json:"node_class,omitempty"`
	Address     string   `json:"address,omitempty"`
	Version     string   `json:"version,omitempty"`
	Draining    bool     `json:"draining,omitempty"`
	Eligibility string   `json:"scheduling_eligibility,omitempty"`
	Drivers     []string `json:"healthy_drivers,omitempty"`
	Unhealthy   bool     `json:"needs_attention,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// ListNodes lists client nodes.
func ListNodes(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the client nodes registered with the cluster: their status, datacenter, node " +
				"pool and class, whether they are draining, and which task drivers are healthy on " +
				"each.\n\n" +
				"Use this when work is not being placed. A job can only run on a node that is ready, " +
				"eligible, in a matching datacenter and node pool, and running a healthy driver for " +
				"the task. Nodes flagged needs_attention are down, ineligible or draining, and are " +
				"therefore unavailable to the scheduler.\n\n" +
				"The healthy_drivers list is worth checking specifically: a job using the docker " +
				"driver cannot be placed on a node where docker is unhealthy, and Nomad reports that " +
				"as a constraint filter rather than as a driver problem."),
		utils.ReadOnlyTool(),
		utils.RegionParam(),
		utils.PrefixParam("nodes"),
		utils.FilterParam(`Status == "down"  •  NodePool == "default"  •  Drain == true`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_nodes", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			page := utils.PageFrom(req)
			q := page.Apply(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix: req.GetString("prefix", ""),
				Filter: req.GetString("filter", ""),
			})

			stubs, meta, err := nomad.Nodes().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list nodes",
					Kind:       "node",
					Address:    p.Address(),
					Capability: "node:read",
				}, p.Redactor()))
			}

			items := make([]nodeStub, 0, len(stubs))
			var unavailable int
			for _, s := range stubs {
				if s == nil {
					continue
				}
				item := nodeStub{
					ID:          s.ID,
					ShortID:     utils.ShortID(s.ID),
					Name:        s.Name,
					Status:      s.Status,
					StatusDesc:  s.StatusDescription,
					Datacenter:  s.Datacenter,
					NodePool:    s.NodePool,
					NodeClass:   s.NodeClass,
					Address:     s.Address,
					Version:     s.Version,
					Draining:    s.Drain,
					Eligibility: s.SchedulingEligibility,
					Drivers:     healthyDrivers(s.Drivers),
				}
				item.Unhealthy = s.Status != "ready" || s.Drain ||
					s.SchedulingEligibility == "ineligible"
				if item.Unhealthy {
					unavailable++
					item.Note = unavailableReason(s)
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			switch {
			case len(items) == 0:
				result.Note = "No client nodes are registered. Nothing can be scheduled until at least one joins."
			case result.Note == "" && unavailable == len(items):
				result.Note = "Every node is unavailable to the scheduler. No new work can be placed anywhere."
			case result.Note == "" && unavailable > 0:
				result.Note = "Some nodes are unavailable to the scheduler; see each node's note field."
			}
			return utils.JSONResult(result)
		},
	}
}

func unavailableReason(s *api.NodeListStub) string {
	switch {
	case s.Status == "down":
		return "This node is down: it has stopped heartbeating, and its allocations will be rescheduled elsewhere."
	case s.Status == "initializing":
		return "This node is still initializing and is not ready for work yet."
	case s.Drain:
		return "This node is draining: existing allocations are being migrated off and no new work will be placed."
	case s.SchedulingEligibility == "ineligible":
		return "This node is marked ineligible, so the scheduler will not place new work on it. Existing allocations keep running."
	}
	return ""
}

func healthyDrivers(drivers map[string]*api.DriverInfo) []string {
	var out []string
	for name, info := range drivers {
		if info != nil && info.Detected && info.Healthy {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out
}

type nodeDetail struct {
	nodeStub
	Resources   *nodeResources    `json:"resources,omitempty"`
	Reserved    *nodeResources    `json:"reserved,omitempty"`
	Drivers     map[string]driver `json:"drivers,omitempty"`
	CSIPlugins  map[string]string `json:"csi_plugins,omitempty"`
	HostVolumes []string          `json:"host_volumes,omitempty"`
	Attributes  map[string]string `json:"selected_attributes,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
	DrainDetail *drainDetail      `json:"drain,omitempty"`
	LastDrain   string            `json:"last_drain,omitempty"`
	Events      []nodeEvent       `json:"recent_events,omitempty"`
	Diagnosis   string            `json:"diagnosis,omitempty"`
}

type nodeResources struct {
	CPUMHz   int64 `json:"cpu_mhz,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`
	DiskMB   int64 `json:"disk_mb,omitempty"`
	Cores    int   `json:"cpu_cores,omitempty"`
}

type driver struct {
	Detected bool   `json:"detected"`
	Healthy  bool   `json:"healthy"`
	Message  string `json:"message,omitempty"`
	Updated  string `json:"updated,omitempty"`
}

type drainDetail struct {
	Deadline     string `json:"deadline,omitempty"`
	IgnoreSystem bool   `json:"ignore_system_jobs,omitempty"`
	ForceDrain   bool   `json:"force,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
}

type nodeEvent struct {
	Time    string `json:"time"`
	Subsys  string `json:"subsystem,omitempty"`
	Message string `json:"message"`
}

// ReadNode returns one node in detail.
func ReadNode(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_node",
			mcp.WithDescription(
				"Read one client node in detail: its status and eligibility, total and reserved "+
					"resources, the state of every task driver, its host volumes, and its recent "+
					"node events.\n\n"+
					"Use this when a job will not place on a node you expected it to, or when a node "+
					"is behaving oddly. The driver states explain most placement surprises — a driver "+
					"that is detected but unhealthy takes the node out of consideration for any task "+
					"using it, and the driver's message usually says why.\n\n"+
					"Node events are Nomad's own record of what happened to the node, including "+
					"driver health transitions and drain operations."),
			utils.ReadOnlyTool(),
			NodeIDParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return readNode(ctx, req, p)
		},
	}
}

func readNode(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	nodeID, err := req.RequireString("node_id")
	if err != nil {
		return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	node, _, err := nomad.Nodes().Info(nodeID, &api.QueryOptions{
		Region: p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read node " + utils.ShortID(nodeID),
			Kind:       "node",
			Name:       nodeID,
			Address:    p.Address(),
			Capability: "node:read",
			ListTool:   "list_nodes",
		}, p.Redactor()))
	}

	out := nodeDetail{
		nodeStub: nodeStub{
			ID:          node.ID,
			ShortID:     utils.ShortID(node.ID),
			Name:        node.Name,
			Status:      node.Status,
			StatusDesc:  node.StatusDescription,
			Datacenter:  node.Datacenter,
			NodePool:    node.NodePool,
			NodeClass:   node.NodeClass,
			Address:     node.HTTPAddr,
			Version:     node.Attributes["nomad.version"],
			Draining:    node.Drain,
			Eligibility: node.SchedulingEligibility,
			Drivers:     healthyDrivers(node.Drivers),
		},
		Meta: node.Meta,
	}
	out.Unhealthy = node.Status != "ready" || node.Drain || node.SchedulingEligibility == "ineligible"

	if node.NodeResources != nil {
		out.Resources = &nodeResources{}
		if node.NodeResources.Cpu.CpuShares > 0 {
			out.Resources.CPUMHz = node.NodeResources.Cpu.CpuShares
		}
		out.Resources.MemoryMB = node.NodeResources.Memory.MemoryMB
		out.Resources.DiskMB = node.NodeResources.Disk.DiskMB
		out.Resources.Cores = len(node.NodeResources.Cpu.ReservableCpuCores)
	}
	if node.ReservedResources != nil {
		out.Reserved = &nodeResources{
			CPUMHz:   int64(node.ReservedResources.Cpu.CpuShares),
			MemoryMB: int64(node.ReservedResources.Memory.MemoryMB),
			DiskMB:   int64(node.ReservedResources.Disk.DiskMB),
		}
	}

	for name, info := range node.Drivers {
		if info == nil {
			continue
		}
		if out.Drivers == nil {
			out.Drivers = map[string]driver{}
		}
		out.Drivers[name] = driver{
			Detected: info.Detected,
			Healthy:  info.Healthy,
			Message:  info.HealthDescription,
			Updated:  utils.FormatTime(info.UpdateTime.UnixNano()),
		}
	}

	for name := range node.HostVolumes {
		out.HostVolumes = append(out.HostVolumes, name)
	}
	sortStrings(out.HostVolumes)

	for name, plugin := range node.CSINodePlugins {
		if plugin == nil {
			continue
		}
		if out.CSIPlugins == nil {
			out.CSIPlugins = map[string]string{}
		}
		if plugin.Healthy {
			out.CSIPlugins[name] = "healthy"
		} else {
			out.CSIPlugins[name] = "unhealthy: " + plugin.HealthDescription
		}
	}

	// A handful of attributes are worth surfacing; the full set is hundreds of
	// keys and would swamp the response.
	out.Attributes = selectedAttributes(node.Attributes)

	if node.DrainStrategy != nil {
		out.DrainDetail = &drainDetail{
			IgnoreSystem: node.DrainStrategy.IgnoreSystemJobs,
		}
		if node.DrainStrategy.Deadline > 0 {
			out.DrainDetail.Deadline = node.DrainStrategy.Deadline.String()
		}
	}
	if node.LastDrain != nil {
		out.LastDrain = node.LastDrain.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z") +
			" (" + string(node.LastDrain.Status) + ")"
	}

	events := node.Events
	if len(events) > 10 {
		events = events[len(events)-10:]
	}
	for _, e := range events {
		if e == nil {
			continue
		}
		out.Events = append(out.Events, nodeEvent{
			Time:    utils.FormatTime(e.Timestamp.UnixNano()),
			Subsys:  e.Subsystem,
			Message: p.Redactor().String(e.Message),
		})
	}

	out.Diagnosis = diagnoseNode(node, out)

	return utils.JSONResult(out)
}

func diagnoseNode(node *api.Node, d nodeDetail) string {
	var problems []string

	if node.Status == "down" {
		problems = append(problems,
			"the node is down and not heartbeating, so Nomad will reschedule its allocations elsewhere")
	}
	if node.Drain {
		problems = append(problems,
			"the node is draining, so no new work will be placed on it")
	}
	if node.SchedulingEligibility == "ineligible" && !node.Drain {
		problems = append(problems,
			"the node is marked ineligible, so the scheduler is skipping it; set_node_eligibility can restore it")
	}

	var unhealthy []string
	for name, info := range d.Drivers {
		if info.Detected && !info.Healthy {
			unhealthy = append(unhealthy, name)
		}
	}
	sortStrings(unhealthy)
	if len(unhealthy) > 0 {
		problems = append(problems,
			"the driver(s) "+strings.Join(unhealthy, ", ")+" are detected but unhealthy, so tasks using them cannot be placed here")
	}

	if len(problems) == 0 {
		return ""
	}
	return "This node is not fully available: " + strings.Join(problems, "; ") + "."
}

// selectedAttributes picks the few node attributes that matter for triage.
func selectedAttributes(attrs map[string]string) map[string]string {
	wanted := []string{
		"os.name", "os.version", "kernel.name", "kernel.version",
		"cpu.arch", "cpu.numcores", "cpu.modelname",
		"memory.totalbytes", "unique.hostname",
		"nomad.version", "consul.version", "vault.version",
	}
	out := map[string]string{}
	for _, k := range wanted {
		if v, ok := attrs[k]; ok && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ListNodeAllocations lists the allocations on one node.
func ListNodeAllocations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_node_allocations",
			mcp.WithDescription(
				"List every allocation currently assigned to one client node, across all jobs and "+
					"namespaces.\n\n"+
					"Use this to see what a node is actually carrying: before draining it, to know "+
					"what will move; when a node is under resource pressure, to find what is "+
					"consuming it; or when several unrelated jobs fail at once and you suspect a "+
					"single bad node is the common factor."),
			utils.ReadOnlyTool(),
			NodeIDParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nodeID, err := req.RequireString("node_id")
			if err != nil {
				return utils.ErrorResult("The 'node_id' argument is required. Use list_nodes to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			allocations, _, err := nomad.Nodes().Allocations(nodeID, &api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list allocations on node " + utils.ShortID(nodeID),
					Kind:       "node",
					Name:       nodeID,
					Address:    p.Address(),
					Capability: "node:read (plus read-job for the allocations)",
					ListTool:   "list_nodes",
				}, p.Redactor()))
			}

			cfg := p.Config()
			items := make([]projection.AllocStub, 0, len(allocations))
			var hidden int
			for _, a := range allocations {
				if a == nil {
					continue
				}
				// This endpoint spans namespaces, so the allowlist has to be
				// applied to the results rather than to the request.
				if !cfg.NamespaceAllowed(a.Namespace) {
					hidden++
					continue
				}
				items = append(items, projection.Alloc(a.Stub()))
			}

			result := utils.List{Count: len(items), Items: items}
			switch {
			case hidden > 0:
				result.Note = "Some allocations on this node were omitted because they are in " +
					"namespaces this server is not permitted to read."
			case len(items) == 0:
				result.Note = "This node has no allocations. It may be new, draining, or ineligible — check read_node."
			}
			return utils.JSONResult(result)
		},
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
