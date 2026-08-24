// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

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

// The Raft peer set is the ground truth beneath get_autopilot_health: Autopilot
// reports on the servers it knows about, and this reports on the entries Raft
// actually counts for quorum. The two differ in exactly the case that matters —
// a server that is gone but was never removed from the configuration still
// counts toward the quorum it can no longer contribute to.
//
// Nomad marks such an entry's node name "(unknown)", which is the single most
// diagnostic field here and the reason get_raft_config exists as a separate
// tool rather than being folded into the Autopilot projection.

// unknownNode is what Nomad puts in RaftServer.Node when it has no member
// matching a peer in the Raft configuration.
const unknownNode = "(unknown)"

// raftConfig is the projection returned by get_raft_config.
type raftConfig struct {
	Index            uint64     `json:"index"`
	Leader           string     `json:"leader,omitempty"`
	ServerCount      int        `json:"server_count"`
	VoterCount       int        `json:"voter_count"`
	Quorum           int        `json:"quorum_required"`
	FailureTolerance int        `json:"failure_tolerance"`
	Servers          []raftPeer `json:"servers"`

	Degraded bool     `json:"degraded"`
	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

type raftPeer struct {
	ID           string `json:"id"`
	Node         string `json:"node,omitempty"`
	Address      string `json:"address,omitempty"`
	Leader       bool   `json:"leader,omitempty"`
	Voter        bool   `json:"voter"`
	RaftProtocol string `json:"raft_protocol,omitempty"`
	Orphaned     bool   `json:"orphaned,omitempty"`
}

// GetRaftConfig reads the Raft peer configuration.
func GetRaftConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_raft_config",
			mcp.WithDescription(
				"Read the Raft peer configuration: every server entry that counts toward quorum, "+
					"which of them vote, which is the leader, and how many servers the cluster can "+
					"lose before it stops being able to elect one.\n\n"+
					"This is the ground truth beneath get_autopilot_health. Autopilot reports on the "+
					"servers it knows about; this reports on the entries Raft actually counts. They "+
					"differ in the case that matters most: a server that was destroyed or replaced "+
					"but never removed from the configuration still counts toward quorum while "+
					"contributing nothing to it. Nomad shows those entries with the node name "+
					"\"(unknown)\", and this tool flags them as orphaned.\n\n"+
					"Reach for this when a cluster cannot elect a leader despite enough servers "+
					"running, after servers have been replaced, or when get_autopilot_health reports "+
					"a failure tolerance lower than the server count suggests it should be. Three "+
					"live servers plus two orphaned entries is a five-peer cluster with a quorum of "+
					"three that will lose its leader the moment one live server restarts.\n\n"+
					"remove_raft_peer removes an orphaned entry."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			cfg, err := nomad.Operator().RaftGetConfiguration(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the Raft configuration",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if cfg == nil {
				return utils.ErrorResult("Nomad returned no Raft configuration.")
			}

			return utils.JSONResult(buildRaftConfig(cfg))
		},
	}
}

