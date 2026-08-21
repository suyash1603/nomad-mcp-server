// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package variables

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// secretWarning is attached to both write tools.
//
// These are the highest-blast-radius tools in the catalog: they write to and
// delete from the cluster's secret store, and a workload that cannot read its
// variable usually fails at startup rather than degrading visibly.
const secretWarning = "\n\nNomad Variables are the cluster's secret store. Writing here can break " +
	"running workloads that read these values, and a wrong value typically shows up as tasks " +
	"failing to start rather than as an obvious error. Confirm the exact path and keys with the " +
	"user before calling this, and never invent a value."

// WriteVariable creates or replaces a variable.
func WriteVariable(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("write_variable",
			mcp.WithDescription(
				"Create a Nomad Variable, or replace an existing one at the same path.\n\n"+
					"This REPLACES the whole variable, it does not merge into it. Every key you "+
					"omit is deleted. If you are changing one key of an existing variable, read it "+
					"first with read_variable and send back the complete set of items, otherwise "+
					"you will silently drop the others.\n\n"+
					"Workloads read these values at startup and via templates, so changing one can "+
					"take effect immediately or on the next restart depending on how the job "+
					"consumes it."+secretWarning),
			utils.MutatingTool(true, true),
			PathParam("The variable's full path, for example \"nomad/jobs/my-service\". "+
				"Paths under nomad/jobs/ are readable by the matching job's workload identity."),
			mcp.WithObject("items",
				mcp.Required(),
				mcp.Description(
					"The variable's complete contents, as a flat object of string keys to string "+
						"values. This replaces all existing keys at this path."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path, err := req.RequireString("path")
			if err != nil {
				return utils.ErrorResult("The 'path' argument is required.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			raw, ok := req.GetArguments()["items"].(map[string]any)
			if !ok || len(raw) == 0 {
				return utils.ErrorResult(
					"The 'items' argument is required and must be a non-empty object of string keys " +
						"to string values. To remove a variable entirely, use delete_variable.")
			}

			items := api.VariableItems{}
			for k, v := range raw {
				s, ok := v.(string)
				if !ok {
					return utils.ErrorResultf(
						"Item %q is not a string. Nomad Variable values must all be strings; "+
							"encode structured data as JSON text first.", k)
				}
				items[k] = s
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			// Establish whether this replaces something, so the response can
			// say which keys were dropped.
			var replacedKeys []string
			if existing, _, err := nomad.Variables().Peek(path, &api.QueryOptions{Namespace: namespace}); err == nil && existing != nil {
				for k := range existing.Items {
					if _, kept := items[k]; !kept {
						replacedKeys = append(replacedKeys, k)
					}
				}
				sortStrings(replacedKeys)
			}

			written, _, err := nomad.Variables().Update(&api.Variable{
				Namespace: namespace,
				Path:      path,
				Items:     items,
			}, &api.WriteOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "write variable " + path,
					Kind:       "variable",
					Name:       path,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "variables:write",
				}, p.Redactor()))
			}

			keys := make([]string, 0, len(items))
			for k := range items {
				keys = append(keys, k)
			}
			sortStrings(keys)

			out := map[string]any{
				"path":      path,
				"namespace": namespace,
				"keys":      keys,
				"note": "The variable was written. Values are not echoed back here, by design. " +
					"Use list_variables to confirm the modification time.",
			}
			if written != nil {
				out["modify_index"] = written.ModifyIndex
			}
			if len(replacedKeys) > 0 {
				out["keys_removed"] = replacedKeys
				out["warning"] = "This replaced an existing variable, and the keys listed in " +
					"keys_removed no longer exist. If that was not intended, restore them by " +
					"writing the complete set again."
			}
			return utils.JSONResult(out)
		},
	}
}

// DeleteVariable removes a variable.
func DeleteVariable(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("delete_variable",
			mcp.WithDescription(
				"Delete a Nomad Variable and all of its contents, permanently.\n\n"+
					"This is irreversible: Nomad keeps no history of variables, so a deleted value "+
					"is unrecoverable unless it exists somewhere else. Any workload that reads this "+
					"path will fail the next time it tries.\n\n"+
					"Confirm the exact path with the user before calling this, and check what reads "+
					"it first — a path under nomad/jobs/<job> is consumed by that job."+secretWarning),
			utils.MutatingTool(true, true),
			PathParam("The full path of the variable to delete."),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			path, err := req.RequireString("path")
			if err != nil {
				return utils.ErrorResult("The 'path' argument is required. Use list_variables to see what exists.")
			}
			namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
			if err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.Variables().Delete(path, &api.WriteOptions{
				Namespace: namespace,
				Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "delete variable " + path,
					Kind:       "variable",
					Name:       path,
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "variables:destroy",
					ListTool:   "list_variables",
				}, p.Redactor()))
			}

			note := "The variable was deleted permanently. Nomad keeps no history of variables, so " +
				"this cannot be undone."
			if strings.HasPrefix(path, "nomad/jobs/") {
				note += " This path was under nomad/jobs/, so the matching job's workload identity " +
					"could read it — that job may now fail to start."
			}

			return utils.JSONResult(map[string]any{
				"path":      path,
				"namespace": namespace,
				"deleted":   true,
				"note":      note,
			})
		},
	}
}
