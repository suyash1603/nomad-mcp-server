// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// integrationsResult is the tool's output.
type integrationsResult struct {
	Namespace     string    `json:"namespace_scope"`
	VaultEnabled  *bool     `json:"vault_enabled,omitempty"`
	VaultClusters []string  `json:"vault_clusters,omitempty"`
	ConsulCount   int       `json:"consul_clusters_configured"`
	Findings      []finding `json:"findings"`
	Count         int       `json:"finding_count"`
	AllocsScanned int       `json:"allocations_scanned"`
	AllocsTotal   int       `json:"allocations_total"`
	Healthy       bool      `json:"looks_healthy"`
	Note          string    `json:"note,omitempty"`
	Warning       string    `json:"warning"`
}

// signature is one class of integration failure, recognised in task events.
type signature struct {
	category string
	system   string
	match    *regexp.Regexp
	summary  string
	detail   string
	nextStep string
	sev      severity
}

// signatures are the integration failures that show up in Nomad's own task
// events. Each is a failure whose cause lives in Vault or Consul but whose
// symptom Nomad records, which is exactly the set this tool can find without
// holding credentials for either system.
var signatures = []signature{
	{
		category: "vault-token",
		system:   "vault",
		match:    regexp.MustCompile(`(?i)vault.*(deriv|token|denied|permission|403|missing.*polic)`),
		summary:  "Vault token derivation or permission failures",
		detail: "Nomad could not obtain a Vault token for the task, or the token it obtained was " +
			"refused. The task never starts, so it has no logs to read — the reason is in this " +
			"event and in Vault's audit log.",
		nextStep: "Check the job's vault block: the role or policy it names must exist and must " +
			"permit the paths its templates read. If a Vault MCP server is available, the role " +
			"named in the detail is what to look up there.",
		sev: sevCritical,
	},
	{
		category: "template-render",
		system:   "vault or consul",
		match:    regexp.MustCompile(`(?i)(template).*(fail|error|denied|missing|timeout)`),
		summary:  "template rendering failures",
		detail: "A task's template block could not render. On a cluster with Vault or Consul " +
			"integrated this is nearly always a secret path or KV key that does not exist, or a " +
			"token without permission to read it. The task stays pending and writes nothing.",
		nextStep: "read_allocation on one of these allocations for the full event text, which " +
			"usually names the path. The path is the thing to check in Vault or Consul.",
		sev: sevCritical,
	},
	{
		category: "consul-connect",
		system:   "consul",
		match:    regexp.MustCompile(`(?i)(envoy|connect|sidecar).*(fail|error|bootstrap|timeout|exit)`),
		summary:  "Consul Connect sidecar failures",
		detail: "The Envoy sidecar did not start or did not bootstrap. The main task can be " +
			"perfectly healthy while this is broken, and the service will be unreachable through " +
			"the mesh regardless.",
		nextStep: "read_allocation_logs on the sidecar task — it is a separate task in the same " +
			"allocation, usually named connect-proxy-<service>.",
		sev: sevCritical,
	},
	{
		category: "consul-registration",
		system:   "consul",
		match:    regexp.MustCompile(`(?i)consul.*(regist|dereg|fail|error|denied|unreachable|connection refused)`),
		summary:  "Consul service registration failures",
		detail: "Nomad could not register or deregister a service with Consul. The allocation runs, " +
			"but nothing discovers it, so it receives no traffic while looking entirely healthy.",
		nextStep: "Check the Nomad client agent's Consul token and reachability. list_services " +
			"shows only services using provider = \"nomad\", so a Consul-registered service that " +
			"failed will not appear there either.",
		sev: sevCritical,
	},
	{
		category: "identity-workload",
		system:   "vault or consul",
		match:    regexp.MustCompile(`(?i)(workload ident|jwt|signed identity).*(fail|error|invalid|expire|denied)`),
		summary:  "workload identity failures",
		detail: "Nomad issues each task a signed identity that Vault and Consul verify. When that " +
			"is rejected, every integration for the task fails at once even though the job, the " +
			"policies and the paths are all correct.",
		nextStep: "Check the auth method configuration in Vault or Consul against the cluster's " +
			"JWKS endpoint. This is a cluster-level misconfiguration, not a per-job one.",
		sev: sevCritical,
	},
}

