// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

func TestEnterpriseVersionDetection(t *testing.T) {
	enterprise := []string{"1.9.3+ent", "1.10.0+ent.hsm", "2.0.5+ENT", "1.8.4+ent.fips1402"}
	community := []string{"1.9.3", "2.0.5", "1.10.0-beta.1", "1.9.3+dev"}

	for _, v := range enterprise {
		require.True(t, isEnterpriseVersion(v), "%q is an Enterprise build", v)
	}
	for _, v := range community {
		require.False(t, isEnterpriseVersion(v), "%q is not an Enterprise build", v)
	}
}

// editionServer is a fake agent that answers only the two endpoints the probe
// uses, so each case can be set up in one line.
type editionServer struct {
	version    string
	licenseFn  http.HandlerFunc
	agentCalls int
}

func (e *editionServer) start(t *testing.T) *Provider {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/self", func(w http.ResponseWriter, _ *http.Request) {
		e.agentCalls++
		if e.version == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "Permission denied")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]any{},
			"member": map[string]any{"Tags": map[string]string{"build": e.version}},
			"stats":  map[string]map[string]string{"nomad": {"version": e.version}},
		})
	})
	mux.HandleFunc("/v1/operator/license", func(w http.ResponseWriter, r *http.Request) {
		if e.licenseFn != nil {
			e.licenseFn(w, r)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, "Nomad Enterprise only endpoint")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	logger := log.New()
	logger.SetOutput(io.Discard)

	p, err := New(&config.Config{
		NomadAddr:      srv.URL,
		NomadNamespace: config.DefaultNomadNamespace,
		MaxLogBytes:    config.DefaultMaxLogBytes,
	}, logger)
	require.NoError(t, err)
	return p
}

func TestCommunityEditionIsDetected(t *testing.T) {
	p := (&editionServer{version: "1.9.3"}).start(t)

	info := p.Edition(context.Background())
	require.Equal(t, EditionCommunity, info.Edition)
	require.Equal(t, "1.9.3", info.Version)
	require.False(t, info.IsEnterprise())
	require.NotEmpty(t, info.Reason, "the probe must say how it decided")
}

func TestEnterpriseEditionIsDetectedFromTheVersion(t *testing.T) {
	// The licence endpoint is denied, as it is for a token without
	// operator:read. The version string alone must still be conclusive.
	p := (&editionServer{
		version: "1.9.3+ent",
		licenseFn: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "Permission denied")
		},
	}).start(t)

	info := p.Edition(context.Background())
	require.Equal(t, EditionEnterprise, info.Edition)
	require.True(t, info.IsEnterprise())
	require.False(t, info.Licensed, "a denied licence read is not a licence")
}

// The licence endpoint answering with a 501 is conclusive on its own, which is
// what makes the probe work for a token that cannot read the agent.
func TestCommunityEditionIsDetectedWithoutAVersion(t *testing.T) {
	p := (&editionServer{version: ""}).start(t)

	info := p.Edition(context.Background())
	require.Equal(t, EditionCommunity, info.Edition)
	require.Contains(t, info.Reason, "Enterprise-only endpoint")
}

func TestLicenceDetailIsReturnedWhenReadable(t *testing.T) {
	p := (&editionServer{
		version: "1.9.3+ent",
		licenseFn: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"License": map[string]any{
					"Features":       []string{"Resource Quotas", "Sentinel Policies"},
					"ExpirationTime": "2030-01-01T00:00:00Z",
				},
			})
		},
	}).start(t)

	info := p.Edition(context.Background())
	require.Equal(t, EditionEnterprise, info.Edition)
	require.True(t, info.Licensed)
	require.Contains(t, info.Features, "Resource Quotas")
	require.Equal(t, "2030-01-01T00:00:00Z", info.LicenseExpires)
}

// An unreachable cluster must resolve to unknown rather than to Community.
// Reporting Community would hide the Enterprise tools from a server that simply
// started before its Nomad did.
func TestUnreachableClusterIsUnknownNotCommunity(t *testing.T) {
	logger := log.New()
	logger.SetOutput(io.Discard)

	p, err := New(&config.Config{
		NomadAddr:      "http://127.0.0.1:1",
		NomadNamespace: config.DefaultNomadNamespace,
		MaxLogBytes:    config.DefaultMaxLogBytes,
	}, logger)
	require.NoError(t, err)

	info := p.Edition(context.Background())
	require.Equal(t, EditionUnknown, info.Edition)
	require.False(t, info.IsEnterprise())
}

// A successful probe is cached, so the dozen Enterprise tools do not each pay
// for their own round trip.
func TestEditionIsCached(t *testing.T) {
	e := &editionServer{version: "1.9.3"}
	p := e.start(t)

	for i := 0; i < 5; i++ {
		require.Equal(t, EditionCommunity, p.Edition(context.Background()).Edition)
	}
	require.Equal(t, 1, e.agentCalls, "the probe should run once and be cached")
}

// An unknown result is not cached: the next call must try again, otherwise a
// server that started before its cluster stays wrong for its whole life.
func TestUnknownEditionIsNotCached(t *testing.T) {
	e := &editionServer{
		version: "",
		licenseFn: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "Permission denied")
		},
	}
	p := e.start(t)

	require.Equal(t, EditionUnknown, p.Edition(context.Background()).Edition)
	require.Equal(t, EditionUnknown, p.Edition(context.Background()).Edition)
	require.Equal(t, 2, e.agentCalls, "an inconclusive probe must be retried, not cached")
}

func TestEnterpriseOnlyErrorMatching(t *testing.T) {
	require.False(t, isEnterpriseOnlyError(nil))
	require.True(t, isEnterpriseOnlyError(errString("Nomad Enterprise only endpoint")))
	require.True(t, isEnterpriseOnlyError(errString("unexpected response: nomad enterprise only endpoint")))
	require.False(t, isEnterpriseOnlyError(errString("Permission denied")))
}

type errString string

func (e errString) Error() string { return string(e) }

func TestAgentVersionFallsBackThroughEachField(t *testing.T) {
	require.Equal(t, "1.2.3", agentVersion(selfWith(map[string]map[string]string{
		"nomad": {"version": "1.2.3"},
	}, nil, nil)))

	require.Equal(t, "9.9.9", agentVersion(selfWith(nil,
		map[string]string{"build": "9.9.9"}, nil)))

	require.Equal(t, "4.5.6+ent", agentVersion(selfWith(nil, nil, map[string]any{
		"Version": map[string]any{"Version": "4.5.6", "VersionMetadata": "ent"},
	})))

	require.Empty(t, agentVersion(selfWith(nil, nil, nil)))
}

func TestEditionProbeNeverPanicsOnAnEmptyBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	logger := log.New()
	logger.SetOutput(io.Discard)
	p, err := New(&config.Config{NomadAddr: srv.URL, MaxLogBytes: 1}, logger)
	require.NoError(t, err)

	require.NotPanics(t, func() { p.Edition(context.Background()) })
}

func TestEditionReasonIsAlwaysPopulated(t *testing.T) {
	for _, version := range []string{"1.9.3", "1.9.3+ent", ""} {
		p := (&editionServer{version: version}).start(t)
		info := p.Edition(context.Background())
		require.NotEmpty(t, strings.TrimSpace(info.Reason),
			"every probe outcome must explain itself")
	}
}

// selfWith builds an AgentSelf with only the fields under test populated.
func selfWith(stats map[string]map[string]string, tags map[string]string, cfg map[string]any) *api.AgentSelf {
	self := &api.AgentSelf{Stats: stats, Config: cfg}
	self.Member.Tags = tags
	return self
}