func buildRaftConfig(cfg *api.RaftConfiguration) raftConfig {
	out := raftConfig{
		Index:       cfg.Index,
		ServerCount: len(cfg.Servers),
		Servers:     make([]raftPeer, 0, len(cfg.Servers)),
	}

	var orphaned []string
	oldProtocol := map[string]bool{}

	for _, s := range cfg.Servers {
		if s == nil {
			continue
		}
		if s.Voter {
			out.VoterCount++
		}
		if s.Leader {
			out.Leader = s.Node
			if out.Leader == "" || out.Leader == unknownNode {
				out.Leader = s.Address
			}
		}

		peer := raftPeer{
			ID:           s.ID,
			Node:         s.Node,
			Address:      s.Address,
			Leader:       s.Leader,
			Voter:        s.Voter,
			RaftProtocol: s.RaftProtocol,
			Orphaned:     s.Node == unknownNode,
		}
		out.Servers = append(out.Servers, peer)

		if peer.Orphaned {
			orphaned = append(orphaned, describePeer(s))
		}
		// Removing a peer by ID and transferring leadership both require Raft
		// protocol 3. On anything older the write tools cannot work.
		if s.RaftProtocol != "" && s.RaftProtocol != "3" {
			oldProtocol[s.RaftProtocol] = true
		}
	}

	// Quorum is a majority of the voters in the configuration, whether or not
	// those voters still exist. That is the whole reason an orphaned entry is
	// dangerous rather than merely untidy.
	out.Quorum = out.VoterCount/2 + 1
	out.FailureTolerance = out.VoterCount - out.Quorum

	if len(orphaned) > 0 {
		out.Degraded = true
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d of %d Raft peers are ORPHANED — they are in the configuration and count toward "+
				"quorum, but Nomad has no server matching them: %s. Quorum here is %d of %d "+
				"voters, and those entries can never vote, so the cluster is closer to losing its "+
				"leader than the server count suggests. remove_raft_peer removes one; "+
				"get_autopilot_config says whether cleanup_dead_servers would have done it "+
				"automatically.",
			len(orphaned), len(cfg.Servers), strings.Join(orphaned, ", "),
			out.Quorum, out.VoterCount))
	}

	if out.Leader == "" {
		out.Degraded = true
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"The Raft configuration has NO LEADER. Nothing can be scheduled and no cluster-wide "+
				"configuration can be written. Electing one needs %d of the %d voters reachable "+
				"and able to talk to each other.",
			out.Quorum, out.VoterCount))
	}

	if out.FailureTolerance <= 0 && out.VoterCount > 0 {
		out.Degraded = true
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"With %d voters the quorum is %d, so the cluster tolerates no further loss. The next "+
				"voter to go takes the control plane with it.",
			out.VoterCount, out.Quorum))
	} else if out.VoterCount%2 == 0 && out.VoterCount > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"There are %d voters, an even number. Quorum is %d either way, so the %dth voter adds "+
				"no fault tolerance over %d — it only adds another machine that can fail. An odd "+
				"voter count is what buys tolerance.",
			out.VoterCount, out.Quorum, out.VoterCount, out.VoterCount-1))
	}

	if len(oldProtocol) > 0 {
		versions := make([]string, 0, len(oldProtocol))
		for v := range oldProtocol {
			versions = append(versions, v)
		}
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"Some peers speak Raft protocol %s rather than 3. remove_raft_peer and "+
				"transfer_leadership both need protocol 3 and will be refused by Nomad below it.",
			strings.Join(versions, ", ")))
	}

	out.Note = "Quorum is a majority of the VOTERS listed here, including any that no longer " +
		"exist. get_autopilot_health gives the same fleet as Autopilot sees it, with per-server " +
		"health."

	return out
}

// describePeer names a peer the way a human would refer to it, since an
// orphaned entry has no useful node name by definition.
func describePeer(s *api.RaftServer) string {
	switch {
	case s.Node != "" && s.Node != unknownNode:
		return s.Node + " (" + s.Address + ")"
	case s.Address != "":
		return s.Address
	default:
		return s.ID
	}
}

