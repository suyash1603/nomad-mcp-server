// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package resources exposes Nomad objects as MCP resources.
//
// A tool is something the model decides to call. A resource is something the
// *user* can attach: in Claude Desktop it is the paperclip menu, in Claude Code
// it is an @-mention. The same job can therefore reach the model two ways, and
// both should show it the same thing.
//
// That is why nothing here re-projects Nomad's API. Each resource delegates to
// the tool that already knows how to render that object, and returns its JSON
// verbatim. The alternative — a second set of projections living in this
// package — would drift from the tools within a release or two, and a user who
// attached a job would get a subtly different view from the one the model gets
// when it calls read_job on the same job. One renderer, two front doors.
//
// Everything registered here is read-only. Resources have no equivalent of the
// destructive-hint annotation and no confirmation flow in any client, so a
// mutating resource would be a change the user could trigger by clicking an
// autocomplete entry. There will not be one.
package resources

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
)

// mimeJSON is the media type every resource here returns.
//
// Nomad's own API is JSON and the tools already emit JSON, so anything else
// would mean re-serialising for no gain. Clients that show a preview will
// pretty-print it; clients that do not will hand the model the same bytes the
// equivalent tool call would have produced.
const mimeJSON = "application/json"

// Registrar turns registered tools into resources.
//
// It holds handlers rather than whole ServerTool values because the annotations
// and schemas are irrelevant once the tool is being called directly: a resource
// read has no arguments to validate beyond what the URI template already
// matched.
type Registrar struct {
	provider *client.Provider
	handlers map[string]server.ToolHandlerFunc

	// delegated records which tool each registered resource was wired to, in
	// registration order. It exists so the test that proves no resource can
	// change the cluster reads the wiring that actually happened rather than a
	// list someone maintains alongside it.
	delegated []string
}

// New builds a Registrar over an existing tool catalog.
//
// The catalog is passed in rather than rebuilt so that the resources and the
// tools are provably the same code — if a tool is removed from the catalog, the
// resource that delegates to it fails loudly at registration time instead of
// silently serving a stale copy.
func New(p *client.Provider, catalog []server.ServerTool) *Registrar {
	handlers := make(map[string]server.ToolHandlerFunc, len(catalog))
	for _, t := range catalog {
		handlers[t.Tool.Name] = t.Handler
	}
	return &Registrar{provider: p, handlers: handlers}
}

// Register adds every resource and resource template to the server.
//
// Both the templates and the two index resources are registered. Templates
// alone would be a mistake in practice: several MCP clients only surface
// resources/list and never call resources/templates/list, so a server with
// nothing but templates looks empty in the attachment menu. The two indexes
// give those clients a starting point that names the URI shapes.
func (r *Registrar) Register(s *server.MCPServer) {
	s.AddResource(
		mcp.NewResource("nomad://cluster", "Nomad cluster status",
			mcp.WithResourceDescription(
				"Live health of the Nomad cluster this server is connected to: the leader, the "+
					"server peers and their versions, and a count of client nodes by status. Attach "+
					"this to give the conversation a picture of the cluster before asking about "+
					"anything specific in it."),
			mcp.WithMIMEType(mimeJSON),
		),
		r.static("get_cluster_status", nil),
	)

	// The namespace is named rather than called "the default" because on a
	// cluster where NOMAD_NAMESPACE is set to something else, "default" would
	// be actively wrong about what this resource returns.
	s.AddResource(
		mcp.NewResource("nomad://jobs", "Nomad jobs",
			mcp.WithResourceDescription(
				"Every job in the "+r.provider.Config().NomadNamespace+" namespace, with its type, "+
					"status and a summary of how many allocations are running, pending or failed. "+
					"Individual jobs can then be attached by URI as nomad://jobs/{namespace}/{job_id}."),
			mcp.WithMIMEType(mimeJSON),
		),
		r.static("list_jobs", nil),
	)

	s.AddResourceTemplate(
		mcp.NewResourceTemplate("nomad://jobs/{namespace}/{job_id}", "Nomad job",
			mcp.WithTemplateDescription(
				"One Nomad job: its task groups, the images and resources each task asks for, its "+
					"constraints, and its current allocation counts. The namespace is part of the "+
					"path because job IDs are only unique within a namespace — use \"default\" if "+
					"the cluster has no others. Example: nomad://jobs/default/web-api"),
			mcp.WithTemplateMIMEType(mimeJSON),
		),
		r.templated("read_job", "namespace", "job_id"),
	)

	s.AddResourceTemplate(
		mcp.NewResourceTemplate("nomad://allocs/{alloc_id}", "Nomad allocation",
			mcp.WithTemplateDescription(
				"One allocation: which node it landed on, the state of each of its tasks, its "+
					"restart and reschedule history, and a plain-language diagnosis when something "+
					"is wrong with it. Allocation IDs are cluster-unique, so no namespace is needed. "+
					"Example: nomad://allocs/1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"),
			mcp.WithTemplateMIMEType(mimeJSON),
		),
		r.templated("read_allocation", "", "alloc_id"),
	)

	s.AddResourceTemplate(
		mcp.NewResourceTemplate("nomad://nodes/{node_id}", "Nomad client node",
			mcp.WithTemplateDescription(
				"One Nomad client node: its status and eligibility, drain state, total and "+
					"allocated resources, healthy task drivers, and recent node events. Use this "+
					"when work is not being placed and you suspect the node rather than the job. "+
					"Example: nomad://nodes/9f8e7d6c-5b4a-3210-fedc-ba9876543210"),
			mcp.WithTemplateMIMEType(mimeJSON),
		),
		r.templated("read_node", "", "node_id"),
	)
}

