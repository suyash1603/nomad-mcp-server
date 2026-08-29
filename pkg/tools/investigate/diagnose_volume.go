// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

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

// volumeDiagnosis is the tool's output.
type volumeDiagnosis struct {
	VolumeID    string    `json:"volume_id"`
	Name        string    `json:"name,omitempty"`
	Namespace   string    `json:"namespace"`
	Plugin      string    `json:"plugin_id,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Schedulable bool      `json:"schedulable"`
	AccessMode  string    `json:"access_mode,omitempty"`
	Findings    []finding `json:"findings"`
	Claims      []claim   `json:"claims,omitempty"`
	ClaimCount  int       `json:"claim_count"`
	PluginNodes string    `json:"plugin_node_instances,omitempty"`
	PluginCtrl  string    `json:"plugin_controllers,omitempty"`
	Healthy     bool      `json:"looks_healthy"`
	Note        string    `json:"note,omitempty"`
}

// claim is one allocation holding this volume.
type claim struct {
	AllocID string `json:"alloc_id"`
	Mode    string `json:"mode"`
	Job     string `json:"job_id,omitempty"`
	Node    string `json:"node,omitempty"`
	Status  string `json:"client_status,omitempty"`
	Stale   bool   `json:"stale,omitempty"`
}

// DiagnoseVolume explains why a volume cannot be used.
func DiagnoseVolume(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("diagnose_volume",
			mcp.WithDescription(
				"Explain why a storage volume cannot be used, by following the whole chain in one "+
					"call: the volume, the health of the CSI plugin behind it, which allocations "+
					"hold claims on it, whether those allocations are still alive, which nodes they "+
					"are on, and whether the plugin is healthy on those specific nodes.\n\n"+
					"Reach for this whenever a job that mounts a volume will not place, or an "+
					"allocation is stuck pending with a volume in its group. Doing it by hand takes "+
					"read_volume, list_csi_plugins, read_csi_plugin, list_allocations and "+
					"read_allocation, and the answer is usually in the relationship between them "+
					"rather than in any one.\n\n"+
					"It specifically detects STALE CLAIMS — a volume still held by an allocation "+
					"that is dead. That is the most common cause of a volume that looks fine and "+
					"will not attach, and nothing in read_volume makes it visible.\n\n"+
					"Works for CSI volumes. Dynamic host volumes have no plugin and no claims, so "+
					"for those this reports what it can and says so."),
			utils.ReadOnlyTool(),
			mcp.WithString("volume_id",
				mcp.Required(),
				mcp.Description("The volume's ID, exactly as returned by list_volumes."),
			),
			mcp.WithString("type",
				mcp.DefaultString("csi"),
				mcp.Enum("csi", "host"),
				mcp.Description(
					"Which kind of volume this is. Nomad keeps CSI and host volumes in separate "+
						"systems; a volume that exists as one will not be found as the other."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return diagnoseVolume(ctx, req, p)
		},
	}
}

func diagnoseVolume(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	volumeID, err := req.RequireString("volume_id")
	if err != nil {
		return utils.ErrorResult(
			"The 'volume_id' argument is required. Use list_volumes to see what exists.")
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
	q := &api.QueryOptions{Namespace: namespace, Region: region}

	if req.GetString("type", "csi") == "host" {
		return diagnoseHostVolume(nomad, p, volumeID, namespace, q)
	}

	vol, _, err := nomad.CSIVolumes().Info(volumeID, q)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read CSI volume " + volumeID,
			Kind:       "volume",
			Name:       volumeID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "csi-read-volume",
			ListTool:   "list_volumes",
		}, p.Redactor()))
	}

	out := volumeDiagnosis{
		VolumeID:    vol.ID,
		Name:        vol.Name,
		Namespace:   orDefault(vol.Namespace, namespace),
		Plugin:      vol.PluginID,
		Provider:    vol.Provider,
		Schedulable: vol.Schedulable,
		AccessMode:  string(vol.AccessMode),
		PluginNodes: ratio(vol.NodesHealthy, vol.NodesExpected),
	}
	if vol.ControllerRequired || vol.ControllersExpected > 0 {
		out.PluginCtrl = ratio(vol.ControllersHealthy, vol.ControllersExpected)
	}

	out.Claims, out.ClaimCount = collectClaims(vol)
	out.Findings = volumeFindings(vol, out.Claims)

	sortFindings(out.Findings)
	out.Healthy = len(out.Findings) == 0
	out.Note = volumeNote(out, vol)

	return utils.JSONResult(out)
}

// collectClaims turns the volume's reader and writer maps into one list.
//
// The maps are keyed by allocation ID with nil values — Nomad documents that
// the Allocation is never populated there — so the live detail comes from the
// separate Allocations slice, which is what makes stale claims detectable at
// all: a claim whose allocation is missing from that slice, or present but
// terminal, is holding a volume it will never release on its own.
func collectClaims(vol *api.CSIVolume) ([]claim, int) {
	alive := map[string]*api.AllocationListStub{}
	for _, a := range vol.Allocations {
		if a != nil {
			alive[a.ID] = a
		}
	}

	var out []claim
	add := func(ids map[string]*api.Allocation, mode string) {
		for id := range ids {
			c := claim{AllocID: utils.ShortID(id), Mode: mode}
			if a, ok := alive[id]; ok {
				c.Job = a.JobID
				c.Node = a.NodeName
				c.Status = a.ClientStatus
				c.Stale = isTerminalAlloc(a.ClientStatus)
			} else {
				// The claim exists but the allocation is gone entirely.
				c.Status = "gone"
				c.Stale = true
			}
			out = append(out, c)
		}
	}
	add(vol.ReadAllocs, "read")
	add(vol.WriteAllocs, "write")

	sort.Slice(out, func(i, j int) bool {
		if out[i].Stale != out[j].Stale {
			return out[i].Stale
		}
		return out[i].AllocID < out[j].AllocID
	})
	return out, len(out)
}

// volumeFindings ranks what is wrong with the volume.
func volumeFindings(vol *api.CSIVolume, claims []claim) []finding {
	var out []finding

	var stale []string
	for _, c := range claims {
		if c.Stale {
			stale = append(stale, c.AllocID)
		}
	}

	if len(stale) > 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "stale-claims",
			Count:    len(stale),
			Summary: fmt.Sprintf("%d claim%s held by allocations that are no longer running",
				len(stale), plural(len(stale))),
			Examples: stale,
			Detail: "A volume with a single-writer access mode cannot be claimed again while a " +
				"stale claim is held, so a new allocation will sit pending forever with nothing " +
				"obviously wrong. Nomad normally releases these itself; one that persists usually " +
				"means the node went away without the controller being able to detach.",
			NextStep: "Check whether the claiming allocation's node is down with list_nodes. " +
				"Stopping the job that held the claim, or detaching the volume with the nomad CLI, " +
				"is what releases it — this server has no tool that does so.",
		})
	}

	if !vol.Schedulable {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "not-schedulable",
			Count:    1,
			Summary:  "Nomad has marked this volume unschedulable",
			Detail: "Schedulable is derived from plugin health, not from the volume itself. " +
				"While it is false, any job asking for this volume will fail to place with a " +
				"constraint error that does not mention storage.",
			NextStep: "read_csi_plugin on " + vol.PluginID + " for which instances are unhealthy.",
		})
	}

	if vol.NodesExpected == 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "no-node-plugin",
			Count:    1,
			Summary:  "the plugin has no node instances registered, so nothing can mount this volume",
			NextStep: "list_csi_plugins, then check the plugin's own job is running.",
		})
	} else if vol.NodesHealthy < vol.NodesExpected {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "degraded-plugin-nodes",
			Count:    vol.NodesExpected - vol.NodesHealthy,
			Summary: fmt.Sprintf("%d of %d plugin node instances are unhealthy",
				vol.NodesExpected-vol.NodesHealthy, vol.NodesExpected),
			Detail: "This volume can only be mounted on nodes where the plugin is healthy. " +
				"Placement will fail on the others without naming storage as the reason.",
			NextStep: "read_csi_plugin on " + vol.PluginID + " to see which nodes are affected.",
		})
	}

	if vol.ControllerRequired && vol.ControllersHealthy == 0 {
		out = append(out, finding{
			sev:      sevCritical,
			Category: "no-healthy-controller",
			Count:    1,
			Summary:  "this volume needs a controller plugin and none is healthy",
			Detail:   "Without a controller the volume cannot be attached or detached anywhere.",
			NextStep: "read_csi_plugin on " + vol.PluginID + ".",
		})
	}

	// A single-writer volume with more than one writer claim should be
	// impossible, so if it is observed it is worth saying loudly.
	if len(vol.WriteAllocs) > 1 && strings.Contains(string(vol.AccessMode), "single-node-writer") {
		out = append(out, finding{
			sev:      sevWarning,
			Category: "conflicting-claims",
			Count:    len(vol.WriteAllocs),
			Summary:  "more than one writer claim on a single-writer volume",
			NextStep: "Expect placement of any further writer to fail until the extra claims clear.",
		})
	}

	return out
}

// volumeNote summarises the diagnosis.
func volumeNote(d volumeDiagnosis, vol *api.CSIVolume) string {
	if len(d.Findings) == 0 {
		note := "No problem found with this volume: it is schedulable, its plugin is healthy, and " +
			"no claim is stale."
		if d.ClaimCount > 0 {
			note += fmt.Sprintf(" It currently has %d live claim%s.", d.ClaimCount, plural(d.ClaimCount))
		}
		return note + " If a job still will not place, the reason is more likely elsewhere — " +
			"read_evaluation names the constraint that actually filtered the nodes out."
	}

	lead := fmt.Sprintf("%d finding%s, most severe first.", len(d.Findings), plural(len(d.Findings)))
	if vol.AccessMode != "" {
		lead += " Access mode is " + string(vol.AccessMode) +
			", which is what decides how many allocations may hold this volume at once."
	}
	return lead
}

// diagnoseHostVolume handles the host-volume case, which has no plugin.
func diagnoseHostVolume(nomad *api.Client, p *client.Provider, volumeID, namespace string, q *api.QueryOptions) (*mcp.CallToolResult, error) {
	vol, _, err := nomad.HostVolumes().Get(volumeID, q)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read host volume " + volumeID,
			Kind:       "volume",
			Name:       volumeID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "host-volume-read",
			ListTool:   "list_volumes",
		}, p.Redactor()))
	}

	out := volumeDiagnosis{
		VolumeID:  vol.ID,
		Name:      vol.Name,
		Namespace: orDefault(vol.Namespace, namespace),
		Findings:  []finding{},
	}

	if string(vol.State) != "ready" {
		out.Findings = append(out.Findings, finding{
			sev:      sevCritical,
			Category: "host-volume-not-ready",
			Count:    1,
			Summary:  "this host volume is in state " + string(vol.State) + " rather than ready",
			NextStep: "read_volume for the full state, and read_node on " + utils.ShortID(vol.NodeID) + ".",
		})
	}

	sortFindings(out.Findings)
	out.Healthy = len(out.Findings) == 0
	out.Note = "This is a dynamic host volume. It lives on one specific node (" +
		utils.ShortID(vol.NodeID) + ") and has no CSI plugin and no claim tracking, so the plugin " +
		"and stale-claim checks do not apply. A job can only use it by being placed on that node: " +
		"if the node is down, draining or ineligible, the job will not place and the volume is why."

	return utils.JSONResult(out)
}

// isTerminalAlloc reports whether an allocation has stopped for good.
func isTerminalAlloc(status string) bool {
	switch status {
	case "complete", "failed", "lost":
		return true
	}
	return false
}

// ratio formats "healthy/expected" with a word for the zero case.
func ratio(healthy, expected int) string {
	if expected == 0 {
		return "none registered"
	}
	return fmt.Sprintf("%d/%d healthy", healthy, expected)
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