// RemoveRaftPeer removes a server from the Raft configuration.
func RemoveRaftPeer(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("remove_raft_peer",
			mcp.WithDescription(
				"Remove a server from the Raft peer configuration, so that it stops counting "+
					"toward quorum.\n\n"+
					"This is for a server that is GONE — destroyed, decommissioned, or replaced — "+
					"and whose entry Nomad never cleaned up. get_raft_config flags those as "+
					"orphaned. Removing one lowers the quorum requirement and can be what lets a "+
					"cluster elect a leader again.\n\n"+
					"Removing a peer that still exists is the dangerous direction, and it is not "+
					"undone by this tool: the server has to rejoin. Removing enough live voters "+
					"costs the cluster quorum outright, at which point no tool here can fix it and "+
					"recovery means a manual peers.json on the remaining servers. This tool refuses "+
					"to remove a peer that Autopilot currently reports as healthy, and refuses to "+
					"remove the leader — transfer_leadership moves leadership away first.\n\n"+
					"Read get_raft_config and get_autopilot_health first, show the user which entry "+
					"is going and what the quorum will be afterwards, and get explicit "+
					"confirmation. If the servers are alive and the entries are stale only because "+
					"cleanup_dead_servers is off, set_autopilot_config is the better fix.\n\n"+
					"Requires Raft protocol 3."),
			// Destructive in the strongest sense available here: it changes who
			// counts toward quorum, and the bad direction is not recoverable
			// through this server.
			utils.MutatingTool(true, true),
			mcp.WithString("peer_id",
				mcp.Required(),
				mcp.Description(
					"The peer to remove, as the \"id\" shown by get_raft_config. Its Raft address "+
						"(\"10.0.0.4:4647\") is also accepted, which is how an orphaned entry with no "+
						"node name is usually identified."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerID, err := req.RequireString("peer_id")
			if err != nil {
				return utils.ErrorResult(
					"The 'peer_id' argument is required. Use get_raft_config to see the peers.")
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := &api.QueryOptions{Region: region}

			cfg, err := nomad.Operator().RaftGetConfiguration(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the Raft configuration before removing a peer",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}

			target := findPeer(cfg, peerID)
			if target == nil {
				return utils.ErrorResult(noSuchPeer(cfg, peerID))
			}

			if target.Leader {
				return utils.ErrorResultf(
					"Refused: %s is the current LEADER.\n\n"+
						"Removing the leader from its own Raft configuration forces an election and "+
						"can leave the cluster without one. Use transfer_leadership to move "+
						"leadership to another voter first, confirm with get_raft_config that it "+
						"moved, and then remove this peer.",
					describePeer(target))
			}

			// The guard that matters. Autopilot's health view is best-effort
			// here on purpose: it is answered by the leader, so it fails in
			// exactly the situation where removing a dead peer is the fix.
			// A failure to check must not block the repair.
			healthNote := ""
			if reply, _, err := nomad.Operator().AutopilotServerHealth(q); err == nil && reply != nil {
				for _, s := range reply.Servers {
					if !samePeer(target, s.ID, s.Address, s.Name) {
						continue
					}
					if s.Healthy {
						return utils.ErrorResultf(
							"Refused: %s is reported HEALTHY by Autopilot right now, which means it "+
								"is alive and in contact with the leader.\n\n"+
								"Removing a live server from the Raft configuration does not stop it; "+
								"it strips its vote and shrinks the quorum base, and the server has to "+
								"rejoin the cluster to come back. This tool is for entries whose "+
								"server no longer exists.\n\n"+
								"If the intent was to decommission this server, stop its agent first "+
								"and remove the peer once it is gone. If the intent was to clear "+
								"stale entries automatically, set_autopilot_config can turn on "+
								"cleanup_dead_servers instead.",
							describePeer(target))
					}
					healthNote = "Autopilot reports this server as unhealthy."
				}
			} else {
				healthNote = "Autopilot's health view could not be read, so this removal was not " +
					"cross-checked against it. That is expected when the cluster has no leader — " +
					"which is also when removing a dead peer is the repair."
			}

			// State the quorum arithmetic before and after, since that is the
			// thing the caller is actually deciding about.
			votersBefore, votersAfter := 0, 0
			for _, s := range cfg.Servers {
				if s == nil || !s.Voter {
					continue
				}
				votersBefore++
				if !samePeer(target, s.ID, s.Address, s.Node) {
					votersAfter++
				}
			}

			if err := nomad.Operator().RaftRemovePeerByID(target.ID, &api.WriteOptions{
				Region: region,
			}); err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "remove Raft peer " + describePeer(target),
					Address:    p.Address(),
					Capability: "operator:write",
				}, p.Redactor()))
			}

			out := map[string]any{
				"removed":       describePeer(target),
				"peer_id":       target.ID,
				"was_voter":     target.Voter,
				"was_orphaned":  target.Node == unknownNode,
				"voters_before": votersBefore,
				"voters_after":  votersAfter,
				"quorum_before": votersBefore/2 + 1,
				"quorum_after":  votersAfter/2 + 1,
				"note": "The peer was removed from the Raft configuration. Confirm with " +
					"get_raft_config. If that server is ever started again it will have to rejoin " +
					"the cluster.",
			}
			if healthNote != "" {
				out["health_check"] = healthNote
			}
			if votersAfter > 0 && votersAfter%2 == 0 {
				out["warning"] = fmt.Sprintf(
					"There are now %d voters, an even number, which buys no more fault tolerance "+
						"than %d. Consider bringing the fleet back to an odd count.",
					votersAfter, votersAfter-1)
			}

			return utils.JSONResult(out)
		},
	}
}

