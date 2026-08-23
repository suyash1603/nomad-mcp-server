// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// checkResult is one probe's outcome.
//
// Status is a small vocabulary rather than free text so that a model can act on
// it without parsing prose: "ok", "warn", "fail" or "skip". Fix is the concrete
// next step, and it is the field that makes this tool worth having over a bare
// connection error.
type checkResult struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// connectionReport is what check_connection returns.
type connectionReport struct {
	Address string `json:"nomad_address"`
	Reached bool   `json:"reached"`

	Edition string `json:"edition,omitempty"`
	Version string `json:"version,omitempty"`

	Checks []checkResult `json:"checks"`

	Posture map[string]any `json:"server_posture"`

	Summary string `json:"summary"`
}

// GetAgentTimeout bounds each probe so that an unreachable address fails in
// seconds rather than sitting on the client's default timeout. A diagnostic
// that hangs is worse than one that reports a timeout.
const probeTimeout = 10 * time.Second

// CheckConnection diagnoses how this server is connected to Nomad.
func CheckConnection(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("check_connection",
			mcp.WithDescription(
				"Diagnose this MCP server's connection to Nomad: whether the cluster is reachable, "+
					"whether TLS and the ACL token are working, which edition and version it runs, "+
					"and what this server is permitted to do.\n\n"+
					"Run this FIRST whenever any other tool fails with a connection, permission or "+
					"authentication error, and whenever someone is setting this server up against "+
					"a new cluster. Each check reports a concrete fix rather than a status code, "+
					"which is the difference between \"connection refused\" and \"NOMAD_ADDR points "+
					"at localhost but you are running in a container, so localhost is the "+
					"container\".\n\n"+
					"It is read-only and safe to run at any time. It reads no job data, no logs and "+
					"no variables, and it never returns the token.\n\n"+
					"This is also the tool that answers \"is this cluster Community or Enterprise\" "+
					"and \"does my token have enough permission\" before anyone tries an operation "+
					"that would fail halfway through."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return checkConnection(ctx, req, p)
		},
	}
}

