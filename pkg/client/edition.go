// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// Edition is which build of Nomad the cluster is running.
//
// It matters because roughly a third of this server's tools call endpoints that
// only exist in Enterprise. Knowing the edition lets the server say "this
// cluster is Community Edition, that feature does not exist here" instead of
// handing the model an opaque HTTP 501, and lets it hide those tools entirely
// when the operator would rather not see them.
type Edition string

const (
	// EditionUnknown means the cluster has not been reached yet, or the probe
	// failed. It is never cached: an unreachable cluster at startup must not
	// pin the answer for the process's lifetime.
	EditionUnknown Edition = "unknown"

	// EditionCommunity is the open-source build, formerly "OSS".
	EditionCommunity Edition = "community"

	// EditionEnterprise is the licensed build.
	EditionEnterprise Edition = "enterprise"
)

// editionTTL is how long a successful probe is trusted.
//
// It is not indefinite because a cluster genuinely can change edition under a
// long-running server: applying a licence to a Community binary is not
// possible, but an operator rolling Enterprise binaries through their servers
// is an ordinary upgrade, and this server may outlive it.
const editionTTL = 15 * time.Minute

// EditionInfo is what a probe found out.
type EditionInfo struct {
	Edition Edition `json:"edition"`

	// Version is the Nomad version string as the agent reports it, including
	// the "+ent" suffix when present.
	Version string `json:"version,omitempty"`

	// Licensed is true when the cluster returned an actual licence. It is
	// distinct from Edition: an Enterprise binary whose licence has expired
	// still reports as Enterprise.
	Licensed bool `json:"licensed,omitempty"`

	// Features lists the licensed Enterprise modules, when the token was
	// allowed to read the licence.
	Features []string `json:"features,omitempty"`

	// LicenseExpires is when the licence lapses, if known.
	LicenseExpires string `json:"license_expires,omitempty"`

	// Reason explains how the edition was determined, or why it could not be.
	// Tools surface this rather than asserting an edition they had to guess.
	Reason string `json:"reason,omitempty"`
}

// IsEnterprise reports whether Enterprise-only endpoints should be expected to
// work. Unknown counts as false for gating decisions but callers should check
// Edition directly when the difference matters to the message they print.
func (e EditionInfo) IsEnterprise() bool { return e.Edition == EditionEnterprise }

// editionCache holds the last successful probe.
type editionCache struct {
	mu       sync.Mutex
	info     EditionInfo
	probedAt time.Time
}

// Edition returns the cluster's edition, probing at most once per TTL.
//
// The probe is deliberately cheap and forgiving. It runs two queries and treats
// every failure as "unknown" rather than as an error, because this is called
// from tool handlers whose actual job is something else: a token that cannot
// read the licence must not turn `list_quotas` into a licence error.
func (p *Provider) Edition(ctx context.Context) EditionInfo {
	p.edition.mu.Lock()
	defer p.edition.mu.Unlock()

	if p.edition.info.Edition != EditionUnknown &&
		time.Since(p.edition.probedAt) < editionTTL {
		return p.edition.info
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return EditionInfo{
			Edition: EditionUnknown,
			Reason:  "could not build a Nomad client: " + p.redactor.Error(err),
		}
	}

	info := probeEdition(nomad)
	if info.Edition != EditionUnknown {
		p.edition.info = info
		p.edition.probedAt = time.Now()
	}
	return info
}

// probeEdition asks the cluster what it is.
//
// The version string is the primary signal: Enterprise builds carry a "+ent"
// suffix and have since Nomad 0.12, and reading it needs no special capability.
// The licence endpoint is consulted second, for the licence detail an operator
// actually wants — and it is the tiebreaker when the version string is missing,
// since Community Edition answers it with a 501.
func probeEdition(nomad *api.Client) EditionInfo {
	info := EditionInfo{Edition: EditionUnknown}

	if self, err := nomad.Agent().Self(); err == nil && self != nil {
		info.Version = agentVersion(self)
		if info.Version != "" {
			if isEnterpriseVersion(info.Version) {
				info.Edition = EditionEnterprise
				info.Reason = "the agent reports version " + info.Version + ", an Enterprise build"
			} else {
				info.Edition = EditionCommunity
				info.Reason = "the agent reports version " + info.Version + ", a Community Edition build"
			}
		}
	}

	// The licence endpoint confirms the version string and carries the detail.
	// A failure here is not conclusive on its own: a token without
	// operator:read gets a 403 on an Enterprise cluster.
	reply, _, err := nomad.Operator().LicenseGet(nil)
	switch {
	case err == nil && reply != nil && reply.License != nil:
		info.Edition = EditionEnterprise
		info.Licensed = true
		info.Features = reply.License.Features
		if !reply.License.ExpirationTime.IsZero() {
			info.LicenseExpires = reply.License.ExpirationTime.UTC().Format(time.RFC3339)
		}
		info.Reason = "the cluster returned an Enterprise licence"

	case err != nil && isEnterpriseOnlyError(err):
		// Only Community Edition answers this way, so it is conclusive.
		info.Edition = EditionCommunity
		info.Reason = "the cluster reports /v1/operator/license as an Enterprise-only endpoint, " +
			"which only Community Edition does"
	}

	if info.Edition == EditionUnknown && info.Reason == "" {
		info.Reason = "the cluster could not be reached, or did not report a version"
	}
	return info
}

// agentVersion digs the version out of an /v1/agent/self response.
//
// Two places carry it and neither is guaranteed: Stats is the documented one,
// and the member's "build" tag is what shows up on a server. Both are checked
// because a client agent and a server agent do not populate the same fields.
func agentVersion(self *api.AgentSelf) string {
	if v, ok := self.Stats["nomad"]["version"]; ok && v != "" {
		return v
	}
	if v, ok := self.Member.Tags["build"]; ok && v != "" {
		return v
	}
	if cfg, ok := self.Config["Version"].(map[string]any); ok {
		if v, ok := cfg["Version"].(string); ok && v != "" {
			if meta, ok := cfg["VersionMetadata"].(string); ok && meta != "" {
				return v + "+" + meta
			}
			return v
		}
	}
	return ""
}

// isEnterpriseVersion reports whether a Nomad version string denotes an
// Enterprise build. Enterprise appends "+ent" and may add a further suffix, as
// in "1.9.3+ent" or "1.9.3+ent.hsm".
func isEnterpriseVersion(v string) bool {
	return strings.Contains(strings.ToLower(v), "+ent")
}

// isEnterpriseOnlyError matches the response Community Edition gives for an
// Enterprise endpoint. The api package converts the 501 into a plain error for
// LicenseGet specifically, so the string is checked as well as the status.
func isEnterpriseOnlyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "enterprise only endpoint")
}