// TransferLeadership moves Raft leadership to another server.
func TransferLeadership(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("transfer_leadership",
			mcp.WithDescription(
				"Move Raft leadership to a specific server.\n\n"+
					"Use this before taking the current leader out of service — a restart, an "+
					"upgrade, or a host that is about to be replaced. Handing leadership over "+
					"deliberately is quicker and quieter than letting the leader disappear and "+
					"waiting for the cluster to notice and elect a replacement.\n\n"+
					"It is not free. There is a brief window during the handover when the cluster "+
					"has no leader, so writes and scheduling stall for a moment. On a healthy "+
					"cluster this is short; on one that is already struggling to hold an election "+
					"it can be considerably longer, and doing this to a degraded cluster can make "+
					"things worse rather than better. Read get_raft_config and get_autopilot_health "+
					"first and do not transfer leadership on a cluster that is already at zero "+
					"failure tolerance unless that is specifically the plan.\n\n"+
					"Name the current leader and the intended one, say that scheduling will pause "+
					"briefly, and get explicit confirmation before calling this. It is not a "+
					"read-only probe and should never be used to \"check\" anything.\n\n"+
					"The target must be a healthy voter. Requires Raft protocol 3."),
			// Destructive: the handover interrupts the control plane, briefly.
			utils.MutatingTool(true, true),
			mcp.WithString("peer_id",
				mcp.Required(),
				mcp.Description(
					"The server to make leader, as the \"id\" shown by get_raft_config. Its Raft "+
						"address (\"10.0.0.2:4647\") is also accepted."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerID, err := req.RequireString("peer_id")
			if err != nil {
				return utils.ErrorResult(
					"The 'peer_id' argument is required. Use get_raft_config to see the peers.")
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := &api.QueryOptions{Region: region}

			cfg, err := nomad.Operator().RaftGetConfiguration(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the Raft configuration before transferring leadership",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}

			target := findPeer(cfg, peerID)
			if target == nil {
				return utils.ErrorResult(noSuchPeer(cfg, peerID))
			}

			if target.Leader {
				return utils.JSONResult(map[string]any{
					"changed": false,
					"leader":  describePeer(target),
					"note": "Nothing was done: that server is already the leader. Transferring " +
						"leadership to the current leader would cost an election for no change.",
				})
			}

			// A non-voter cannot become leader, and Nomad's own error for this
			// does not say why the server is not a candidate.
			if !target.Voter {
				return utils.ErrorResultf(
					"Refused: %s is NOT a voter, so it cannot become the leader.\n\n"+
						"A server that recently joined stays a non-voter until Autopilot promotes "+
						"it, which happens once it has been healthy for server_stabilization_time. "+
						"get_autopilot_health says whether it is stabilising, and "+
						"get_autopilot_config gives the delay. On Enterprise it may instead be a "+
						"read replica, which never becomes a voter.",
					describePeer(target))
			}

			if target.Node == unknownNode {
				return utils.ErrorResultf(
					"Refused: %s is an ORPHANED Raft entry — it counts toward quorum but Nomad has "+
						"no server matching it, so there is nothing there to lead.\n\n"+
						"remove_raft_peer is what to do with an entry like this.",
					describePeer(target))
			}

			// Handing leadership to an unhealthy server is how a brief handover
			// becomes a long outage.
			if reply, _, err := nomad.Operator().AutopilotServerHealth(q); err == nil && reply != nil {
				for _, s := range reply.Servers {
					if samePeer(target, s.ID, s.Address, s.Name) && !s.Healthy {
						return utils.ErrorResultf(
							"Refused: %s is reported UNHEALTHY by Autopilot, so it is trailing the "+
								"Raft log or out of contact with the current leader.\n\n"+
								"Handing leadership to it turns a brief handover into a real outage: "+
								"it has to catch up before it can serve, and it may lose the election "+
								"entirely. Pick a healthy voter from get_autopilot_health instead.",
							describePeer(target))
					}
				}
			}

			var from string
			for _, s := range cfg.Servers {
				if s != nil && s.Leader {
					from = describePeer(s)
				}
			}

			if err := nomad.Operator().RaftTransferLeadershipByID(target.ID, &api.WriteOptions{
				Region: region,
			}); err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "transfer Raft leadership to " + describePeer(target),
					Address:    p.Address(),
					Capability: "operator:write",
				}, p.Redactor()))
			}

			out := map[string]any{
				"changed": true,
				"to":      describePeer(target),
				"peer_id": target.ID,
				"note": "Leadership transfer was accepted. It completes asynchronously, so the new " +
					"leader may take a moment to appear — confirm with get_raft_config rather than " +
					"assuming it has already happened.",
			}
			if from != "" {
				out["from"] = from
			}
			return utils.JSONResult(out)
		},
	}
}

