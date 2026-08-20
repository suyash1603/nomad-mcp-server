// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

func testConfig() *config.Config {
	return &config.Config{
		NomadAddr:      config.DefaultNomadAddr,
		NomadNamespace: config.DefaultNomadNamespace,
		ReadOnly:       true,
		MaxLogBytes:    config.DefaultMaxLogBytes,
	}
}

func newProvider(t *testing.T, cfg *config.Config) *Provider {
	t.Helper()
	p, err := New(cfg, quietLogger())
	require.NoError(t, err)
	return p
}

func TestNewProviderBuildsClient(t *testing.T) {
	p := newProvider(t, testConfig())
	require.NotNil(t, p.fallback)
	require.Equal(t, config.DefaultNomadAddr, p.Address())
}

// TestNewProviderRejectsBadAddress makes a malformed address a startup failure
// rather than a confusing error on the model's first tool call.
func TestNewProviderRejectsBadAddress(t *testing.T) {
	cfg := testConfig()
	cfg.NomadAddr = "://not a url"

	_, err := New(cfg, quietLogger())
	require.Error(t, err)
}

// TestNewProviderRejectsMissingCACert catches an unreadable CA file at startup.
func TestNewProviderRejectsMissingCACert(t *testing.T) {
	cfg := testConfig()
	cfg.NomadCACert = "/nonexistent/ca.pem"

	_, err := New(cfg, quietLogger())
	require.Error(t, err)
}

func TestFromContextWithoutSessionUsesFallback(t *testing.T) {
	p := newProvider(t, testConfig())

	c, err := p.FromContext(context.Background())
	require.NoError(t, err)
	require.Same(t, p.fallback, c)
}

// --- namespace resolution -------------------------------------------------

func TestResolveNamespaceDefaults(t *testing.T) {
	p := newProvider(t, testConfig())

	ns, err := p.ResolveNamespace(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "default", ns, "an empty argument falls back to the configured namespace")
}

func TestResolveNamespaceExplicitArgumentWins(t *testing.T) {
	cfg := testConfig()
	cfg.NomadNamespace = "configured"
	p := newProvider(t, cfg)

	ns, err := p.ResolveNamespace(WithNamespace(context.Background(), "from-header"), "from-argument")
	require.NoError(t, err)
	require.Equal(t, "from-argument", ns)
}

func TestResolveNamespaceHeaderBeatsConfig(t *testing.T) {
	cfg := testConfig()
	cfg.NomadNamespace = "configured"
	p := newProvider(t, cfg)

	ns, err := p.ResolveNamespace(WithNamespace(context.Background(), "from-header"), "")
	require.NoError(t, err)
	require.Equal(t, "from-header", ns)
}

func TestResolveNamespaceTrimsWhitespace(t *testing.T) {
	p := newProvider(t, testConfig())

	ns, err := p.ResolveNamespace(context.Background(), "  prod  ")
	require.NoError(t, err)
	require.Equal(t, "prod", ns)
}

// TestAllowlistRejectsOutsideNamespaces enforces the allowlist before the API
// call, so a disallowed namespace never reaches Nomad.
func TestAllowlistRejectsOutsideNamespaces(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNamespaces = []string{"prod", "staging"}
	p := newProvider(t, cfg)

	_, err := p.ResolveNamespace(context.Background(), "secret-ns")
	require.Error(t, err)
	require.Contains(t, err.Error(), "secret-ns")
	require.Contains(t, err.Error(), "prod, staging")
	require.Contains(t, err.Error(), "NOMAD_MCP_ALLOWED_NAMESPACES",
		"the message must say the limit is the server's, not the token's")
}

func TestAllowlistPermitsListedNamespaces(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNamespaces = []string{"prod", "staging"}
	p := newProvider(t, cfg)

	for _, ns := range []string{"prod", "staging"} {
		got, err := p.ResolveNamespace(context.Background(), ns)
		require.NoError(t, err)
		require.Equal(t, ns, got)
	}
}

// TestAllowlistBlocksWildcard closes the obvious bypass: Nomad treats "*" as
// "every namespace", which would defeat the allowlist entirely.
func TestAllowlistBlocksWildcard(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNamespaces = []string{"prod"}
	p := newProvider(t, cfg)

	_, err := p.ResolveNamespace(context.Background(), "*")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot query all namespaces")
}