func checkConnection(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	cfg := p.Config()
	report := connectionReport{
		Address: cfg.NomadAddr,
		Posture: posture(cfg),
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	report.Checks = append(report.Checks, checkAddress(cfg))

	nomad, err := p.FromContext(ctx)
	if err != nil {
		report.Checks = append(report.Checks, checkResult{
			Check:  "nomad client",
			Status: "fail",
			Detail: "The Nomad API client could not be built: " + p.Redactor().Error(err),
			Fix:    "Check NOMAD_ADDR and any TLS file paths; the server cannot contact Nomad at all until this is resolved.",
		})
		report.Summary = "The Nomad client could not be constructed, so nothing was contacted."
		return utils.JSONResult(report)
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	q := &api.QueryOptions{Region: region}

	// Reachability. Everything downstream is meaningless if this fails, so its
	// error is mapped in full rather than summarised.
	leader, leaderErr := nomad.Status().Leader()
	switch {
	case leaderErr == nil:
		report.Reached = true
		detail := "Connected. The cluster's leader is " + leader + "."
		status := "ok"
		if leader == "" {
			status, detail = "warn", "Connected, but the cluster reports no leader."
		}
		report.Checks = append(report.Checks, checkResult{
			Check:  "reachability",
			Status: status,
			Detail: detail,
			Fix: map[bool]string{true: "", false: "Nothing can be scheduled without a leader. " +
				"Check that a quorum of servers is running and can reach each other."}[leader != ""],
		})
	default:
		report.Checks = append(report.Checks, checkResult{
			Check:  "reachability",
			Status: "fail",
			Detail: utils.MapError(leaderErr, utils.ErrorContext{
				Op:      "contact the Nomad cluster",
				Address: cfg.NomadAddr,
			}, p.Redactor()),
			Fix: reachabilityFix(cfg.NomadAddr),
		})
	}

	report.Checks = append(report.Checks, checkTLS(cfg))

	if !report.Reached {
		report.Summary = "Nomad at " + cfg.NomadAddr + " could not be contacted. Fix the " +
			"reachability check first; the remaining checks were skipped because they cannot " +
			"mean anything until the cluster answers."
		report.Checks = append(report.Checks,
			skipped("edition"), skipped("acl token"), skipped("permissions"))
		return utils.JSONResult(report)
	}

	// Edition. This is also what tells the caller whether the Enterprise tools
	// in this catalog can work at all.
	ed := p.Edition(ctx)
	report.Edition = string(ed.Edition)
	report.Version = ed.Version
	report.Checks = append(report.Checks, checkEdition(ed))

	report.Checks = append(report.Checks, checkToken(nomad, cfg, q, p))
	report.Checks = append(report.Checks, checkPermissions(nomad, q)...)
	report.Checks = append(report.Checks, checkNamespaces(nomad, cfg, q))

	report.Summary = summarise(report)
	return utils.JSONResult(report)
}

// checkAddress inspects the configured address for the mistakes that produce a
// connection refused, before anything is dialled.
func checkAddress(cfg *config.Config) checkResult {
	addr := cfg.NomadAddr
	if addr == "" {
		return checkResult{
			Check:  "address",
			Status: "fail",
			Detail: "No Nomad address is configured.",
			Fix:    "Set NOMAD_ADDR, for example NOMAD_ADDR=http://127.0.0.1:4646.",
		}
	}

	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return checkResult{
			Check:  "address",
			Status: "fail",
			Detail: "NOMAD_ADDR is set to " + addr + ", which is not a valid URL.",
			Fix:    "It must include a scheme and host, as in http://nomad.internal:4646.",
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return checkResult{
			Check:  "address",
			Status: "fail",
			Detail: "NOMAD_ADDR uses the scheme " + u.Scheme + ", which Nomad's HTTP API does not speak.",
			Fix:    "Use http:// or https://.",
		}
	}

	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return checkResult{
			Check:  "address",
			Status: "warn",
			Detail: "NOMAD_ADDR points at " + host + ", which means Nomad is expected on this same machine.",
			Fix: "That is correct for a local `nomad agent -dev`. It is the single most common " +
				"mistake in every other setup: inside a Docker container, localhost is the " +
				"container itself, not the host — use host.docker.internal on macOS and Windows, " +
				"or --network host on Linux. For a cluster on EC2 or anywhere else remote, set " +
				"NOMAD_ADDR to that cluster's address.",
		}
	}
	if u.Scheme == "http" {
		return checkResult{
			Check:  "address",
			Status: "warn",
			Detail: "NOMAD_ADDR points at the remote host " + host + " over plain HTTP.",
			Fix: "The ACL token is sent in a header on every request, so on any network you do " +
				"not fully control this exposes it. Use https:// with NOMAD_CACERT set.",
		}
	}

	return checkResult{
		Check:  "address",
		Status: "ok",
		Detail: "NOMAD_ADDR is " + addr + ", a remote cluster over TLS.",
	}
}

// reachabilityFix names the causes worth checking, ordered by how often they
// are the answer.
func reachabilityFix(addr string) string {
	fix := "Check, in this order: that Nomad is running and its HTTP port is listening; that " +
		"NOMAD_ADDR (" + addr + ") is the address this process can reach, which is not " +
		"necessarily the one a human uses"

	if u, err := url.Parse(addr); err == nil {
		host := u.Hostname()
		switch {
		case host == "127.0.0.1" || host == "localhost":
			fix += "; and that Nomad really is on this machine — in a container, localhost is " +
				"the container, so use host.docker.internal (macOS, Windows) or --network host " +
				"(Linux)"
		default:
			fix += "; that a security group, firewall or VPC route allows this host to reach " +
				"port " + u.Port() + " on " + host + ", which is the usual cause for a cluster on " +
				"EC2 or another private network; and that any VPN the cluster sits behind is up"
		}
	}
	return fix + "."
}

// checkTLS reports the TLS posture, which is the second most common reason a
// correctly addressed cluster still will not talk.
func checkTLS(cfg *config.Config) checkResult {
	if !strings.HasPrefix(cfg.NomadAddr, "https://") {
		return checkResult{
			Check:  "tls",
			Status: "skip",
			Detail: "The address is plain HTTP, so no TLS is involved.",
		}
	}
	if cfg.NomadSkipVerify {
		return checkResult{
			Check:  "tls",
			Status: "warn",
			Detail: "TLS certificate verification is DISABLED (NOMAD_SKIP_VERIFY is set).",
			Fix: "This makes the connection trivially interceptable, and the token is sent over " +
				"it. Set NOMAD_CACERT to the cluster's CA certificate and turn skip-verify off.",
		}
	}
	if cfg.NomadCACert == "" && cfg.NomadCAPath == "" {
		return checkResult{
			Check:  "tls",
			Status: "warn",
			Detail: "TLS is in use with no CA certificate configured, so verification falls back to the system trust store.",
			Fix: "That works for a publicly trusted certificate and fails for the private CA most " +
				"Nomad clusters use. If the connection fails with a certificate error, set " +
				"NOMAD_CACERT to the cluster's CA file.",
		}
	}
	detail := "TLS is verified against the configured CA."
	if cfg.NomadClientCert != "" {
		detail += " A client certificate is configured, so mTLS is in use."
	}
	return checkResult{Check: "tls", Status: "ok", Detail: detail}
}

func checkEdition(ed client.EditionInfo) checkResult {
	switch ed.Edition {
	case client.EditionEnterprise:
		detail := "This cluster runs Nomad Enterprise"
		if ed.Version != "" {
			detail += " (" + ed.Version + ")"
		}
		detail += ", so the quota, Sentinel, licence and recommendation tools are available."
		if ed.Licensed && ed.LicenseExpires != "" {
			detail += " Its licence expires " + ed.LicenseExpires + "."
		}
		return checkResult{Check: "edition", Status: "ok", Detail: detail}

	case client.EditionCommunity:
		detail := "This cluster runs Nomad Community Edition"
		if ed.Version != "" {
			detail += " (" + ed.Version + ")"
		}
		return checkResult{
			Check:  "edition",
			Status: "ok",
			Detail: detail + ". Every core tool works. The Enterprise-only tools — quotas, " +
				"Sentinel policies, licence and recommendations — do not, because those endpoints " +
				"do not exist here.",
		}

	default:
		return checkResult{
			Check:  "edition",
			Status: "warn",
			Detail: "The cluster's edition could not be determined: " + ed.Reason + ".",
			Fix: "This is usually a token without agent:read rather than a fault. Core tools are " +
				"unaffected; the Enterprise-only tools will report clearly if they are unavailable.",
		}
	}
}

// checkToken establishes whether ACLs are on and whether the token works.
//
// The three cases are genuinely different and are easy to confuse: no ACLs at
// all, ACLs on with no token, and ACLs on with a token that is wrong.
func checkToken(nomad *api.Client, cfg *config.Config, q *api.QueryOptions, p *client.Provider) checkResult {
	self, _, err := nomad.ACLTokens().Self(q)
	if err == nil && self != nil {
		detail := "The token works. It is a " + self.Type + " token"
		if self.Name != "" {
			detail += " named " + self.Name
		}
		if len(self.Policies) > 0 {
			detail += ", with the policies " + strings.Join(self.Policies, ", ")
		}
		detail += "."

		result := checkResult{Check: "acl token", Status: "ok", Detail: detail}
		if self.Type == "management" {
			result.Status = "warn"
			result.Detail = detail + " A management token can do anything in this cluster, " +
				"including reading every Variable and deleting every namespace."
			result.Fix = "Give this server a token with a read-only policy scoped to the " +
				"namespaces it needs. The token is the only limit that Nomad itself enforces; " +
				"NOMAD_MCP_READ_ONLY is enforced here and does not restrict the token."
		}
		if self.ExpirationTime != nil {
			left := time.Until(*self.ExpirationTime)
			switch {
			case left <= 0:
				result.Status = "fail"
				result.Detail += " The token EXPIRED at " + self.ExpirationTime.UTC().Format(time.RFC3339) + "."
				result.Fix = "Issue a new token and restart this server with it in NOMAD_TOKEN."
			case left < 24*time.Hour:
				result.Status = "warn"
				result.Detail += " The token expires at " + self.ExpirationTime.UTC().Format(time.RFC3339) + "."
				result.Fix = "Every tool will start failing with a permission error once it lapses."
			}
		}
		return result
	}

	// ACLs disabled: Nomad answers this endpoint with a 404 and a body saying
	// so, rather than with a 403.
	if err != nil {
		msg := strings.ToLower(p.Redactor().Error(err))
		switch {
		case strings.Contains(msg, "acl support disabled"), strings.Contains(msg, "acl disabled"):
			return checkResult{
				Check:  "acl token",
				Status: "warn",
				Detail: "ACLs are DISABLED on this cluster, so no token is needed and every request is unrestricted.",
				Fix: "Nothing is broken, but nothing is restricted either: this server can do " +
					"whatever its tools allow, on every namespace. NOMAD_MCP_READ_ONLY is the only " +
					"thing limiting it. Enable ACLs before pointing this at anything that matters.",
			}
		case cfg.NomadToken == "":
			return checkResult{
				Check:  "acl token",
				Status: "fail",
				Detail: "ACLs appear to be enabled and no token is configured.",
				Fix: "Set NOMAD_TOKEN in the environment this server runs in. It is deliberately " +
					"environment-only, with no command-line flag, because an argument is visible " +
					"to every process on the machine.",
			}
		}
	}

	return checkResult{
		Check:  "acl token",
		Status: "fail",
		Detail: "The configured token was rejected: " + utils.MapError(err, utils.ErrorContext{
			Op:      "verify the ACL token",
			Address: cfg.NomadAddr,
		}, p.Redactor()),
		Fix: "Check that NOMAD_TOKEN is the token's SecretID rather than its AccessorID — that " +
			"mix-up is the usual cause — and that it has not been revoked.",
	}
}

// permissionProbe is one capability worth knowing about before a workflow
// starts, paired with the tools that need it.
type permissionProbe struct {
	name  string
	tools string
	run   func(*api.Client, *api.QueryOptions) error
}

var permissionProbes = []permissionProbe{
	{"read jobs", "list_jobs, read_job, plan_job", func(c *api.Client, q *api.QueryOptions) error {
		_, _, err := c.Jobs().List(q)
		return err
	}},
	{"read nodes", "list_nodes, read_node, drain_node", func(c *api.Client, q *api.QueryOptions) error {
		_, _, err := c.Nodes().List(q)
		return err
	}},
	{"read namespaces", "list_namespaces, and every namespaced tool", func(c *api.Client, q *api.QueryOptions) error {
		_, _, err := c.Namespaces().List(q)
		return err
	}},
}

// checkPermissions probes the capabilities the common workflows need.
//
// Nomad has no endpoint that reports what a token may do, so the only honest
// way to answer is to try. Each probe is a cheap list call that reads no
// sensitive content.
func checkPermissions(nomad *api.Client, q *api.QueryOptions) []checkResult {
	out := make([]checkResult, 0, len(permissionProbes))
	for _, probe := range permissionProbes {
		err := probe.run(nomad, q)
		switch {
		case err == nil:
			out = append(out, checkResult{
				Check:  "permission: " + probe.name,
				Status: "ok",
				Detail: "Permitted. " + probe.tools + " will work.",
			})
		case utils.IsForbidden(err):
			out = append(out, checkResult{
				Check:  "permission: " + probe.name,
				Status: "warn",
				Detail: "Denied by the token's ACL policy. " + probe.tools + " will fail.",
				Fix: "Add the capability to the policy this token uses, if these tools are " +
					"wanted. This is a Nomad-side permission and cannot be changed from here.",
			})
		default:
			out = append(out, checkResult{
				Check:  "permission: " + probe.name,
				Status: "warn",
				Detail: "Could not be determined: the probe failed for a reason other than permission.",
			})
		}
	}
	return out
}

// checkNamespaces reports the server's own namespace allowlist against what the
// cluster actually has, which catches an allowlist naming a namespace that does
// not exist — a typo that otherwise presents as "the job cannot be found".
func checkNamespaces(nomad *api.Client, cfg *config.Config, q *api.QueryOptions) checkResult {
	if len(cfg.AllowedNamespaces) == 0 {
		return checkResult{
			Check:  "namespace allowlist",
			Status: "warn",
			Detail: "No allowlist is configured, so tools may operate on any namespace the token can reach.",
			Fix: "Set NOMAD_MCP_ALLOWED_NAMESPACES to confine this server to the namespaces it " +
				"actually needs.",
		}
	}

	existing, _, err := nomad.Namespaces().List(q)
	if err != nil {
		return checkResult{
			Check:  "namespace allowlist",
			Status: "ok",
			Detail: "Restricted to " + strings.Join(cfg.AllowedNamespaces, ", ") +
				". The list could not be checked against the cluster's own namespaces.",
		}
	}

	have := map[string]bool{}
	for _, ns := range existing {
		if ns != nil {
			have[ns.Name] = true
		}
	}
	var missing []string
	for _, want := range cfg.AllowedNamespaces {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return checkResult{
			Check:  "namespace allowlist",
			Status: "warn",
			Detail: "The allowlist names " + strings.Join(missing, ", ") +
				", which do not exist in this cluster.",
			Fix: "Usually a typo. Every tool call against those names is refused here before it " +
				"reaches Nomad, which looks identical to the namespace being empty.",
		}
	}
	return checkResult{
		Check:  "namespace allowlist",
		Status: "ok",
		Detail: "Restricted to " + strings.Join(cfg.AllowedNamespaces, ", ") + ", all of which exist.",
	}
}

// posture reports what this server will allow, as distinct from what the
// cluster will allow. Both matter and they are constantly confused.
func posture(cfg *config.Config) map[string]any {
	p := map[string]any{
		"read_only":            cfg.ReadOnly,
		"allow_destructive":    cfg.AllowDestructive,
		"allow_variable_reads": cfg.AllowVariableReads,
		"enterprise_tools":     cfg.Enterprise,
		"default_namespace":    cfg.NomadNamespace,
		"max_log_bytes":        cfg.MaxLogBytes,
	}
	switch {
	case cfg.ReadOnly:
		p["summary"] = "Read-only. Every tool that would change the cluster is refused here, " +
			"regardless of what the token permits."
	case !cfg.AllowDestructive:
		p["summary"] = "Writes are enabled, but tools that discard state or interrupt running " +
			"work are refused."
	default:
		p["summary"] = "Writes are fully enabled, including destructive operations. The token is " +
			"the only remaining limit."
	}
	return p
}

func skipped(name string) checkResult {
	return checkResult{
		Check:  name,
		Status: "skip",
		Detail: "Skipped: the cluster could not be contacted.",
	}
}

// summarise turns the checks into the one sentence someone reads first.
func summarise(r connectionReport) string {
	var fails, warns int
	for _, c := range r.Checks {
		switch c.Status {
		case "fail":
			fails++
		case "warn":
			warns++
		}
	}
	switch {
	case fails > 0:
		return plural(fails, "check failed", "checks failed") +
			". The tools depending on them will not work until the fixes listed above are applied."
	case warns > 0:
		return "Connected to Nomad at " + r.Address + " (" + r.Edition + "). " +
			plural(warns, "check raised a warning", "checks raised warnings") +
			" — nothing is broken, but read them: they are the settings most likely to " +
			"surprise someone later."
	default:
		return "Connected to Nomad at " + r.Address + " (" + r.Edition +
			"). Every check passed."
	}
}