// findPeer resolves a user-supplied identifier against the Raft configuration.
//
// Both the ID and the address are accepted because an orphaned entry has no
// node name, and its address is how get_raft_config reports it — which makes
// the address the thing a caller is most likely to paste back.
func findPeer(cfg *api.RaftConfiguration, id string) *api.RaftServer {
	if cfg == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	for _, s := range cfg.Servers {
		if s != nil && s.ID == id {
			return s
		}
	}
	for _, s := range cfg.Servers {
		if s != nil && (s.Address == id || (s.Node != unknownNode && s.Node == id)) {
			return s
		}
	}
	return nil
}

// samePeer reports whether any of the identifiers given refer to target.
//
// Autopilot and Raft describe the same server through different fields, and
// which ones line up varies with the Nomad version, so all three are compared
// rather than trusting any single one.
func samePeer(target *api.RaftServer, id, address, name string) bool {
	if target == nil {
		return false
	}
	if id != "" && id == target.ID {
		return true
	}
	if address != "" && address == target.Address {
		return true
	}
	return name != "" && target.Node != unknownNode && name == target.Node
}

// noSuchPeer builds the not-found message, listing what does exist.
//
// A bare "not found" is unhelpful here: an orphaned peer's identifier is an
// address or a bare UUID, and the likeliest mistake is quoting the wrong one.
func noSuchPeer(cfg *api.RaftConfiguration, id string) string {
	var have []string
	if cfg != nil {
		for _, s := range cfg.Servers {
			if s == nil {
				continue
			}
			have = append(have, fmt.Sprintf("%s [id %s]", describePeer(s), s.ID))
		}
	}
	if len(have) == 0 {
		return fmt.Sprintf(
			"No Raft peer matches %q, and the Raft configuration lists no peers at all. "+
				"get_raft_config shows what Nomad returned.", id)
	}
	return fmt.Sprintf(
		"No Raft peer matches %q. The configuration currently holds: %s. Give the \"id\" or the "+
			"address exactly as get_raft_config reports it.",
		id, strings.Join(have, "; "))
}