func TestWildcardAllowedWithoutAllowlist(t *testing.T) {
	p := newProvider(t, testConfig())

	ns, err := p.ResolveNamespace(context.Background(), "*")
	require.NoError(t, err, "with no allowlist, querying all namespaces is permitted")
	require.Equal(t, "*", ns)
}

// TestAllowlistBlocksDefaultWhenNotListed catches an allowlist that forgot to
// include the configured default namespace.
func TestAllowlistBlocksDefaultWhenNotListed(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNamespaces = []string{"prod"}
	p := newProvider(t, cfg)

	_, err := p.ResolveNamespace(context.Background(), "")
	require.Error(t, err, "the configured default must still be checked against the allowlist")
}

func TestAllowlistIsCaseSensitive(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedNamespaces = []string{"prod"}
	p := newProvider(t, cfg)

	_, err := p.ResolveNamespace(context.Background(), "PROD")
	require.Error(t, err, "Nomad namespaces are case sensitive, so the allowlist must be too")
}

// --- region ---------------------------------------------------------------

func TestResolveRegion(t *testing.T) {
	cfg := testConfig()
	cfg.NomadRegion = "configured"
	p := newProvider(t, cfg)

	require.Equal(t, "explicit", p.ResolveRegion(context.Background(), "explicit"))
	require.Equal(t, "from-header", p.ResolveRegion(WithRegion(context.Background(), "from-header"), ""))
	require.Equal(t, "configured", p.ResolveRegion(context.Background(), ""))
}

func TestResolveRegionEmptyMeansAgentRegion(t *testing.T) {
	p := newProvider(t, testConfig())
	require.Empty(t, p.ResolveRegion(context.Background(), ""),
		"an empty region lets Nomad use the agent's own region")
}

// --- token handling -------------------------------------------------------

func TestResolveTokenPrefersRequestToken(t *testing.T) {
	cfg := testConfig()
	cfg.NomadToken = "configured-token-value"
	p := newProvider(t, cfg)

	require.Equal(t, "per-request-token", p.resolveToken(WithToken(context.Background(), "per-request-token")))
	require.Equal(t, "configured-token-value", p.resolveToken(context.Background()))
}

// TestSessionsWithDifferentTokensGetDifferentClients is the isolation property:
// two MCP sessions authenticating differently must never share a client.
func TestSessionsWithDifferentTokensGetDifferentClients(t *testing.T) {
	p := newProvider(t, testConfig())

	a, err := p.clientFor("session-a", "token-aaaaaaaa")
	require.NoError(t, err)
	b, err := p.clientFor("session-b", "token-bbbbbbbb")
	require.NoError(t, err)

	require.NotSame(t, a, b)

	entryA, ok := p.load("session-a")
	require.True(t, ok)
	entryB, ok := p.load("session-b")
	require.True(t, ok)
	require.NotEqual(t, entryA.tokenHash, entryB.tokenHash)
}

// TestSessionCacheStoresOnlyTokenHashes: a heap dump of the cache must not
// yield usable credentials.
func TestSessionCacheStoresOnlyTokenHashes(t *testing.T) {
	p := newProvider(t, testConfig())

	const secret = "3f9a1c2e-7b4d-4a11-9e2f-6c8d0b5a7e13"
	_, err := p.clientFor("session-a", secret)
	require.NoError(t, err)

	entry, ok := p.load("session-a")
	require.True(t, ok)
	require.Equal(t, hashToken(secret), entry.tokenHash)
	require.NotContains(t, string(entry.tokenHash[:]), secret)
}

func TestEndSessionReleasesClient(t *testing.T) {
	p := newProvider(t, testConfig())

	_, err := p.clientFor("session-a", "token-aaaaaaaa")
	require.NoError(t, err)
	_, ok := p.load("session-a")
	require.True(t, ok)

	p.EndSession("session-a")
	_, ok = p.load("session-a")
	require.False(t, ok, "a finished session must not leak its client")

	require.NotPanics(t, func() { p.EndSession("never-existed") })
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	require.Equal(t, hashToken("abc"), hashToken("abc"))
	require.NotEqual(t, hashToken("abc"), hashToken("abd"))
}