// static returns a handler for a resource whose arguments never vary.
func (r *Registrar) static(tool string, args map[string]any) server.ResourceHandlerFunc {
	r.delegated = append(r.delegated, tool)
	return func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return r.call(ctx, req.Params.URI, tool, args)
	}
}

// templated returns a handler that maps URI template variables onto tool
// arguments.
//
// nsVar is the template variable holding the namespace, or "" for objects that
// are cluster-scoped. idVar is the variable holding the object's ID, and is
// also the argument name the tool expects — the URI templates deliberately name
// their variables after the tool arguments so the two cannot drift apart.
func (r *Registrar) templated(tool, nsVar, idVar string) server.ResourceTemplateHandlerFunc {
	r.delegated = append(r.delegated, tool)
	return func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		vars := req.Params.Arguments

		id := templateVar(vars, idVar)
		if id == "" {
			return nil, fmt.Errorf(
				"the URI %q is missing the %s segment; the shape is nomad://…/{%s}",
				req.Params.URI, idVar, idVar)
		}

		args := map[string]any{idVar: id}
		if nsVar != "" {
			if ns := templateVar(vars, nsVar); ns != "" {
				args[nsVar] = ns
			}
		}
		return r.call(ctx, req.Params.URI, tool, args)
	}
}

// templateVar pulls one matched URI template variable out as a string.
//
// The type switch is not defensive padding. mcp-go matches the URI with
// yosida95/uritemplate and copies the raw uritemplate.Value.V into Arguments,
// and that field is a []string — RFC 6570 variables can expand to lists, so
// even a plain {job_id} arrives as a one-element slice rather than a string.
// Asserting .(string) here silently yields "" and every templated resource read
// fails as "missing segment", which is exactly what happened the first time
// this was wired up. The string and []any cases cover a value that has been
// through a JSON round trip or a future mcp-go that normalises it.
func templateVar(vars map[string]any, name string) string {
	switch v := vars[name].(type) {
	case string:
		return strings.TrimSpace(v)
	case []string:
		if len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// call runs a tool handler and re-frames its result as resource contents.
//
// The error handling is the interesting part. A tool reports a recoverable
// failure through IsError on the result, because the model is expected to read
// the explanation and try something else. A resource read has no such loop: the
// client asked for a specific URI and either gets it or does not. So a tool
// error becomes a real Go error here, which the client surfaces as a failed
// read — and the message is still the mapped, redacted one from utils.MapError,
// so the user is told about the missing capability or the wrong ID rather than
// seeing a bare 403.
func (r *Registrar) call(ctx context.Context, uri, tool string, args map[string]any) ([]mcp.ResourceContents, error) {
	handler, ok := r.handlers[tool]
	if !ok {
		// Only reachable if the catalog and this package disagree, which is a
		// programming error rather than anything the user did.
		return nil, fmt.Errorf("resource %s is backed by the tool %q, which is not registered", uri, tool)
	}
	if args == nil {
		args = map[string]any{}
	}

	result, err := handler(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: tool, Arguments: args},
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", uri, err)
	}
	if result == nil {
		return nil, fmt.Errorf("reading %s returned no content", uri)
	}

	text := resultText(result)
	if result.IsError {
		if text == "" {
			text = "the underlying " + tool + " call failed without an explanation"
		}
		return nil, fmt.Errorf("reading %s: %s", uri, text)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{URI: uri, MIMEType: mimeJSON, Text: text},
	}, nil
}

// Delegated returns the tools the registered resources are backed by.
//
// Exported for the test that asserts every one of them is a read-only tool.
// Call it after Register; before that it is empty.
func (r *Registrar) Delegated() []string {
	out := make([]string, len(r.delegated))
	copy(out, r.delegated)
	return out
}

// resultText concatenates the text parts of a tool result.
//
// In practice every tool in this catalog returns exactly one text block, but
// joining rather than taking the first means a tool that grows a second one
// does not silently lose it here.
func resultText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if t, ok := c.(mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
