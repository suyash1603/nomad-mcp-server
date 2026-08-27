// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// TestSignaturesMatchRealFailureText checks each signature against text in the
// shape Nomad actually records. These are the events a task emits when it never
// starts — the case where there are no logs to read, which is the whole reason
// the tool exists.
func TestSignaturesMatchRealFailureText(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"vault: failed to derive token: Permission denied", "vault-token"},
		{"Vault token lookup failed: missing policy nomad-workloads", "vault-token"},
		{"Template failed: vault.read(secret/data/app): permission denied", "template-render"},
		{"Missing: template render timeout", "template-render"},
		{"envoy: bootstrap failed after 3 attempts", "consul-connect"},
		{"connect proxy sidecar exited with code 1", "consul-connect"},
		{"failed to register service with Consul: connection refused", "consul-registration"},
		{"consul: deregistration error", "consul-registration"},
		{"workload identity JWT rejected: invalid signature", "identity-workload"},
		{"signed identity expired", "identity-workload"},
	}

	for _, tc := range cases {
		t.Run(tc.want+"/"+tc.text[:20], func(t *testing.T) {
			var matched []string
			for _, sig := range signatures {
				if sig.match.MatchString(tc.text) {
					matched = append(matched, sig.category)
				}
			}
			require.Contains(t, matched, tc.want, "text did not match its signature: %q", tc.text)
		})
	}
}

// TestSignaturesDoNotMatchOrdinaryEvents is the more important half. A
// signature that fires on healthy events would report a broken integration on
// every cluster.
func TestSignaturesDoNotMatchOrdinaryEvents(t *testing.T) {
	benign := []string{
		"Task started by client",
		"Building Task Directory",
		"Task received by client",
		"Downloading image",
		"Sent interrupt. Waiting 5s before force killing",
		"Restart Signaled",
		"Template re-rendered",
		"Task exited successfully",
	}

	for _, text := range benign {
		t.Run(text[:12], func(t *testing.T) {
			for _, sig := range signatures {
				require.False(t, sig.match.MatchString(text),
					"signature %q fired on a benign event %q", sig.category, text)
			}
		})
	}
}

func TestSignaturesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, sig := range signatures {
		require.NotEmpty(t, sig.category)
		require.False(t, seen[sig.category], "duplicate signature category %q", sig.category)
		seen[sig.category] = true

		require.NotNil(t, sig.match)
		require.NotEmpty(t, sig.summary)
		require.NotEmpty(t, sig.detail)
		// Every finding has to tell the reader where to go next, because this
		// tool deliberately stops at Nomad's boundary.
		require.NotEmpty(t, sig.nextStep, "signature %q has no next step", sig.category)
	}
}

func TestEventTextGathersEveryField(t *testing.T) {
	got := eventText(&api.TaskEvent{
		Type:           "Task Setup",
		DisplayMessage: "display",
		VaultError:     "vault boom",
		DriverError:    "driver boom",
	})

	for _, want := range []string{"display", "vault boom", "driver boom", "Task Setup"} {
		require.Contains(t, got, want)
	}
	require.Empty(t, eventText(&api.TaskEvent{}))
}

// TestVaultConfigReadsOnlyTheSafeFields is a security test. The agent's Vault
// block sits beside a token and TLS material, and get_agent_config refuses to
// expose that block at all; nothing here may widen it.
func TestVaultConfigReadsOnlyTheSafeFields(t *testing.T) {
	cfg := map[string]any{
		"Vaults": []any{map[string]any{
			"Enabled":     true,
			"Name":        "default",
			"Token":       "s.SUPERSECRET",
			"Addr":        "https://vault.internal:8200",
			"TLSCertFile": "/etc/vault/cert.pem",
			"Role":        "nomad-workloads",
		}},
	}

	enabled, names := vaultConfig(cfg)

	require.NotNil(t, enabled)
	require.True(t, *enabled)
	require.Equal(t, []string{"default"}, names)

	// Nothing else may escape. Asserting on the values directly is the point:
	// a future change that started returning the address would fail here.
	for _, leaked := range []string{"s.SUPERSECRET", "vault.internal", "/etc/vault/cert.pem", "nomad-workloads"} {
		require.NotContains(t, names, leaked)
	}
}

