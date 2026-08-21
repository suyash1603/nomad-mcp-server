// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package variables holds the tools for Nomad Variables.
//
// Nomad Variables are the cluster's secret store: workloads read them at
// runtime for database passwords, API keys and certificates. Everything in this
// package is written on the assumption that a variable's *value* is a
// credential, even though its *path* usually is not.
//
// That split is why listing and reading are separated. list_variables returns
// paths and never touches values, so it is always available. read_variable
// returns values and is off unless NOMAD_MCP_ALLOW_VARIABLE_READS is explicitly
// turned on — a second gate, independent of the read-only gate, because
// read-only mode does not protect confidentiality.
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

type variableMeta struct {
	Path      string `json:"path"`
	Namespace string `json:"namespace"`
	Created   string `json:"created,omitempty"`
	Modified  string `json:"modified,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
}

// ListVariables lists variable paths, never values.
func ListVariables(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the paths of Nomad Variables in a namespace, with their creation and " +
				"modification times.\n\n" +
				"This returns PATHS ONLY and never returns any variable's contents. That is a " +
				"property of the endpoint, not a filter applied afterwards — Nomad's list API does " +
				"not include values.\n\n" +
				"Use it to discover what configuration exists and where, to confirm a job's expected " +
				"variable path is actually populated, or to check when a value was last rotated. " +
				"Reading the contents requires read_variable, which is separately gated because " +
				"Nomad Variables commonly hold credentials."),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("variable paths"),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_variables", opts...),
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
			})

			vars, meta, err := nomad.Variables().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list variables",
					Kind:       "variable",
					Namespace:  namespace,
					Address:    p.Address(),
					Capability: "variables:list",
				}, p.Redactor()))
			}

			items := make([]variableMeta, 0, len(vars))
			for _, v := range vars {
				if v == nil {
					continue
				}
				items = append(items, variableMeta{
					Path:      v.Path,
					Namespace: v.Namespace,
					Created:   utils.FormatTime(v.CreateTime),
					Modified:  utils.FormatTime(v.ModifyTime),
					Locked:    v.Lock != nil,
				})
			}

			result := utils.List{Count: len(items), Namespace: namespace, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if result.Note == "" {
				if len(items) == 0 {
					result.Note = "No variables found in namespace " + namespace + "."
				} else if !p.Config().AllowVariableReads {
					result.Note = "These are paths only. Reading variable values is disabled on this " +
						"server; see read_variable if you need to know why."
				}
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadVariable returns a variable's values, if that is enabled.
func ReadVariable(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_variable",
			mcp.WithDescription(
				"Read the contents of a Nomad Variable: its key/value items.\n\n"+
					"WARNING: Nomad Variables are where workloads keep secrets — database passwords, "+
					"API tokens, private keys. This tool returns them in plain text into the "+
					"conversation.\n\n"+
					"It is DISABLED by default and only works when the operator has set "+
					"NOMAD_MCP_ALLOW_VARIABLE_READS=true. This is separate from read-only mode: "+
					"read-only protects the cluster from changes, while this protects secrets from "+
					"disclosure, and they are different concerns.\n\n"+
					"Prefer list_variables, which shows paths and timestamps without values, for "+
					"anything that does not genuinely require the contents. When you do read a "+
					"value, do not echo it back to the user or repeat it in a summary unless they "+
					"have explicitly asked for that specific value."),
			utils.ReadOnlyTool(),
			mcp.WithString("path",
				mcp.Required(),
				mcp.Description("The variable's full path, as returned by list_variables."),
			),
			utils.NamespaceParam(),
			utils.RegionParam(),
			mcp.WithBoolean("keys_only",
				mcp.DefaultBool(false),
				mcp.Description(
					"Return only the item keys and not their values. Use this when you need to know "+
						"what a variable contains without disclosing the secrets themselves. This "+
						"works even when variable value reads are disabled on the server."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return readVariable(ctx, req, p)
		},
	}
}

func readVariable(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return utils.ErrorResult("The 'path' argument is required. Use list_variables to see what exists.")
	}
	namespace, err := p.ResolveNamespace(ctx, req.GetString("namespace", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	keysOnly := req.GetBool("keys_only", false)

	// The gate. keys_only stays available because it discloses nothing beyond
	// the shape of the variable, which list_variables already implies.
	if !p.Config().AllowVariableReads && !keysOnly {
		return utils.ErrorResult(
			"Refused: reading Nomad Variable values is disabled on this MCP server.\n\n" +
				"Nomad Variables commonly hold secrets, so their contents are off by default. This " +
				"is separate from read-only mode and is not something you can work around — no other " +
				"tool will return these values, so do not retry.\n\n" +
				"Two options:\n" +
				"  • Call this tool again with keys_only=true to see which keys the variable has, " +
				"without their values. That works right now.\n" +
				"  • To allow full reads, the person running this server must restart it with " +
				"NOMAD_MCP_ALLOW_VARIABLE_READS=true or --allow-variable-reads=true.\n\n" +
				"list_variables also remains available and shows paths and modification times.")
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	v, _, err := nomad.Variables().Read(path, &api.QueryOptions{
		Namespace: namespace,
		Region:    p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read variable " + path,
			Kind:       "variable",
			Name:       path,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "variables:read",
			ListTool:   "list_variables",
		}, p.Redactor()))
	}
	if v == nil {
		return utils.ErrorResultf(
			"No variable at path %q in namespace %q. Use list_variables to see what exists.",
			path, namespace)
	}

	out := map[string]any{
		"path":      v.Path,
		"namespace": v.Namespace,
		"created":   utils.FormatTime(v.CreateTime),
		"modified":  utils.FormatTime(v.ModifyTime),
	}

	if keysOnly {
		keys := make([]string, 0, len(v.Items))
		for k := range v.Items {
			keys = append(keys, k)
		}
		sortStrings(keys)
		out["keys"] = keys
		out["values_withheld"] = true
		out["note"] = "Only the keys are shown. Values were not requested."
		return utils.JSONResult(out)
	}

	out["items"] = v.Items
	out["warning"] = "This variable's values are secrets. Do not repeat them back to the user, " +
		"include them in a summary, or write them into a job specification unless the user has " +
		"explicitly asked for that specific value."

	return utils.JSONResult(out)
}

// PathParam is exported so the write tools share this definition.
func PathParam(desc string) mcp.ToolOption {
	if strings.TrimSpace(desc) == "" {
		desc = "The variable's full path."
	}
	return mcp.WithString("path", mcp.Required(), mcp.Description(desc))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
