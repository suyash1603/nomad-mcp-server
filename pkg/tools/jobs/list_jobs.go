// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package jobs holds the tools that read and manage Nomad jobs.
package jobs

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// jobStub is the projection returned by list_jobs.
//
// JobListStub carries a full JobSummary with per-group counters plus a set of
// Raft indexes. Those are dropped here and rolled into a single "allocs" line,
// because a list is for finding the job you want, not for analysing it.
type jobStub struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Namespace   string   `json:"namespace"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Description string   `json:"status_description,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Datacenters []string `json:"datacenters,omitempty"`
	Periodic    bool     `json:"periodic,omitempty"`
	Parameter   bool     `json:"parameterized,omitempty"`
	Stopped     bool     `json:"stopped,omitempty"`
	ParentID    string   `json:"parent_id,omitempty"`
	Allocs      *counts  `json:"allocs,omitempty"`
	Submitted   string   `json:"submitted,omitempty"`
	Unhealthy   bool     `json:"needs_attention,omitempty"`
}

// counts flattens every task group's summary into one set of totals.
type counts struct {
	Running  int `json:"running,omitempty"`
	Starting int `json:"starting,omitempty"`
	Queued   int `json:"queued,omitempty"`
	Complete int `json:"complete,omitempty"`
	Failed   int `json:"failed,omitempty"`
	Lost     int `json:"lost,omitempty"`
	Unknown  int `json:"unknown,omitempty"`
}

// ListJobs lists jobs in a namespace.
func ListJobs(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List Nomad jobs in a namespace, with each job's type, status and a roll-up of how " +
				"many of its allocations are running, queued, failed or complete.\n\n" +
				"This is the usual starting point for \"what is running on this cluster?\" and for " +
				"finding a job whose exact ID you do not know. Jobs flagged with needs_attention " +
				"have queued or failed allocations and are worth looking at first.\n\n" +
				"Returns a summary per job, not the job specification. Use read_job for the " +
				"definition, read_job_summary for per-task-group detail, and list_job_allocations " +
				"to see individual allocations."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("jobs"),
		utils.FilterParam(`Status == "pending"  •  Type == "batch"  •  Stop == true`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_jobs", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return listJobs(ctx, req, p)
		},
	}
}

func listJobs(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	page := utils.PageFrom(req)
	q := page.Apply(&api.QueryOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
		Prefix:    req.GetString("prefix", ""),
		Filter:    req.GetString("filter", ""),
	})

	stubs, meta, err := nomad.Jobs().List(q)
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "list jobs",
			Kind:       "job",
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "list-jobs",
		}, p.Redactor()))
	}

	items := make([]jobStub, 0, len(stubs))
	for _, s := range stubs {
		if s == nil {
			continue
		}
		item := jobStub{
			ID:          s.ID,
			Namespace:   orDefault(s.Namespace, namespace),
			Type:        s.Type,
			Status:      s.Status,
			Description: s.StatusDescription,
			Priority:    s.Priority,
			Datacenters: s.Datacenters,
			Periodic:    s.Periodic,
			Parameter:   s.ParameterizedJob,
			Stopped:     s.Stop,
			ParentID:    s.ParentID,
			Submitted:   utils.FormatUnixSeconds(s.SubmitTime / 1e9),
		}
		// Name repeats ID for most jobs; only include it when it differs.
		if s.Name != s.ID {
			item.Name = s.Name
		}
		if c := summarise(s.JobSummary); c != nil {
			item.Allocs = c
			item.Unhealthy = c.Failed > 0 || c.Queued > 0 || c.Lost > 0
		}
		items = append(items, item)
	}

	result := utils.List{
		Count:     len(items),
		Namespace: namespace,
		Items:     items,
	}
	if meta != nil {
		result.NextToken = meta.NextToken
		result.Note = utils.NextTokenNote(meta.NextToken, len(items))
	}
	if len(items) == 0 && result.Note == "" {
		result.Note = emptyNote(namespace, req.GetString("prefix", ""), req.GetString("filter", ""))
	}

	return utils.JSONResult(result)
}

// summarise flattens a JobSummary's per-group counters into one total.
func summarise(s *api.JobSummary) *counts {
	if s == nil {
		return nil
	}
	var c counts
	for _, tg := range s.Summary {
		c.Running += tg.Running
		c.Starting += tg.Starting
		c.Queued += tg.Queued
		c.Complete += tg.Complete
		c.Failed += tg.Failed
		c.Lost += tg.Lost
		c.Unknown += tg.Unknown
	}
	return &c
}

// emptyNote explains an empty list, which is otherwise ambiguous: the model
// cannot tell "no jobs here" from "wrong namespace" or "filter excluded them".
func emptyNote(namespace, prefix, filter string) string {
	note := "No jobs found in namespace " + namespace + "."
	switch {
	case prefix != "" && filter != "":
		note += " Both a prefix and a filter were applied; try removing them."
	case prefix != "":
		note += " A prefix was applied; try removing it, or use the search tool for partial IDs."
	case filter != "":
		note += " A filter was applied; try removing it."
	default:
		note += " The cluster may genuinely have no jobs here — check other namespaces with list_namespaces, or pass namespace \"*\"."
	}
	return note
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
