// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package jobs

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

type jobVersion struct {
	Version   uint64   `json:"version"`
	Stable    bool     `json:"stable"`
	Submitted string   `json:"submitted,omitempty"`
	Current   bool     `json:"current,omitempty"`
	Groups    []string `json:"task_groups,omitempty"`
	Changes   []string `json:"changes_from_previous,omitempty"`
}

// ListJobVersions lists a job's version history.
func ListJobVersions(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("list_job_versions", jobToolOptions(
			"List the version history of a job, newest first, with what changed between each "+
				"version and the one before it.\n\n"+
				"Nomad keeps every submitted version of a job. Use this to answer \"what changed?\" "+
				"after a job started misbehaving, and to find the version number to hand to "+
				"revert_job_version.\n\n"+
				"Versions marked stable completed a deployment successfully, which makes them the "+
				"safest thing to revert to. The version marked current is what is running now.",
			mcp.WithBoolean("diffs",
				mcp.DefaultBool(true),
				mcp.Description("Include a summary of what changed between consecutive versions."),
			),
		)...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, namespace, q, err := jobArgs(ctx, req, p)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			wantDiffs := req.GetBool("diffs", true)
			versions, diffs, _, err := nomad.Jobs().Versions(jobID, wantDiffs, q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list versions for job " + jobID,
					Kind:       "job",
					Name:       jobID,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "read-job",
					ListTool:   "list_jobs",
				}, p.Redactor()))
			}

			items := make([]jobVersion, 0, len(versions))
			for i, v := range versions {
				if v == nil {
					continue
				}
				item := jobVersion{Current: i == 0}
				if v.Version != nil {
					item.Version = *v.Version
				}
				if v.Stable != nil {
					item.Stable = *v.Stable
				}
				if v.SubmitTime != nil {
					item.Submitted = utils.FormatTime(*v.SubmitTime)
				}
				for _, tg := range v.TaskGroups {
					if tg != nil {
						item.Groups = append(item.Groups, deref(tg.Name))
					}
				}
				// diffs[i] describes the change from versions[i+1] to versions[i].
				if wantDiffs && i < len(diffs) && diffs[i] != nil {
					item.Changes = projectDiff(diffs[i]).summarise()
				}
				items = append(items, item)
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if len(items) <= 1 {
				result.Note = "This job has only ever been submitted once, so there is nothing to compare or revert to."
			}
			return utils.JSONResult(result)
		},
	}
}

// decodeJSON is a small wrapper so plan.go does not import encoding/json
// directly for its one use.
func decodeJSON(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