func TestVaultConfigHandlesBothShapes(t *testing.T) {
	t.Run("newer plural form", func(t *testing.T) {
		enabled, _ := vaultConfig(map[string]any{
			"Vaults": []any{map[string]any{"Enabled": true, "Name": "a"}},
		})
		require.NotNil(t, enabled)
		require.True(t, *enabled)
	})

	t.Run("older singular form", func(t *testing.T) {
		enabled, _ := vaultConfig(map[string]any{
			"Vault": map[string]any{"Enabled": true},
		})
		require.NotNil(t, enabled)
		require.True(t, *enabled)
	})

	t.Run("absent means unknown, not disabled", func(t *testing.T) {
		// nil and false mean different things: one is "the token could not read
		// the config", the other is "Vault is off". Collapsing them would have
		// the tool assert something it does not know.
		enabled, _ := vaultConfig(map[string]any{})
		require.Nil(t, enabled)
	})

	t.Run("a placeholder block with no Enabled key reads as not enabled", func(t *testing.T) {
		enabled, _ := vaultConfig(map[string]any{
			"Vaults": []any{map[string]any{"Name": "default", "Addr": "http://x"}},
		})
		require.NotNil(t, enabled)
		require.False(t, *enabled)
	})

	t.Run("any enabled cluster counts as enabled", func(t *testing.T) {
		enabled, names := vaultConfig(map[string]any{
			"Vaults": []any{
				map[string]any{"Enabled": false, "Name": "b"},
				map[string]any{"Enabled": true, "Name": "a"},
			},
		})
		require.True(t, *enabled)
		require.Equal(t, []string{"a", "b"}, names, "names are sorted for stable output")
	})
}

func TestConsulCount(t *testing.T) {
	require.Equal(t, 2, consulCount(map[string]any{"Consuls": []any{map[string]any{}, map[string]any{}}}))
	require.Equal(t, 1, consulCount(map[string]any{"Consul": map[string]any{}}))
	require.Equal(t, 0, consulCount(map[string]any{}))
}

func TestIntegrationFindingsAggregateByCategory(t *testing.T) {
	hits := map[string]*signatureHits{
		"vault-token": {
			sig:     signatures[0],
			allocs:  map[string]bool{"aaaaaaaa-1111-2222-3333-444444444444": true, "bbbbbbbb-1111-2222-3333-444444444444": true},
			samples: []string{"web: vault denied"},
		},
	}

	got := integrationFindings(hits, 5)

	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Count)
	require.Len(t, got[0].Examples, 2)
	require.Contains(t, got[0].Detail, "vault denied", "the observed text belongs in the finding")
	require.NotEmpty(t, got[0].NextStep)
}

func TestIntegrationFindingsRespectExampleCap(t *testing.T) {
	allocs := map[string]bool{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		allocs[id] = true
	}
	got := integrationFindings(map[string]*signatureHits{
		"vault-token": {sig: signatures[0], allocs: allocs},
	}, 3)

	require.Equal(t, 7, got[0].Count, "the count is the true total")
	require.Len(t, got[0].Examples, 3, "examples are capped")
}

func TestIntegrationsNoteNeverClaimsProofOfAbsence(t *testing.T) {
	note := integrationsNote(integrationsResult{AllocsScanned: 40, Count: 0})
	require.Contains(t, note, "not proof nothing ever went wrong")
	// And it must always say where its boundary is.
	require.Contains(t, note, "reads Nomad only")
}

func TestIntegrationsNoteFlagsDisabledVault(t *testing.T) {
	off := false
	note := integrationsNote(integrationsResult{AllocsScanned: 1, VaultEnabled: &off})
	require.Contains(t, note, "NOT enabled")
	require.NotContains(t, note, "configured on this cluster but")
}

func TestIntegrationsNoteFlagsUnreadableConfig(t *testing.T) {
	note := integrationsNote(integrationsResult{AllocsScanned: 1, VaultEnabled: nil})
	require.Contains(t, note, "agent:read")
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "abc", truncate("abc", 5))
	require.Equal(t, "ab…", truncate("abcdef", 2))
}