// DiagnoseIntegrations finds Vault and Consul failures from the Nomad side.
func DiagnoseIntegrations(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("diagnose_integrations",
			mcp.WithDescription(
				"Find Vault and Consul integration failures by scanning what Nomad itself recorded: "+
					"whether each integration is configured, and every task event across the cluster "+
					"matching a known failure signature — Vault token derivation, template rendering, "+
					"Consul Connect sidecar startup, service registration, and workload identity.\n\n"+
					"These failures are hard to find any other way because the task usually never "+
					"starts, so there are NO LOGS to read. read_allocation_logs returns nothing and "+
					"the job looks fine; the reason exists only in the allocation's task events, "+
					"scattered one allocation at a time.\n\n"+
					"Reach for this when tasks are stuck pending on a cluster that uses Vault or "+
					"Consul, when a service is running but nothing can reach it, or when a change to "+
					"Vault policy or Consul ACLs coincides with jobs breaking.\n\n"+
					"IMPORTANT — this reads NOMAD ONLY. It holds no Vault or Consul credentials and "+
					"queries neither system. It tells you which Vault role or Consul path to look "+
					"at; confirming what is wrong there is a separate step, with the Vault or Consul "+
					"tooling you already use."),
			utils.ReadOnlyTool(),
			utils.NamespaceParam(),
			utils.RegionParam(),
			mcp.WithNumber("max_examples",
				mcp.DefaultNumber(defaultExamplesPerFinding),
				mcp.Description(
					"How many example allocation IDs each finding carries. Defaults to 5; the count "+
						"is always the true total regardless."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return diagnoseIntegrations(ctx, req, p)
		},
	}
}

func diagnoseIntegrations(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	namespace, err := resolveScanNamespace(ctx, req, p)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	region := p.ResolveRegion(ctx, req.GetString("region", ""))
	maxExamples := req.GetInt("max_examples", defaultExamplesPerFinding)
	if maxExamples <= 0 {
		maxExamples = defaultExamplesPerFinding
	}

	out := integrationsResult{
		Namespace: namespace,
		Findings:  []finding{},
		Warning:   untrustedNote,
	}

	// Configuration first, and only the two booleans. The agent's Vault and
	// Consul blocks hold a token and TLS paths, which get_agent_config
	// deliberately refuses to expose; nothing here widens that. Whether an
	// integration is switched on is not a secret, and it answers the most
	// common confusion outright — a job with a vault block on a cluster where
	// Vault was never enabled.
	if self, err := nomad.Agent().Self(); err == nil && self != nil {
		out.VaultEnabled, out.VaultClusters = vaultConfig(self.Config)
		out.ConsulCount = consulCount(self.Config)
	}

	stubs, _, err := nomad.Allocations().List(&api.QueryOptions{
		Namespace: namespace,
		Region:    region,
		PerPage:   scanPageSize,
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "list allocations",
			Kind:       "allocation",
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "read-job",
			ListTool:   "list_allocations",
		}, p.Redactor()))
	}
	out.AllocsTotal = len(stubs)

	hits := map[string]*signatureHits{}
	for _, a := range stubs {
		if a == nil {
			continue
		}
		out.AllocsScanned++
		scanAllocEvents(a, hits, p)
	}

	out.Findings = integrationFindings(hits, maxExamples)
	sortFindings(out.Findings)
	out.Count = len(out.Findings)
	out.Healthy = out.Count == 0
	out.Note = integrationsNote(out)

	return utils.JSONResult(out)
}

// signatureHits accumulates one signature's matches.
type signatureHits struct {
	sig     signature
	allocs  map[string]bool
	samples []string
}

// scanAllocEvents matches one allocation's task events against every signature.
func scanAllocEvents(a *api.AllocationListStub, hits map[string]*signatureHits, p *client.Provider) {
	for task, ts := range a.TaskStates {
		if ts == nil {
			continue
		}
		for _, e := range ts.Events {
			if e == nil {
				continue
			}
			text := eventText(e)
			if text == "" {
				continue
			}
			for _, sig := range signatures {
				if !sig.match.MatchString(text) {
					continue
				}
				h := hits[sig.category]
				if h == nil {
					h = &signatureHits{sig: sig, allocs: map[string]bool{}}
					hits[sig.category] = h
				}
				h.allocs[a.ID] = true
				if len(h.samples) < 3 {
					h.samples = append(h.samples,
						task+": "+p.Redactor().String(truncate(text, 200)))
				}
			}
		}
	}
}

