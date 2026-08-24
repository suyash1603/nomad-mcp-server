// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

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
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// Autopilot governs the server fleet, not workload placement — that is
// get_scheduler_config. The two are easy to confuse and answer completely
// different questions, so both tools say so in their descriptions.
//
// Durations are rendered as strings ("200ms", "10s") rather than nanosecond
// integers. Nomad's own configuration and documentation use that form, so it is
// what a user will recognise and what a model can quote back at them.

// autopilotConfig is the projection returned by get_autopilot_config.
type autopilotConfig struct {
	CleanupDeadServers      bool   `json:"cleanup_dead_servers"`
	LastContactThreshold    string `json:"last_contact_threshold"`
	MaxTrailingLogs         uint64 `json:"max_trailing_logs"`
	MinQuorum               uint   `json:"min_quorum"`
	ServerStabilizationTime string `json:"server_stabilization_time"`

	EnableRedundancyZones   bool `json:"enable_redundancy_zones"`
	DisableUpgradeMigration bool `json:"disable_upgrade_migration"`
	EnableCustomUpgrades    bool `json:"enable_custom_upgrades"`

	ModifyIndex uint64 `json:"modify_index"`

	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// GetAutopilotConfig reads the cluster's Autopilot configuration.
func GetAutopilotConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_autopilot_config",
			mcp.WithDescription(
				"Read the cluster's Autopilot configuration: whether dead servers are pruned from "+
					"the Raft peer set automatically, how far behind or out of contact a server may "+
					"fall before Autopilot calls it unhealthy, how long a new server must stay "+
					"healthy before it is promoted to a voter, and the minimum number of servers "+
					"Autopilot will leave in place.\n\n"+
					"Autopilot manages the SERVER FLEET and Raft quorum. It is not the scheduler and "+
					"has no effect on where allocations are placed — use get_scheduler_config for "+
					"placement, preemption and the cluster-wide pause switches.\n\n"+
					"Read this when servers were replaced and the cluster still counts the old ones, "+
					"when a newly joined server is not becoming a voter, or when "+
					"get_autopilot_health reports servers as unhealthy and you need to know which "+
					"thresholds produced that verdict. cleanup_dead_servers = false is the usual "+
					"cause of a cluster that lost quorum after a rolling replacement: the departed "+
					"servers still count toward the quorum they can no longer contribute to.\n\n"+
					"Redundancy zones, upgrade migration and custom upgrades are Nomad Enterprise "+
					"features; on Community Edition they read as their defaults."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			cfg, _, err := nomad.Operator().AutopilotGetConfiguration(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the Autopilot configuration",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if cfg == nil {
				return utils.ErrorResult("Nomad returned no Autopilot configuration.")
			}

			out := autopilotConfig{
				CleanupDeadServers:      cfg.CleanupDeadServers,
				LastContactThreshold:    cfg.LastContactThreshold.String(),
				MaxTrailingLogs:         cfg.MaxTrailingLogs,
				MinQuorum:               cfg.MinQuorum,
				ServerStabilizationTime: cfg.ServerStabilizationTime.String(),
				EnableRedundancyZones:   cfg.EnableRedundancyZones,
				DisableUpgradeMigration: cfg.DisableUpgradeMigration,
				EnableCustomUpgrades:    cfg.EnableCustomUpgrades,
				ModifyIndex:             cfg.ModifyIndex,
			}

			if !cfg.CleanupDeadServers {
				out.Warnings = append(out.Warnings,
					"cleanup_dead_servers is OFF. Servers that fail or are decommissioned stay in "+
						"the Raft peer set indefinitely and keep counting toward the quorum they can "+
						"no longer contribute to. After a rolling replacement this is what leaves a "+
						"cluster unable to elect a leader even though enough new servers are running. "+
						"get_autopilot_health lists the peers Nomad currently believes in.")
			}
			if cfg.MinQuorum == 0 {
				out.Warnings = append(out.Warnings,
					"min_quorum is 0, which means Autopilot has no floor below which it will stop "+
						"pruning dead servers. Setting it to the expected number of servers stops "+
						"automatic cleanup from removing peers the cluster still needs for quorum.")
			}
			if cfg.ServerStabilizationTime > time.Minute {
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"server_stabilization_time is %s. A newly joined server stays a NON-VOTER for at "+
						"least that long before promotion, so a server that looks healthy but is not "+
						"yet a voter may simply still be stabilising rather than broken.",
					cfg.ServerStabilizationTime))
			}
			if cfg.DisableUpgradeMigration {
				out.Warnings = append(out.Warnings,
					"disable_upgrade_migration is ON. Autopilot will not hold back promotion until "+
						"enough newer-versioned servers have joined, so a version upgrade will not be "+
						"sequenced for you.")
			}

			out.Note = "Autopilot configuration is per-region and applies to the server fleet. " +
				"get_autopilot_health shows what it currently makes of each server."

			return utils.JSONResult(out)
		},
	}
}

