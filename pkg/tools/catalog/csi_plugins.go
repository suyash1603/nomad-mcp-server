// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"context"
	"sort"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// pluginStub is the list view of a CSI plugin.
type pluginStub struct {
	ID          string `json:"id"`
	Provider    string `json:"provider,omitempty"`
	Controllers string `json:"controllers,omitempty"`
	Nodes       string `json:"nodes"`
	Healthy     bool   `json:"healthy"`
	Problem     string `json:"problem,omitempty"`
}

// pluginDetail is the read view, including per-node fingerprints.
type pluginDetail struct {
	ID                 string           `json:"id"`
	Provider           string           `json:"provider,omitempty"`
	Version            string           `json:"provider_version,omitempty"`
	ControllerRequired bool             `json:"controller_required"`
	Controllers        string           `json:"controllers,omitempty"`
	Nodes              string           `json:"nodes"`
	Healthy            bool             `json:"healthy"`
	Problem            string           `json:"problem,omitempty"`
	Unhealthy          []pluginInstance `json:"unhealthy_instances,omitempty"`
	AllocCount         int              `json:"plugin_allocations,omitempty"`
	Note               string           `json:"note,omitempty"`
}

// pluginInstance is one plugin allocation's fingerprint on one node.
type pluginInstance struct {
	Kind    string `json:"kind"`
	NodeID  string `json:"node_id"`
	AllocID string `json:"alloc_id,omitempty"`
	Healthy bool   `json:"healthy"`
	Why     string `json:"health_description,omitempty"`
	Updated string `json:"updated,omitempty"`
}