// eventText gathers every field of a task event worth matching against.
func eventText(e *api.TaskEvent) string {
	parts := []string{
		e.DisplayMessage, e.Message, e.DriverError, e.SetupError,
		e.VaultError, e.KillError, e.ValidationError, e.Type,
	}
	var b strings.Builder
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			b.WriteString(p)
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

// integrationFindings turns the accumulated hits into ranked findings.
func integrationFindings(hits map[string]*signatureHits, maxExamples int) []finding {
	out := make([]finding, 0, len(hits))

	for _, h := range hits {
		ids := make([]string, 0, len(h.allocs))
		for id := range h.allocs {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		f := finding{
			sev:      h.sig.sev,
			Category: h.sig.category,
			Count:    len(ids),
			Summary: fmt.Sprintf("%s affecting %d allocation%s (%s)",
				h.sig.summary, len(ids), plural(len(ids)), h.sig.system),
			Examples: shortIDs(ids, maxExamples),
			Detail:   h.sig.detail,
			NextStep: h.sig.nextStep,
		}
		if len(h.samples) > 0 {
			f.Detail += " Observed: " + strings.Join(h.samples, " | ")
		}
		out = append(out, f)
	}

	return out
}

// vaultConfig reads whether Vault is enabled, and nothing else.
//
// Newer Nomad reports a list of Vault clusters under "Vaults"; older versions
// use a single "Vault" block. Both are handled, and from either only Enabled
// and Name are read — never Token, Addr or the TLS paths that sit beside them.
func vaultConfig(cfg map[string]any) (*bool, []string) {
	var enabled bool
	var names []string
	var found bool

	consider := func(m map[string]any) {
		found = true
		if v, ok := m["Enabled"].(bool); ok && v {
			enabled = true
		}
		if n, ok := m["Name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}

	if list, ok := cfg["Vaults"].([]any); ok {
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				consider(m)
			}
		}
	}
	if m, ok := cfg["Vault"].(map[string]any); ok {
		consider(m)
	}

	if !found {
		return nil, nil
	}
	sort.Strings(names)
	return &enabled, names
}

// consulCount reports how many Consul clusters are configured.
//
// Only the count. The Consul block carries an auth token and TLS material, and
// unlike Vault it has no Enabled field to read — Nomad's Consul integration is
// implicit — so presence is the only safe signal, and it is enough to answer
// "is Consul configured on this cluster at all?".
func consulCount(cfg map[string]any) int {
	if list, ok := cfg["Consuls"].([]any); ok {
		return len(list)
	}
	if _, ok := cfg["Consul"].(map[string]any); ok {
		return 1
	}
	return 0
}

// integrationsNote explains the result.
func integrationsNote(r integrationsResult) string {
	var parts []string

	if r.Count == 0 {
		parts = append(parts, fmt.Sprintf(
			"No Vault or Consul failure signatures found in the task events of %d allocations. "+
				"This covers allocations that still exist — an allocation that failed and was "+
				"garbage collected takes its events with it, so this is not proof nothing ever "+
				"went wrong.", r.AllocsScanned))
	} else {
		parts = append(parts, fmt.Sprintf(
			"%d finding%s from the task events of %d allocations, most severe first.",
			r.Count, plural(r.Count), r.AllocsScanned))
	}

	switch {
	case r.VaultEnabled != nil && !*r.VaultEnabled:
		// Deliberately not "configured but disabled": a Nomad agent carries a
		// placeholder vault block whether or not anyone set one up, so calling
		// that "configured" sends people looking for configuration that was
		// never written. Not enabled is true either way.
		parts = append(parts,
			"Vault is NOT enabled on this cluster — no vault block sets enabled = true. A job with "+
				"a vault block will fail here, and the error rarely says so plainly.")
	case r.VaultEnabled == nil:
		parts = append(parts,
			"Whether Vault is enabled could not be read — the token probably lacks agent:read. "+
				"The event scan above does not depend on it.")
	}

	if r.ConsulCount == 0 {
		parts = append(parts,
			"No Consul cluster is configured. Jobs using provider = \"consul\" or a connect block "+
				"cannot work here.")
	}

	parts = append(parts,
		"This tool reads Nomad only. It names the Vault role or Consul path to investigate; "+
			"confirming the cause requires the Vault or Consul tooling you already use.")

	return joinNote(parts...)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