// autopilotHealth is the projection returned by get_autopilot_health.
type autopilotHealth struct {
	Healthy          bool                   `json:"healthy"`
	FailureTolerance int                    `json:"failure_tolerance"`
	Leader           string                 `json:"leader,omitempty"`
	ServerCount      int                    `json:"server_count"`
	VoterCount       int                    `json:"voter_count"`
	Servers          []autopilotServer      `json:"servers"`
	Versions         []string               `json:"versions,omitempty"`
	RedundancyZones  map[string]zoneHealth  `json:"redundancy_zones,omitempty"`
	Upgrade          *autopilotUpgradeState `json:"upgrade,omitempty"`

	// Enterprise-only, and 0 on Community Edition, so it is omitted rather
	// than reported as a tolerance of zero — which would read as an alarm.
	OptimisticFailureTolerance int `json:"optimistic_failure_tolerance,omitempty"`

	Degraded bool     `json:"degraded"`
	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

type autopilotServer struct {
	Name        string `json:"name"`
	ID          string `json:"id,omitempty"`
	Address     string `json:"address,omitempty"`
	Version     string `json:"version,omitempty"`
	Healthy     bool   `json:"healthy"`
	Voter       bool   `json:"voter"`
	Leader      bool   `json:"leader,omitempty"`
	SerfStatus  string `json:"serf_status,omitempty"`
	LastContact string `json:"last_contact,omitempty"`
	LastIndex   uint64 `json:"last_index,omitempty"`
	LastTerm    uint64 `json:"last_term,omitempty"`
	StableSince string `json:"stable_since,omitempty"`
}

type zoneHealth struct {
	Servers          []string `json:"servers,omitempty"`
	Voters           []string `json:"voters,omitempty"`
	FailureTolerance int      `json:"failure_tolerance"`
}

// autopilotUpgradeState is a count-based summary rather than the four server
// name lists the API returns. During an upgrade those lists repeat every server
// name several times over, and the counts are what answer the question.
type autopilotUpgradeState struct {
	Status                 string `json:"status,omitempty"`
	TargetVersion          string `json:"target_version,omitempty"`
	TargetVersionVoters    int    `json:"target_version_voters"`
	TargetVersionNonVoters int    `json:"target_version_non_voters"`
	OtherVersionVoters     int    `json:"other_version_voters"`
	OtherVersionNonVoters  int    `json:"other_version_non_voters"`
}

// GetAutopilotHealth reports Autopilot's view of each server and of quorum.
func GetAutopilotHealth(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_autopilot_health",
			mcp.WithDescription(
				"Report Autopilot's health assessment of the server fleet: whether every server is "+
					"healthy, how many more servers the cluster could lose before it loses quorum, "+
					"which servers are voters, how far behind the leader each one is, and how long "+
					"each has been stable.\n\n"+
					"This is the single most useful call for \"is the control plane in trouble?\". "+
					"failure_tolerance is the number to read first: at 0 the cluster survives no "+
					"further server loss, and the next failure takes the whole cluster down rather "+
					"than degrading it. A server that is healthy but not a voter is usually still "+
					"stabilising after joining; one that is a voter but not healthy is falling "+
					"behind on Raft or out of contact with the leader.\n\n"+
					"This describes SERVERS and Raft quorum only. Client nodes running workloads are "+
					"list_nodes, and cluster-wide placement settings are get_scheduler_config. Use "+
					"get_cluster_status first for the broad picture, then this when the servers "+
					"themselves are the suspects.\n\n"+
					"Redundancy zones and upgrade status appear only on Nomad Enterprise."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			reply, _, err := nomad.Operator().AutopilotServerHealth(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read Autopilot server health",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if reply == nil {
				return utils.ErrorResult(
					"Nomad returned no Autopilot health report. This endpoint is answered by the " +
						"leader, so a cluster with no leader cannot produce one; get_cluster_status " +
						"says whether a leader exists.")
			}

			return utils.JSONResult(buildAutopilotHealth(reply))
		},
	}
}

