// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package prompts

import (
	"context"
	"errors"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// errMissingNodeID mirrors errMissingJobID: a missing required argument is a
// client-side mistake, and a prompt about node "" helps nobody.
var errMissingNodeID = errors.New(
	"drain_node_safely needs a node_id: the ID of the client node to take out of service, as shown by list_nodes")

func drainNodeSafely() mcp.Prompt {
	return mcp.NewPrompt("drain_node_safely",
		mcp.WithPromptDescription(
			"Take a client node out of service without dropping the work on it. Checks that the "+
				"rest of the cluster can absorb the load before anything is moved, drains in the "+
				"right order, and verifies the work actually landed somewhere — rather than "+
				"assuming a drain that returned successfully did what was wanted."),
		mcp.WithArgument("node_id",
			mcp.RequiredArgument(),
			mcp.ArgumentDescription("The ID of the client node, exactly as it appears in list_nodes."),
		),
		mcp.WithArgument("reason",
			mcp.ArgumentDescription(
				"Optional: why the node is being taken out — \"replacing the instance\", "+
					"\"kernel upgrade\", \"suspected bad disk\". This changes the right ending: a "+
					"node coming back needs its drain cancelled, a node being destroyed needs "+
					"purging."),
		),
		mcp.WithArgument("permanent",
			mcp.ArgumentDescription(
				"Optional: \"true\" if the machine is being destroyed and will not rejoin. "+
					"Anything else is treated as temporary."),
		),
	)
}

func (r *Registrar) handleDrainNode(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	args := req.Params.Arguments

	nodeID := strings.TrimSpace(args["node_id"])
	if nodeID == "" {
		return nil, errMissingNodeID
	}

	permanent := strings.EqualFold(strings.TrimSpace(args["permanent"]), "true")
	reason := strings.TrimSpace(args["reason"])

	var b strings.Builder

	b.WriteString("Take Nomad client node ")
	b.WriteString(nodeID)
	b.WriteString(" out of service safely.\n\n")

	if reason != "" {
		b.WriteString("The operator's stated reason: ")
		b.WriteString(reason)
		b.WriteString("\n\n")
	}

	b.WriteString(r.modeNote())
	b.WriteString("\n\n")
	b.WriteString(injectionNote)
	b.WriteString("\n\n")

	// The ordering below is the point of this prompt. A drain that is issued
	// before checking capacity does not fail — it succeeds, and the work it
	// evicted simply stops running, which surfaces later as an unrelated
	// incident. Checking first is the difference.
	b.WriteString(`Work through this in order. Do not skip ahead to the drain.

STEP 1 — Understand what is at stake.
  - read_node on this node: its status, pool, class, drain state and eligibility.
  - list_node_allocations on it: exactly what is running there, and for which jobs.
  - get_node_stats on it, if the node is up: whether it is unhealthy in a way that
    explains the request.

  Report what is on the node before proposing to move any of it. If the node is
  already down, nothing is running on it and there is nothing to drain — say so
  and go to step 5.

STEP 2 — Establish that the rest of the cluster can take the load.
  - list_nodes: how many other nodes are ready, eligible and not draining.
  - For any allocation on this node with a node_pool, node class or constraint,
    check with read_node_pool or read_node that somewhere else satisfies it.

  This is the step that gets skipped. A drain with nowhere to reschedule to does
  not fail: Nomad stops the allocations, the replacements go into a blocked
  evaluation, and the work is simply gone until someone notices. If capacity or
  a matching constraint is missing, STOP and tell the operator that before
  draining anything.

STEP 3 — Stop new work landing on the node.
  - set_node_eligibility with eligible=false.

  This is not the drain. It is non-destructive and instantly reversible, and it
  prevents the scheduler putting anything new on a node that is about to be
  emptied.

STEP 4 — Drain, and confirm the operator wants it.
  Present what you found in steps 1 and 2, name the node, and get an explicit
  yes before calling drain_node.

  - drain_node with enable=true and a deadline that suits the workloads. The
    default of 1h is right for services that take time to become healthy
    elsewhere; a short deadline forces allocations off before their replacements
    are ready.
  - Consider ignore_system_jobs=true if the node runs log or metrics agents that
    should keep working until the machine actually goes away.

STEP 5 — Verify, do not assume.
  A drain returning successfully means it started, not that it finished.

  - list_node_allocations on the node again: it should be emptying.
  - list_job_allocations for each affected job: the replacements should be
    running somewhere else and healthy.
  - list_evaluations filtered to blocked evaluations: anything blocked here is
    work that could NOT be rescheduled, and that is the failure mode step 2 was
    guarding against.

  If work is stuck, say so plainly. Cancelling the drain with drain_node
  enable=false stops further eviction, but allocations already moved do not
  come back on their own.

`)

	if permanent {
		b.WriteString(`STEP 6 — The machine is being destroyed.
  Once the node is empty and its work is confirmed running elsewhere, the agent
  can be stopped. Nomad will show the node as "down".

  purge_node then removes it from Nomad's state for good. It refuses to run
  while the node is still heartbeating, which is deliberate: purging a live node
  achieves nothing, because the agent re-registers on its next beat. Stop the
  agent first, wait for "down", then purge.

  Purging is irreversible. Confirm with the operator before it.
`)
	} else {
		b.WriteString(`STEP 6 — Bringing the node back.
  The node is out of service but still registered, which is the right state for
  a machine that is coming back.

  When it is ready to take work again, drain_node with enable=false cancels the
  drain and marks it eligible in one call. Allocations that migrated away do not
  return on their own — Nomad places new work there as it comes, and the node
  fills up again over time rather than immediately. Say that rather than
  reporting the node as restored and empty.

  Do NOT purge this node. Purging is for a machine that will not rejoin.
`)
	}

	b.WriteString(`
Finish with a short summary: what was running on the node, where it went,
anything that could not be moved, and what state the node is in now.`)

	return &mcp.GetPromptResult{
		Description: "Safely drain Nomad client node " + nodeID,
		Messages: []mcp.PromptMessage{
			mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(b.String())),
		},
	}, nil
}
