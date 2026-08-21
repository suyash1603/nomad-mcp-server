// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

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

// CreateNamespace creates or updates a namespace.
func CreateNamespace(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("create_namespace",
			mcp.WithDescription(
				"Create a namespace, or update an existing one's description, metadata and node "+
					"pool configuration.\n\n"+
					"Namespaces partition jobs, allocations and variables. Creating one is additive "+
					"and safe — it does not affect anything already running.\n\n"+
					"Note that this is an upsert: calling it with the name of an existing namespace "+
					"REPLACES that namespace's configuration rather than failing. Read the current "+
					"state with read_namespace first if you are modifying rather than creating."),
			utils.MutatingTool(false, true),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The namespace's name. Lowercase alphanumerics, dashes and underscores."),
			),
			mcp.WithString("description",
				mcp.Description("A human-readable description of what this namespace is for."),
			),
			mcp.WithObject("meta",
				mcp.Description("Arbitrary key/value metadata, as a flat object of strings."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			if strings.TrimSpace(name) == "" {
				return utils.ErrorResult("The 'name' argument cannot be empty.")
			}
			// Creating a namespace outside the allowlist would produce one this
			// server then refuses to use, so refuse up front.
			if _, err := p.ResolveNamespace(ctx, name); err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			existing, _, infoErr := nomad.Namespaces().Info(name, nil)
			isUpdate := infoErr == nil && existing != nil

			ns := &api.Namespace{
				Name:        name,
				Description: req.GetString("description", ""),
			}
			if raw, ok := req.GetArguments()["meta"].(map[string]any); ok {
				ns.Meta = map[string]string{}
				for k, v := range raw {
					if s, ok := v.(string); ok {
						ns.Meta[k] = s
					}
				}
			}

			_, err = nomad.Namespaces().Register(ns, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "create namespace " + name,
					Kind:       "namespace",
					Name:       name,
					Address:    p.Address(),
					Capability: "namespace:write",
				}, p.Redactor()))
			}

			action := "created"
			note := "The namespace is ready to use."
			if isUpdate {
				action = "updated"
				note = "A namespace with this name already existed, so its configuration was replaced."
			}

			return utils.JSONResult(map[string]any{
				"name":   name,
				"action": action,
				"note":   note,
			})
		},
	}
}

// DeleteNamespace deletes an empty namespace.
func DeleteNamespace(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("delete_namespace",
			mcp.WithDescription(
				"Delete a namespace permanently.\n\n"+
					"This is irreversible. Nomad refuses to delete a namespace that still contains "+
					"jobs, which is a real safeguard, but it does not protect the namespace's "+
					"Variables — and those commonly hold secrets.\n\n"+
					"Before calling this: run list_jobs and list_variables against the namespace so "+
					"you know what is there, and get explicit confirmation from the user naming the "+
					"namespace. Do not call it speculatively or as cleanup you inferred was wanted."),
			utils.MutatingTool(true, true),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("The namespace to delete. Must contain no jobs."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			if name == "default" {
				return utils.ErrorResult(
					"The \"default\" namespace cannot be deleted; Nomad requires it to exist.")
			}
			if _, err := p.ResolveNamespace(ctx, name); err != nil {
				return utils.ErrorResult(err.Error())
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			_, err = nomad.Namespaces().Delete(name, &api.WriteOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				msg := utils.MapError(err, utils.ErrorContext{
					Op:         "delete namespace " + name,
					Kind:       "namespace",
					Name:       name,
					Address:    p.Address(),
					Capability: "namespace:write",
					ListTool:   "list_namespaces",
				}, p.Redactor())
				if strings.Contains(strings.ToLower(msg), "non-terminal") ||
					strings.Contains(strings.ToLower(msg), "has jobs") {
					msg += "\n\nNomad will not delete a namespace that still has jobs in it. Stop " +
						"and purge them first with stop_job (purge=true), then retry."
				}
				return utils.ErrorResult(msg)
			}

			return utils.JSONResult(map[string]any{
				"name":    name,
				"deleted": true,
				"note": "The namespace was deleted permanently, along with any Variables it held. " +
					"This cannot be undone.",
			})
		},
	}
}