func buildAutopilotHealth(reply *api.OperatorHealthReply) autopilotHealth {
	out := autopilotHealth{
		Healthy:                    reply.Healthy,
		FailureTolerance:           reply.FailureTolerance,
		Leader:                     reply.Leader,
		ServerCount:                len(reply.Servers),
		OptimisticFailureTolerance: reply.OptimisticFailureTolerance,
		Servers:                    make([]autopilotServer, 0, len(reply.Servers)),
	}

	var unhealthy, stabilising []string
	versions := map[string]bool{}

	for _, s := range reply.Servers {
		if s.Voter {
			out.VoterCount++
		}
		if s.Version != "" {
			versions[s.Version] = true
		}

		srv := autopilotServer{
			Name:       s.Name,
			ID:         s.ID,
			Address:    s.Address,
			Version:    s.Version,
			Healthy:    s.Healthy,
			Voter:      s.Voter,
			Leader:     s.Leader,
			SerfStatus: s.SerfStatus,
			LastIndex:  s.LastIndex,
			LastTerm:   s.LastTerm,
		}
		if s.LastContact > 0 {
			srv.LastContact = s.LastContact.String()
		}
		if !s.StableSince.IsZero() {
			srv.StableSince = utils.FormatTime(s.StableSince.UnixNano())
		}
		out.Servers = append(out.Servers, srv)

		switch {
		case !s.Healthy:
			unhealthy = append(unhealthy, s.Name)
		case !s.Voter:
			// Healthy but not voting. On Enterprise this may be a deliberate
			// read replica, which the caller can tell apart from the
			// read_replicas list; on Community it means still stabilising.
			stabilising = append(stabilising, s.Name)
		}
	}

	for v := range versions {
		out.Versions = append(out.Versions, v)
	}
	sort.Strings(out.Versions)

	if len(reply.RedundancyZones) > 0 {
		out.RedundancyZones = make(map[string]zoneHealth, len(reply.RedundancyZones))
		for name, z := range reply.RedundancyZones {
			out.RedundancyZones[name] = zoneHealth{
				Servers:          z.Servers,
				Voters:           z.Voters,
				FailureTolerance: z.FailureTolerance,
			}
		}
	}

	if u := reply.Upgrade; u != nil {
		out.Upgrade = &autopilotUpgradeState{
			Status:                 u.Status,
			TargetVersion:          u.TargetVersion,
			TargetVersionVoters:    len(u.TargetVersionVoters),
			TargetVersionNonVoters: len(u.TargetVersionNonVoters),
			OtherVersionVoters:     len(u.OtherVersionVoters),
			OtherVersionNonVoters:  len(u.OtherVersionNonVoters),
		}
	}

	// A cluster that can lose no further server without an outage is the
	// finding worth leading with, even when every server is currently healthy.
	if reply.FailureTolerance <= 0 {
		out.Degraded = true
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"failure_tolerance is %d: the cluster cannot lose another server without losing "+
				"quorum, and the next server failure is an outage rather than a degradation. "+
				"Quorum needs a majority of voters, so tolerance only rises with an ODD number "+
				"of servers — going from 3 to 4 adds no tolerance at all, 3 to 5 does.",
			reply.FailureTolerance))
	}

	if len(unhealthy) > 0 {
		out.Degraded = true
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"Autopilot considers %d of %d servers UNHEALTHY: %v. A server is unhealthy when its "+
				"Serf status is not alive, when it has been out of contact with the leader for "+
				"longer than last_contact_threshold, or when it trails the leader's Raft log by "+
				"more than max_trailing_logs. get_autopilot_config shows those thresholds; the "+
				"last_contact and last_index fields above show which one this server tripped.",
			len(unhealthy), len(reply.Servers), unhealthy))
	}

	if len(stabilising) > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%v are healthy but are NOT voters, so they do not count toward quorum. A recently "+
				"joined server stays a non-voter until it has been healthy for "+
				"server_stabilization_time; if it has been far longer than that, it is not being "+
				"promoted and get_autopilot_config is the place to look. On Enterprise a "+
				"permanent non-voter may instead be a deliberate read replica.",
			stabilising))
	}

	if reply.Leader == "" {
		out.Degraded = true
		out.Warnings = append(out.Warnings,
			"Autopilot reports no leader. Nothing can be scheduled and no cluster-wide "+
				"configuration can be written until one is elected.")
	}

	if len(out.Versions) > 1 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"The servers are running %d different Nomad versions (%v). That is expected part-way "+
				"through a rolling upgrade and a problem if it was not intended; a mixed fleet "+
				"left in place indefinitely is how an interrupted upgrade hides.",
			len(out.Versions), out.Versions))
	}

	out.Note = "This covers Nomad servers and Raft quorum only, not the client nodes that run " +
		"workloads. The thresholds behind each verdict are in get_autopilot_config."

	return out
}

