// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// agentConfig is a deliberately narrow projection of the agent's self report.
//
// The raw /v1/agent/self response is the agent's entire configuration: TLS file
// paths, bind addresses, and the Consul and Vault integration blocks, which on
// older configurations can contain tokens outright. Returning it wholesale
// would push all of that into the model's context.
//
// So this is an allowlist, not a redaction pass. Nomad adds configuration keys
// between versions, and a denylist would silently start leaking whatever was
// added next. An allowlist can only ever fail by omitting something harmless.
type agentConfig struct {
	NodeName   string   `json:"node_name"`
	Region     string   `json:"region"`
	Datacenter string   `json:"datacenter"`
	Version    string   `json:"version,omitempty"`
	Server     bool     `json:"is_server"`
	Client     bool     `json:"is_client"`
	NodePool   string   `json:"node_pool,omitempty"`
	NodeID     string   `json:"node_id,omitempty"`
	Address    string   `json:"advertised_http_address,omitempty"`
	MemberAddr string   `json:"serf_address,omitempty"`
	Status     string   `json:"member_status,omitempty"`
	Note       string   `json:"note"`
	Omitted    []string `json:"omitted_for_safety"`
}

// GetAgentConfig reports identity and role for the agent this server talks to.
func GetAgentConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_agent_config",
			mcp.WithDescription(
				"Report identity and role information for the Nomad agent this MCP server is "+
					"connected to: its node name, region, datacenter, version, whether it is running "+
					"as a server or a client, and its node pool.\n\n"+
					"Use this to confirm which agent you are actually talking to before drawing "+
					"conclusions about a cluster, or when a user asks \"what am I connected to?\".\n\n"+
					"This returns a deliberately small subset of the agent's configuration. TLS paths, "+
					"bind addresses and integration settings such as Consul and Vault blocks are "+
					"withheld because they can contain credentials. Do not tell the user this tool can "+
					"retrieve the agent's full configuration; it cannot, by design."),
			utils.ReadOnlyTool(),
		),
		Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			self, err := nomad.Agent().Self()
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read agent configuration",
					Address:    p.Address(),
					Capability: "agent:read",
				}, p.Redactor()))
			}

			out := agentConfig{
				MemberAddr: self.Member.Addr,
				Status:     self.Member.Status,
				Note: "This is an intentionally limited view. Configuration that can contain " +
					"credentials or reveal network topology is not exposed by this server.",
				Omitted: []string{
					"tls", "consul", "vault", "acl", "telemetry",
					"addresses", "advertise", "plugins", "client.options",
				},
			}

			// Prefer the member's own tags: they are flat strings, whereas the
			// config map's shape varies between Nomad versions.
			if self.Member.Name != "" {
				out.NodeName = self.Member.Name
			}
			out.Region = self.Member.Tags["region"]
			out.Datacenter = self.Member.Tags["dc"]
			out.Version = self.Member.Tags["build"]

			// Fall back to the config map for the handful of scalar keys that
			// have been stable across versions.
			out.NodeName = firstNonEmpty(out.NodeName, stringFrom(self.Config, "Name"))
			out.Region = firstNonEmpty(out.Region, stringFrom(self.Config, "Region"))
			out.Datacenter = firstNonEmpty(out.Datacenter, stringFrom(self.Config, "Datacenter"))
			out.Version = firstNonEmpty(out.Version, stringFrom(self.Config, "Version"))

			out.Server = boolFromNested(self.Config, "Server", "Enabled")
			out.Client = boolFromNested(self.Config, "Client", "Enabled")
			out.NodePool = stringFromNested(self.Config, "Client", "NodePool")

			if stats, ok := self.Stats["client"]; ok {
				out.NodeID = stats["node_id"]
			}

			return utils.JSONResult(out)
		},
	}
}

func stringFrom(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}

func stringFromNested(cfg map[string]any, outer, inner string) string {
	nested, ok := cfg[outer].(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := nested[inner].(string); ok {
		return v
	}
	return ""
}

func boolFromNested(cfg map[string]any, outer, inner string) bool {
	nested, ok := cfg[outer].(map[string]any)
	if !ok {
		return false
	}
	v, _ := nested[inner].(bool)
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
