// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package enterprise holds the tools for features that exist only in Nomad
// Enterprise: licensing, resource quotas, Sentinel policies and the resource
// recommendations produced by Dynamic Application Sizing.
//
// Every tool here is marked with utils.EnterpriseTool, which does two things:
// it says so in the description the model reads, and it lets the server leave
// these tools out of the catalog entirely when it has established that the
// cluster is Community Edition. Nomad Community answers all of these endpoints
// with HTTP 501, which utils.MapError already turns into a plain explanation,
// so a tool that does slip through against Community Edition still fails
// legibly rather than mysteriously.
package enterprise

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// licenseInfo is the projection returned by get_license.
//
// The licence document itself contains a signed blob and a customer identifier;
// neither helps anyone diagnose anything, and both are the sort of thing that
// should not be pasted into a chat transcript. Only the operational fields are
// returned.
type licenseInfo struct {
	Edition string `json:"edition"`

	InstallationID string `json:"installation_id,omitempty"`
	Product        string `json:"product,omitempty"`
	NonProduction  bool   `json:"non_production,omitempty"`

	IssuedAt     string `json:"issued_at,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	TerminatesAt string `json:"terminates_at,omitempty"`
	DaysLeft     int    `json:"days_until_expiry,omitempty"`

	Modules  []string `json:"modules,omitempty"`
	Features []string `json:"features,omitempty"`

	Expired  bool     `json:"expired"`
	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// GetLicense reports the cluster's Enterprise licence.
func GetLicense(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_license",
			mcp.WithDescription(
				"Read the Nomad Enterprise licence: which modules it covers, when it expires, and "+
					"whether it is a non-production licence.\n\n"+
					"Reach for this when an Enterprise feature is not working and you need to know "+
					"whether the cluster is actually licensed for it — a quota or Sentinel policy "+
					"that will not apply is often an unlicensed module rather than a "+
					"misconfiguration. An expiring licence is also worth surfacing before it "+
					"becomes an incident.\n\n"+
					"Reading the licence needs the operator:read capability, which is more than "+
					"most read tokens carry. Requires Nomad Enterprise: get_cluster_status reports "+
					"which edition this cluster runs."),
			utils.ReadOnlyTool(),
			utils.EnterpriseTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			reply, _, err := nomad.Operator().LicenseGet(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the Nomad Enterprise licence",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if reply == nil || reply.License == nil {
				return utils.ErrorResult(
					"Nomad returned no licence. On Community Edition there is none to return, " +
						"which get_cluster_status will confirm.")
			}

			lic := reply.License
			out := licenseInfo{
				Edition:        string(client.EditionEnterprise),
				InstallationID: lic.InstallationID,
				Product:        lic.Product,
				NonProduction:  lic.NonProduction,
				Modules:        lic.Modules,
				Features:       lic.Features,
			}

			if !lic.IssueTime.IsZero() {
				out.IssuedAt = lic.IssueTime.UTC().Format(time.RFC3339)
			}
			if !lic.TerminationTime.IsZero() {
				out.TerminatesAt = lic.TerminationTime.UTC().Format(time.RFC3339)
			}
			if !lic.ExpirationTime.IsZero() {
				out.ExpiresAt = lic.ExpirationTime.UTC().Format(time.RFC3339)
				left := time.Until(lic.ExpirationTime)
				out.DaysLeft = int(left.Hours() / 24)
				out.Expired = left <= 0

				switch {
				case out.Expired:
					out.Warnings = append(out.Warnings,
						"This licence has EXPIRED. Enterprise features stop applying once a licence "+
							"lapses, and the cluster degrades further at the termination time.")
				case out.DaysLeft <= 30:
					out.Warnings = append(out.Warnings,
						"This licence expires in "+itoa(out.DaysLeft)+" days. Renew it before then: "+
							"Enterprise features stop applying when it lapses.")
				}
			}
			if lic.NonProduction {
				out.Warnings = append(out.Warnings,
					"This is a non-production licence. It is not licensed for production workloads.")
			}
			if reply.ConfigOutdated {
				out.Warnings = append(out.Warnings,
					"Nomad reports the licence on disk is newer than the one in use. A server "+
						"reload or restart is needed to pick it up.")
			}

			out.Note = "Only the operational fields of the licence are returned. The signed licence " +
				"blob and customer identifier are deliberately omitted."

			return utils.JSONResult(out)
		},
	}
}

// itoa avoids importing strconv for the handful of small integers these tools
// put into prose.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