// ListCSIPlugins lists the CSI plugins registered in the cluster.
func ListCSIPlugins(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the CSI storage plugins registered in this cluster, with how many of their " +
				"controllers and node instances are actually healthy versus expected.\n\n" +
				"When a job will not place because of a volume, or a volume will not mount, this is " +
				"usually where the answer is rather than in the volume itself. A volume is only " +
				"schedulable if its plugin is healthy, so a plugin with fewer healthy node " +
				"instances than expected will block placement on exactly the nodes that are " +
				"missing one — while the volume itself still looks fine.\n\n" +
				"Use read_csi_plugin for which specific nodes are unhealthy, and diagnose_volume " +
				"to go straight from a volume to the reason it cannot be used."),
		utils.ReadOnlyTool(),
		utils.RegionParam(),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_csi_plugins", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			q := utils.PageFrom(req).Apply(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})

			stubs, meta, err := nomad.CSIPlugins().List(q)
			if err != nil {
				return utils.ErrorResult(pluginError(err, p, "list CSI plugins", ""))
			}

			items := make([]pluginStub, 0, len(stubs))
			var degraded int
			for _, s := range stubs {
				if s == nil {
					continue
				}
				item := pluginStub{
					ID:       s.ID,
					Provider: s.Provider,
					Nodes:    ratio(s.NodesHealthy, s.NodesExpected),
					Healthy:  pluginHealthy(s.ControllerRequired, s.ControllersHealthy, s.ControllersExpected, s.NodesHealthy, s.NodesExpected),
				}
				if s.ControllerRequired || s.ControllersExpected > 0 {
					item.Controllers = ratio(s.ControllersHealthy, s.ControllersExpected)
				}
				if !item.Healthy {
					degraded++
					item.Problem = pluginProblem(s.ControllerRequired,
						s.ControllersHealthy, s.ControllersExpected, s.NodesHealthy, s.NodesExpected)
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			switch {
			case len(items) == 0 && result.Note == "":
				result.Note = "No CSI plugins are registered. A job that mounts a CSI volume cannot " +
					"place until the plugin providing it is running — plugins are themselves Nomad " +
					"jobs, so check list_jobs for one that was never submitted or has stopped."
			case degraded > 0:
				result.Note = utils.NextTokenNote(result.NextToken, len(items)) +
					" " + itoa(degraded) + " of these plugins are not fully healthy. Volumes they " +
					"provide will be unschedulable on the nodes missing a healthy instance, even " +
					"though the volumes themselves look fine."
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadCSIPlugin reads one plugin, naming the instances that are unhealthy.
func ReadCSIPlugin(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_csi_plugin",
			mcp.WithDescription(
				"Read one CSI plugin: whether it needs a controller, how many controller and node "+
					"instances are healthy against how many are expected, and — the part worth "+
					"having — exactly which instances are unhealthy and what the fingerprint says "+
					"about them.\n\n"+
					"Reach for this when list_csi_plugins shows a plugin short of its expected "+
					"count. The unhealthy instances name the nodes where volumes from this plugin "+
					"cannot be mounted, which is what turns \"the volume will not mount\" into "+
					"\"the volume will not mount on these three nodes\"."),
			utils.ReadOnlyTool(),
			mcp.WithString("plugin_id",
				mcp.Required(),
				mcp.Description("The plugin's ID, exactly as returned by list_csi_plugins."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			pluginID, err := req.RequireString("plugin_id")
			if err != nil {
				return utils.ErrorResult(
					"The 'plugin_id' argument is required. Use list_csi_plugins to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			plugin, _, err := nomad.CSIPlugins().Info(pluginID, &api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(pluginError(err, p, "read CSI plugin "+pluginID, ""))
			}

			return utils.JSONResult(projectPlugin(plugin))
		},
	}
}

// projectPlugin builds the read view of a plugin.
func projectPlugin(plugin *api.CSIPlugin) pluginDetail {
	if plugin == nil {
		return pluginDetail{}
	}

	out := pluginDetail{
		ID:                 plugin.ID,
		Provider:           plugin.Provider,
		Version:            plugin.Version,
		ControllerRequired: plugin.ControllerRequired,
		Nodes:              ratio(plugin.NodesHealthy, plugin.NodesExpected),
		AllocCount:         len(plugin.Allocations),
		Healthy: pluginHealthy(plugin.ControllerRequired,
			plugin.ControllersHealthy, plugin.ControllersExpected,
			plugin.NodesHealthy, plugin.NodesExpected),
	}
	if plugin.ControllerRequired || plugin.ControllersExpected > 0 {
		out.Controllers = ratio(plugin.ControllersHealthy, plugin.ControllersExpected)
	}

	out.Unhealthy = append(out.Unhealthy, unhealthyInstances("controller", plugin.Controllers)...)
	out.Unhealthy = append(out.Unhealthy, unhealthyInstances("node", plugin.Nodes)...)
	sort.Slice(out.Unhealthy, func(i, j int) bool {
		if out.Unhealthy[i].Kind != out.Unhealthy[j].Kind {
			return out.Unhealthy[i].Kind < out.Unhealthy[j].Kind
		}
		return out.Unhealthy[i].NodeID < out.Unhealthy[j].NodeID
	})

	// Order matters: a plugin with no node instances is also unhealthy, so the
	// specific case has to be tested first or its more useful message is
	// unreachable.
	switch {
	case plugin.NodesExpected == 0:
		out.Problem = pluginProblem(plugin.ControllerRequired,
			plugin.ControllersHealthy, plugin.ControllersExpected,
			plugin.NodesHealthy, plugin.NodesExpected)
		out.Note = "This plugin has no node instances at all, so nothing can mount its volumes. " +
			"A node plugin usually runs as a system job so that every client gets one — check " +
			"that job exists, is running, and is not constrained away from your clients."
	case !out.Healthy:
		out.Problem = pluginProblem(plugin.ControllerRequired,
			plugin.ControllersHealthy, plugin.ControllersExpected,
			plugin.NodesHealthy, plugin.NodesExpected)
		out.Note = "Volumes from this plugin are unschedulable wherever an instance is missing or " +
			"unhealthy. The plugin runs as a Nomad job like any other, so its own allocations are " +
			"the place to look next: list_job_allocations on the plugin's job, then " +
			"read_allocation_logs on one that is failing."
	default:
		out.Note = "This plugin is healthy. If a volume it provides still will not mount, the " +
			"problem is more likely the volume's claims or access mode than the plugin — " +
			"diagnose_volume checks both."
	}

	return out
}

// unhealthyInstances picks out the fingerprints that are not healthy.
//
// Only the unhealthy ones are returned. A cluster with two hundred nodes has
// two hundred healthy fingerprints that say nothing, and listing them would
// bury the handful that matter.
func unhealthyInstances(kind string, in map[string]*api.CSIInfo) []pluginInstance {
	var out []pluginInstance
	for nodeID, info := range in {
		if info == nil || info.Healthy {
			continue
		}
		inst := pluginInstance{
			Kind:    kind,
			NodeID:  utils.ShortID(nodeID),
			AllocID: utils.ShortID(info.AllocID),
			Healthy: false,
			Why:     info.HealthDescription,
		}
		if !info.UpdateTime.IsZero() {
			inst.Updated = utils.RelativeAge(info.UpdateTime)
		}
		if inst.Why == "" {
			inst.Why = "the plugin reported no reason; its allocation's logs usually say more"
		}
		out = append(out, inst)
	}
	return out
}

// pluginHealthy reports whether a plugin has everything it is expected to have.
func pluginHealthy(controllerRequired bool, ctrlHealthy, ctrlExpected, nodeHealthy, nodeExpected int) bool {
	if nodeExpected == 0 || nodeHealthy < nodeExpected {
		return false
	}
	if controllerRequired && (ctrlExpected == 0 || ctrlHealthy < ctrlExpected) {
		return false
	}
	return true
}

// pluginProblem says what is missing, in words.
func pluginProblem(controllerRequired bool, ctrlHealthy, ctrlExpected, nodeHealthy, nodeExpected int) string {
	switch {
	case nodeExpected == 0:
		return "no node instances are registered at all, so no allocation can mount a volume from this plugin"
	case controllerRequired && ctrlExpected == 0:
		return "this plugin requires a controller and none is registered, so volumes cannot be attached"
	case controllerRequired && ctrlHealthy < ctrlExpected:
		return itoa(ctrlExpected-ctrlHealthy) + " of " + itoa(ctrlExpected) +
			" controllers are unhealthy, which blocks attaching and detaching volumes cluster-wide"
	case nodeHealthy < nodeExpected:
		return itoa(nodeExpected-nodeHealthy) + " of " + itoa(nodeExpected) +
			" node instances are unhealthy; volumes cannot be mounted on those nodes"
	}
	return ""
}

// ratio formats "healthy/expected" with a word for the zero case.
func ratio(healthy, expected int) string {
	if expected == 0 {
		return "none registered"
	}
	return itoa(healthy) + "/" + itoa(expected) + " healthy"
}

// pluginError explains a failed plugin call.
func pluginError(err error, p *client.Provider, op, namespace string) string {
	msg := utils.MapError(err, utils.ErrorContext{
		Op:         op,
		Kind:       "CSI plugin",
		Namespace:  namespace,
		Address:    p.Address(),
		Capability: "csi-read-volume",
		ListTool:   "list_csi_plugins",
	}, p.Redactor())

	if utils.IsNotFound(err) {
		msg += "\n\nCSI plugins are registered by the jobs that run them, so a plugin only exists " +
			"once its job has placed an allocation that fingerprinted successfully. If the plugin's " +
			"job is running but the plugin is absent here, the plugin task started but never " +
			"registered — its allocation logs are the place to look."
	}
	return msg
}