// SetAutopilotConfig changes the cluster's Autopilot configuration.
func SetAutopilotConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("set_autopilot_config",
			mcp.WithDescription(
				"Change the cluster's Autopilot configuration.\n\n"+
					"Autopilot decides which servers count toward Raft quorum and which are removed "+
					"from it, so a mistake here can cost the cluster its control plane rather than "+
					"one workload. Turning cleanup_dead_servers ON gives Autopilot permission to "+
					"REMOVE servers from the Raft peer set: that is exactly right for pruning "+
					"replaced servers, and exactly wrong if the servers are alive but temporarily "+
					"unreachable, because it will evict peers the cluster still needs and can drop "+
					"it below quorum.\n\n"+
					"Loosening the health thresholds makes Autopilot slower to notice a genuinely "+
					"failing server; tightening them makes it call healthy servers unhealthy over "+
					"ordinary network jitter. Neither failure is visible until it matters.\n\n"+
					"Only the settings you pass are changed; anything omitted keeps its current "+
					"value. Read the current state with get_autopilot_config first, check "+
					"get_autopilot_health to see what the fleet looks like before the change, show "+
					"the user exactly what will change, and get explicit confirmation.\n\n"+
					"This is the SERVER FLEET, not the scheduler. Placement, preemption and the "+
					"cluster-wide pause switches are set_scheduler_config.\n\n"+
					"Redundancy zones, upgrade migration and custom upgrades require Nomad "+
					"Enterprise."),
			// Destructive: cleanup_dead_servers removes peers from the Raft
			// configuration, and the health thresholds decide who survives it.
			utils.MutatingTool(true, true),
			mcp.WithBoolean("cleanup_dead_servers",
				mcp.Description(
					"Allow Autopilot to remove failed servers from the Raft peer set automatically. "+
						"Turning this ON prunes servers that are genuinely gone, and evicts servers "+
						"that are merely unreachable."),
			),
			mcp.WithString("last_contact_threshold",
				mcp.Description(
					"How long a server may go without contacting the leader before Autopilot calls "+
						"it unhealthy, as a duration string such as \"200ms\" or \"5s\". Nomad's "+
						"default is 200ms."),
			),
			mcp.WithString("server_stabilization_time",
				mcp.Description(
					"How long a newly joined server must stay healthy before Autopilot promotes it "+
						"to a voter, as a duration string such as \"10s\" or \"2m\". Until then it "+
						"does not count toward quorum. Nomad's default is 10s."),
			),
			mcp.WithNumber("max_trailing_logs",
				mcp.Description(
					"How many Raft log entries a server may fall behind the leader before Autopilot "+
						"calls it unhealthy. Nomad's default is 250."),
			),
			mcp.WithNumber("min_quorum",
				mcp.Description(
					"The number of servers below which Autopilot will stop pruning dead ones. Set "+
						"this to the number of servers the cluster is supposed to have; it is the "+
						"floor that stops automatic cleanup from removing peers quorum still needs."),
			),
			mcp.WithBoolean("enable_redundancy_zones",
				mcp.Description(
					"Spread voters across redundancy zones using each server's redundancy_zone "+
						"metadata. Requires Nomad Enterprise."),
			),
			mcp.WithBoolean("disable_upgrade_migration",
				mcp.Description(
					"When true, Autopilot will not hold back promoting new servers until enough "+
						"newer-versioned ones have joined, so it will not sequence an upgrade for "+
						"you. Requires Nomad Enterprise."),
			),
			mcp.WithBoolean("enable_custom_upgrades",
				mcp.Description(
					"Use each server's upgrade_version metadata instead of its Nomad version when "+
						"sequencing an upgrade migration. Requires Nomad Enterprise."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))

			// The API replaces the whole document, so the current state is read
			// first and only the named fields are overwritten. Without this,
			// omitting an argument would silently reset that setting — and the
			// settings here decide who stays in the Raft peer set.
			cfg, _, err := nomad.Operator().AutopilotGetConfiguration(&api.QueryOptions{Region: region})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the current Autopilot configuration",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if cfg == nil {
				return utils.ErrorResult(
					"Nomad returned no current Autopilot configuration, so it cannot be safely " +
						"modified: a write would have to guess at the settings not being changed.")
			}

			args := req.GetArguments()
			changes := map[string]any{}

			setBool := func(arg string, field *bool) {
				if _, ok := args[arg]; !ok {
					return
				}
				v := req.GetBool(arg, *field)
				if *field != v {
					changes[arg] = boolArrow(*field, v)
				}
				*field = v
			}
			setBool("cleanup_dead_servers", &cfg.CleanupDeadServers)
			setBool("enable_redundancy_zones", &cfg.EnableRedundancyZones)
			setBool("disable_upgrade_migration", &cfg.DisableUpgradeMigration)
			setBool("enable_custom_upgrades", &cfg.EnableCustomUpgrades)

			// Durations arrive as strings because that is the form Nomad's own
			// configuration uses; a bare number would be ambiguous about units.
			setDuration := func(arg string, field *time.Duration) error {
				if _, ok := args[arg]; !ok {
					return nil
				}
				raw := req.GetString(arg, "")
				v, err := time.ParseDuration(raw)
				if err != nil {
					return fmt.Errorf(
						"invalid %s %q: it must be a duration string such as \"200ms\", \"5s\" or "+
							"\"2m\", not a bare number", arg, raw)
				}
				if v <= 0 {
					return fmt.Errorf(
						"invalid %s %q: it must be greater than zero. A threshold of zero would make "+
							"Autopilot judge every server unhealthy immediately", arg, raw)
				}
				if *field != v {
					changes[arg] = field.String() + " -> " + v.String()
				}
				*field = v
				return nil
			}
			if err := setDuration("last_contact_threshold", &cfg.LastContactThreshold); err != nil {
				return utils.ErrorResult(capitalise(err.Error()) + ".")
			}
			if err := setDuration("server_stabilization_time", &cfg.ServerStabilizationTime); err != nil {
				return utils.ErrorResult(capitalise(err.Error()) + ".")
			}

			if _, ok := args["max_trailing_logs"]; ok {
				v := req.GetInt("max_trailing_logs", int(cfg.MaxTrailingLogs))
				if v <= 0 {
					return utils.ErrorResultf(
						"Invalid max_trailing_logs %d: it must be greater than zero. At zero, any "+
							"server that is even one entry behind the leader is judged unhealthy.", v)
				}
				if cfg.MaxTrailingLogs != uint64(v) {
					changes["max_trailing_logs"] = fmt.Sprintf("%d -> %d", cfg.MaxTrailingLogs, v)
				}
				cfg.MaxTrailingLogs = uint64(v)
			}

			if _, ok := args["min_quorum"]; ok {
				v := req.GetInt("min_quorum", int(cfg.MinQuorum))
				if v < 0 {
					return utils.ErrorResultf(
						"Invalid min_quorum %d: it cannot be negative.", v)
				}
				if cfg.MinQuorum != uint(v) {
					changes["min_quorum"] = fmt.Sprintf("%d -> %d", cfg.MinQuorum, v)
				}
				cfg.MinQuorum = uint(v)
			}

			if len(changes) == 0 {
				return utils.JSONResult(map[string]any{
					"changed": false,
					"note": "Nothing was changed: every setting given already had the value " +
						"requested, or no setting was given. get_autopilot_config shows the current " +
						"configuration.",
				})
			}

			// A compare-and-swap on the modify index turns a concurrent change
			// by another operator into a refusal rather than a silent overwrite
			// of settings this call never intended to touch.
			updated, _, err := nomad.Operator().AutopilotCASConfiguration(cfg, &api.WriteOptions{
				Region: region,
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "update the Autopilot configuration",
					Address:    p.Address(),
					Capability: "operator:write",
				}, p.Redactor()))
			}
			if !updated {
				return utils.ErrorResult(
					"The Autopilot configuration was NOT updated: it changed underneath this call, " +
						"so the write was refused rather than overwriting whatever the other change " +
						"was. Read it again with get_autopilot_config and retry if the change is " +
						"still wanted.")
			}

			out := map[string]any{
				"changed": true,
				"changes": changes,
				"note": "Autopilot configuration updated for this region. Confirm with " +
					"get_autopilot_config, and check get_autopilot_health afterwards — these " +
					"settings change which servers Autopilot considers healthy and which count " +
					"toward quorum.",
			}

			var warnings []string
			if cfg.CleanupDeadServers {
				warnings = append(warnings,
					"cleanup_dead_servers is now ON: Autopilot may remove servers from the Raft peer "+
						"set without further confirmation. Check get_autopilot_health now — a server "+
						"that is unreachable rather than genuinely gone is a candidate for eviction.")
			}
			if cfg.MinQuorum > 0 && cfg.CleanupDeadServers {
				warnings = append(warnings, fmt.Sprintf(
					"Autopilot will not prune below min_quorum = %d servers.", cfg.MinQuorum))
			}
			if len(warnings) > 0 {
				out["warnings"] = warnings
			}

			return utils.JSONResult(out)
		},
	}
}

// capitalise upper-cases the first letter, so a lower-case Go error reads as a
// sentence in a tool result.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
