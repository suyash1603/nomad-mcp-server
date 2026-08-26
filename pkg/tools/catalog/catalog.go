// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package catalog holds the tools for the things a cluster contains rather than
// runs: namespaces, service registrations, and volumes.
package catalog

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

type namespaceInfo struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Quota       string            `json:"quota,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
	NodePools   *nodePoolConfig   `json:"node_pool_config,omitempty"`
	Accessible  bool              `json:"accessible_by_this_server"`
}

type nodePoolConfig struct {
	Default string   `json:"default,omitempty"`
	Allowed []string `json:"allowed,omitempty"`
	Denied  []string `json:"denied,omitempty"`
}

// ListNamespaces lists namespaces.
func ListNamespaces(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_namespaces",
			mcp.WithDescription(
				"List the namespaces defined in the cluster. Namespaces partition jobs, allocations "+
					"and variables, and almost every other tool operates within one.\n\n"+
					"Use this when a job or allocation cannot be found where you expected it: living "+
					"in a different namespace is the single most common reason. Each namespace here "+
					"reports whether this MCP server is permitted to read it, which may be narrower "+
					"than what your Nomad token allows."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
			utils.PrefixParam("namespaces"),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix: req.GetString("prefix", ""),
			}

			namespaces, _, err := nomad.Namespaces().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list namespaces",
					Kind:       "namespace",
					Address:    p.Address(),
					Capability: "namespace:read (or any capability in a namespace)",
				}, p.Redactor()))
			}

			cfg := p.Config()
			items := make([]namespaceInfo, 0, len(namespaces))
			restricted := 0
			for _, ns := range namespaces {
				if ns == nil {
					continue
				}
				item := namespaceInfo{
					Name:        ns.Name,
					Description: ns.Description,
					Quota:       ns.Quota,
					Meta:        ns.Meta,
					Accessible:  cfg.NamespaceAllowed(ns.Name),
				}
				if !item.Accessible {
					restricted++
				}
				if npc := ns.NodePoolConfiguration; npc != nil {
					item.NodePools = &nodePoolConfig{
						Default: npc.Default,
						Allowed: npc.Allowed,
						Denied:  npc.Denied,
					}
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Items: items}
			if restricted > 0 {
				result.Note = "Some namespaces are marked not accessible: this server is configured " +
					"with NOMAD_MCP_ALLOWED_NAMESPACES and will refuse tool calls against them, " +
					"regardless of what your Nomad token permits."
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadNamespace returns one namespace.
func ReadNamespace(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_namespace",
			mcp.WithDescription(
				"Read one namespace's configuration: its description, any resource quota attached "+
					"to it, its node pool restrictions and its metadata.\n\n"+
					"The node pool configuration matters when a job in this namespace will not "+
					"place: a namespace can restrict which node pools its jobs are allowed to use, "+
					"which filters out nodes before any of the job's own constraints apply."),
			utils.ReadOnlyTool(),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The namespace's name, as returned by list_namespaces."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required. Use list_namespaces to see what exists.")
			}
			// Reading a namespace's configuration is itself namespaced, so the
			// allowlist applies.
			if _, err := p.ResolveNamespace(ctx, name); err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			ns, _, err := nomad.Namespaces().Info(name, &api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read namespace " + name,
					Kind:       "namespace",
					Name:       name,
					Address:    p.Address(),
					Capability: "namespace:read",
					ListTool:   "list_namespaces",
				}, p.Redactor()))
			}

			out := namespaceInfo{
				Name:        ns.Name,
				Description: ns.Description,
				Quota:       ns.Quota,
				Meta:        ns.Meta,
				Accessible:  true,
			}
			if npc := ns.NodePoolConfiguration; npc != nil {
				out.NodePools = &nodePoolConfig{
					Default: npc.Default,
					Allowed: npc.Allowed,
					Denied:  npc.Denied,
				}
			}
			return utils.JSONResult(out)
		},
	}
}

type serviceStub struct {
	Name string   `json:"name"`
	Tags []string `json:"tags,omitempty"`
}

// ListServices lists service registrations.
func ListServices(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the services registered in Nomad's own service discovery for a namespace.\n\n" +
				"These are services declared with provider = \"nomad\" in a job's service block. " +
				"Services registered into Consul instead will NOT appear here — that is the most " +
				"common reason this returns an empty list on a cluster that clearly has services.\n\n" +
				"Use read_service to see the individual instances, their addresses and ports."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("services"),
		utils.FilterParam(`ServiceName contains "api"  •  Tags contains "canary"`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_services", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
				Prefix:    req.GetString("prefix", ""),
				Filter:    req.GetString("filter", ""),
			})

			listed, meta, err := nomad.Services().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list services",
					Kind:       "service",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
				}, p.Redactor()))
			}

			items := make([]serviceStub, 0)
			for _, group := range listed {
				if group == nil {
					continue
				}
				for _, svc := range group.Services {
					if svc == nil {
						continue
					}
					items = append(items, serviceStub{Name: svc.ServiceName, Tags: svc.Tags})
				}
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if len(items) == 0 && result.Note == "" {
				result.Note = "No services registered with Nomad's own service discovery in namespace " +
					namespace + ". Jobs using provider = \"consul\" register with Consul instead and " +
					"will not appear here."
				if req.GetString("prefix", "") != "" || req.GetString("filter", "") != "" {
					result.Note += " A prefix or filter was also applied; try removing it."
				}
			}
			return utils.JSONResult(result)
		},
	}
}

type serviceInstance struct {
	ID         string   `json:"id"`
	Name       string   `json:"service_name"`
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	Tags       []string `json:"tags,omitempty"`
	JobID      string   `json:"job_id,omitempty"`
	AllocID    string   `json:"alloc_id,omitempty"`
	NodeID     string   `json:"node_id,omitempty"`
	Datacenter string   `json:"datacenter,omitempty"`
}

// ReadService lists the instances of one service.
func ReadService(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the registered instances of one Nomad service: each instance's address, port, " +
				"tags, and the job and allocation it belongs to.\n\n" +
				"Use this to check whether a service actually has healthy instances behind it, and " +
				"to map a service back to the allocation serving it — the alloc_id here is what you " +
				"need for read_allocation_logs.\n\n" +
				"An empty result for a service that should exist usually means its allocations are " +
				"not running; check the owning job with list_job_allocations."),
		utils.ReadOnlyTool(),
		mcp.WithString("service_name",
			mcp.Required(),
			mcp.Description("The service's name, as returned by list_services."),
		),
		utils.NamespaceParam(),
		utils.RegionParam(),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("read_service", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("service_name")
			if err != nil {
				return utils.ErrorResult("The 'service_name' argument is required. Use list_services to see what exists.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})

			regs, meta, err := nomad.Services().Get(name, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read service " + name,
					Kind:       "service",
					Name:       name,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_services",
				}, p.Redactor()))
			}

			items := make([]serviceInstance, 0, len(regs))
			for _, r := range regs {
				if r == nil {
					continue
				}
				items = append(items, serviceInstance{
					ID:         r.ID,
					Name:       r.ServiceName,
					Address:    r.Address,
					Port:       r.Port,
					Tags:       r.Tags,
					JobID:      r.JobID,
					AllocID:    r.AllocID,
					NodeID:     r.NodeID,
					Datacenter: r.Datacenter,
				})
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
			}
			if len(items) == 0 {
				result.Note = "Service \"" + name + "\" has no registered instances in namespace " +
					namespace + ". Its allocations are probably not running — check the owning job."
			}
			return utils.JSONResult(result)
		},
	}
}

type volumeStub struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type"`
	Namespace string `json:"namespace,omitempty"`
	Plugin    string `json:"plugin_id,omitempty"`
	State     string `json:"state,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	NodePool  string `json:"node_pool,omitempty"`
	Capacity  int64  `json:"capacity_bytes,omitempty"`
	Schedule  string `json:"schedulable,omitempty"`
	Healthy   string `json:"health,omitempty"`
}

// volumeTypeParam declares the volume type selector.
//
// Nomad serves CSI volumes and dynamic host volumes from entirely separate
// APIs, so one tool has to choose. Defaulting to CSI matches what most people
// mean by "volume".
func volumeTypeParam() mcp.ToolOption {
	return mcp.WithString("type",
		mcp.DefaultString("csi"),
		mcp.Enum("csi", "host"),
		mcp.Description(
			"Which kind of volume to look at. \"csi\" covers volumes provided by a CSI plugin and "+
				"is the default. \"host\" covers Nomad's dynamic host volumes, which live on a "+
				"specific client node. These are separate systems in Nomad with separate APIs; a "+
				"volume that exists as one will not appear as the other."),
	)
}

// ListVolumes lists CSI or host volumes.
func ListVolumes(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the storage volumes available to jobs, either CSI volumes or Nomad's dynamic " +
				"host volumes.\n\n" +
				"Use this when a job that mounts a volume will not place: the volume must exist, be " +
				"schedulable, and for host volumes be on a node the job can reach. A volume that is " +
				"not schedulable will block placement without the job itself looking wrong."),
		utils.ReadOnlyTool(),
		volumeTypeParam(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		mcp.WithString("node_id",
			mcp.Description(
				"Return only volumes attached to, or available on, this client node. "+
					"Applies to both volume types. Use it when a job will not place on a "+
					"particular node and you need to know what storage that node can actually see."),
		),
		mcp.WithString("plugin_id",
			mcp.Description(
				"Return only volumes provided by this CSI plugin. Applies to CSI volumes only "+
					"and is ignored when type is \"host\". Use it to scope a storage problem to one "+
					"plugin when a cluster runs several."),
		),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_volumes", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			volType := req.GetString("type", "csi")
			nodeID := req.GetString("node_id", "")
			pluginID := req.GetString("plugin_id", "")

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})

			var items []volumeStub

			switch volType {
			case "host":
				// Host volumes take their scoping in the request body rather
				// than as query parameters; plugin_id has no meaning here,
				// because host volumes have no plugin.
				stubs, _, err := nomad.HostVolumes().List(&api.HostVolumeListRequest{NodeID: nodeID}, q)
				if err != nil {
					return utils.ErrorResult(volumeError(err, p, "list host volumes", namespace))
				}
				for _, v := range stubs {
					if v == nil {
						continue
					}
					items = append(items, volumeStub{
						ID:        v.ID,
						Name:      v.Name,
						Type:      "host",
						Namespace: v.Namespace,
						Plugin:    v.PluginID,
						NodeID:    v.NodeID,
						NodePool:  v.NodePool,
						Capacity:  v.CapacityBytes,
						State:     string(v.State),
					})
				}

			default:
				// The CSI list endpoint scopes through query parameters. Both
				// are applied by the Nomad servers, so a cluster with thousands
				// of volumes never sends them all back to be discarded here.
				if nodeID != "" || pluginID != "" {
					if q.Params == nil {
						q.Params = map[string]string{}
					}
					if nodeID != "" {
						q.Params["node_id"] = nodeID
					}
					if pluginID != "" {
						q.Params["plugin_id"] = pluginID
					}
				}

				stubs, _, err := nomad.CSIVolumes().List(q)
				if err != nil {
					return utils.ErrorResult(volumeError(err, p, "list CSI volumes", namespace))
				}
				for _, v := range stubs {
					if v == nil {
						continue
					}
					items = append(items, volumeStub{
						ID:        v.ID,
						Name:      v.Name,
						Type:      "csi",
						Namespace: v.Namespace,
						Plugin:    v.PluginID,
						Schedule:  boolWord(v.Schedulable, "schedulable", "not schedulable"),
						Healthy:   healthWord(v.ControllersHealthy, v.NodesHealthy),
					})
				}
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if len(items) == 0 {
				other := "host"
				if volType == "host" {
					other = "csi"
				}
				result.Note = "No " + volType + " volumes found in namespace " + namespace +
					". Note that Nomad keeps CSI and host volumes in separate systems — try type \"" +
					other + "\" if you expected a volume here."
				if nodeID != "" || pluginID != "" {
					result.Note += " This call was also scoped by node_id or plugin_id; " +
						"try removing that before concluding the volume does not exist."
				}
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadVolume returns one volume.
func ReadVolume(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_volume",
			mcp.WithDescription(
				"Read one storage volume in detail: its plugin, capacity, whether it is currently "+
					"schedulable, and which allocations have it mounted.\n\n"+
					"Use this when a job cannot mount a volume, or to find out what is currently "+
					"using one before changing it. A CSI volume that reports as not schedulable "+
					"usually has an unhealthy plugin behind it; check the plugin's nodes."),
			utils.ReadOnlyTool(),
			mcp.WithString("volume_id",
				mcp.Required(),
				mcp.Description("The volume's ID, as returned by list_volumes."),
			),
			volumeTypeParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("volume_id")
			if err != nil {
				return utils.ErrorResult("The 'volume_id' argument is required. Use list_volumes to see what exists.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := &api.QueryOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			}

			if req.GetString("type", "csi") == "host" {
				v, _, err := nomad.HostVolumes().Get(id, q)
				if err != nil {
					return utils.ErrorResult(volumeError(err, p, "read host volume "+id, namespace))
				}
				return utils.JSONResult(map[string]any{
					"id":             v.ID,
					"name":           v.Name,
					"type":           "host",
					"namespace":      v.Namespace,
					"node_id":        v.NodeID,
					"node_pool":      v.NodePool,
					"plugin_id":      v.PluginID,
					"capacity_bytes": v.CapacityBytes,
					"state":          string(v.State),
					"host_path":      v.HostPath,
					"allocations":    allocIDs(v.Allocations),
				})
			}

			v, _, err := nomad.CSIVolumes().Info(id, q)
			if err != nil {
				return utils.ErrorResult(volumeError(err, p, "read CSI volume "+id, namespace))
			}

			out := map[string]any{
				"id":                  v.ID,
				"name":                v.Name,
				"type":                "csi",
				"namespace":           v.Namespace,
				"plugin_id":           v.PluginID,
				"provider":            v.Provider,
				"schedulable":         v.Schedulable,
				"access_mode":         string(v.AccessMode),
				"attachment_mode":     string(v.AttachmentMode),
				"controllers_healthy": v.ControllersHealthy,
				"controllers_total":   v.ControllersExpected,
				"nodes_healthy":       v.NodesHealthy,
				"nodes_total":         v.NodesExpected,
				"capacity_bytes":      v.Capacity,
				"read_allocs":         len(v.ReadAllocs),
				"write_allocs":        len(v.WriteAllocs),
			}
			if !v.Schedulable {
				out["note"] = "This volume is not schedulable, so any job requiring it cannot be " +
					"placed. That usually means its CSI plugin has no healthy controllers or nodes."
			}
			return utils.JSONResult(out)
		},
	}
}

// volumeError maps a volume failure, noting that CSI is often simply not set up.
func volumeError(err error, p *client.Provider, op, namespace string) string {
	msg := utils.MapError(err, utils.ErrorContext{
		Op:         op,
		Kind:       "volume",
		Namespace:  namespace,
		Address:    p.Address(),
		Capability: "csi-read-volume (or host-volume-read)",
		ListTool:   "list_volumes",
	}, p.Redactor())

	if strings.Contains(strings.ToLower(msg), "no such") || utils.IsNotFound(err) {
		msg += "\n\nNomad keeps CSI volumes and dynamic host volumes in separate systems. If you " +
			"expected this volume to exist, try the other value for the 'type' argument."
	}
	return msg
}

func allocIDs(allocs []*api.AllocationListStub) []string {
	var out []string
	for _, a := range allocs {
		if a != nil {
			out = append(out, a.ID)
		}
	}
	return out
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func healthWord(controllers, nodes int) string {
	if controllers == 0 && nodes == 0 {
		return "no healthy plugins"
	}
	return "controllers healthy: " + itoa(controllers) + ", nodes healthy: " + itoa(nodes)
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
