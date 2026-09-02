// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestValidateTokenShape(t *testing.T) {
	t.Run("a client token needs something to grant", func(t *testing.T) {
		err := validateTokenShape(TypeClient, nil, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least one policy or role")
	})

	t.Run("a policy is enough, and so is a role", func(t *testing.T) {
		require.NoError(t, validateTokenShape(TypeClient, []string{"deploy"}, nil))
		require.NoError(t, validateTokenShape(TypeClient, nil, []string{"deployers"}))
	})

	t.Run("a management token cannot carry policies", func(t *testing.T) {
		// Nomad rejects this too, but its message is terse enough that a model
		// tends to retry it rather than reconsider the type.
		err := validateTokenShape(TypeManagement, []string{"deploy"}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "ignores them entirely")
		require.Contains(t, err.Error(), "client", "the message must name the likely intent")
	})

	t.Run("a bare management token is legal", func(t *testing.T) {
		require.NoError(t, validateTokenShape(TypeManagement, nil, nil))
	})

	t.Run("an unknown type is rejected by name", func(t *testing.T) {
		err := validateTokenShape("admin", nil, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), `"admin"`)
	})
}

func TestParseTTL(t *testing.T) {
	t.Run("empty means no expiry", func(t *testing.T) {
		d, err := parseTTL("")
		require.NoError(t, err)
		require.Zero(t, d)
	})

	t.Run("a duration parses", func(t *testing.T) {
		d, err := parseTTL(" 8h ")
		require.NoError(t, err)
		require.Equal(t, 8*time.Hour, d)
	})

	t.Run("nonsense is rejected with an example", func(t *testing.T) {
		_, err := parseTTL("eight hours")
		require.Error(t, err)
		require.Contains(t, err.Error(), `"8h"`)
	})

	t.Run("zero and negative are rejected", func(t *testing.T) {
		// "0s" reads like a request for an immediately-expired token, which
		// Nomad would accept as no expiry at all — the opposite of the intent.
		for _, v := range []string{"0s", "-1h"} {
			_, err := parseTTL(v)
			require.Error(t, err, "parseTTL(%q) should fail", v)
			require.Contains(t, err.Error(), "greater than zero")
		}
	})
}

func TestFormatExpiry(t *testing.T) {
	t.Run("no expiry", func(t *testing.T) {
		s, expired := formatExpiry(nil)
		require.Empty(t, s)
		require.False(t, expired)

		var zero time.Time
		s, expired = formatExpiry(&zero)
		require.Empty(t, s)
		require.False(t, expired)
	})

	t.Run("a past expiry is flagged", func(t *testing.T) {
		past := time.Now().Add(-time.Hour)
		s, expired := formatExpiry(&past)
		require.NotEmpty(t, s)
		require.True(t, expired)
	})

	t.Run("a future expiry is not", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		_, expired := formatExpiry(&future)
		require.False(t, expired)
	})
}

func TestLinkFlattening(t *testing.T) {
	t.Run("role links prefer the name and fall back to the ID", func(t *testing.T) {
		got := roleLinkNames([]*api.ACLTokenRoleLink{
			{Name: "deployers", ID: "an-id"},
			{ID: "only-an-id"},
			nil,
			{},
		})
		require.Equal(t, []string{"deployers", "only-an-id"}, got)
	})

	t.Run("policy links drop the empties", func(t *testing.T) {
		got := policyLinkNames([]*api.ACLRolePolicyLink{{Name: "deploy"}, nil, {Name: ""}})
		require.Equal(t, []string{"deploy"}, got)
	})

	t.Run("building links trims and drops blanks", func(t *testing.T) {
		require.Equal(t,
			[]*api.ACLTokenRoleLink{{Name: "a"}, {Name: "b"}},
			roleLinks([]string{" a ", "", "b", "   "}))
		require.Equal(t,
			[]*api.ACLRolePolicyLink{{Name: "a"}},
			policyLinks([]string{"a", " "}))
	})
}

func TestJobACLProjection(t *testing.T) {
	require.Nil(t, jobACLProjection(nil))
	require.Nil(t, jobACLProjection(&api.JobACL{}),
		"an all-empty attachment is no attachment; emitting an empty object implies one exists")

	require.Equal(t,
		map[string]any{"namespace": "default", "job_id": "web"},
		jobACLProjection(&api.JobACL{Namespace: "default", JobID: "web"}))
}

func TestDisabledMessageTellsTheOperatorWhatToDo(t *testing.T) {
	msg := disabledMessage("create_acl_token")

	require.Contains(t, msg, "create_acl_token")
	require.Contains(t, msg, "NOMAD_MCP_ENABLE_ACL=true")
	require.Contains(t, msg, "--enable-acl=true")
	// A model that is merely told "no" goes looking for another route. Here
	// the route it would find is the user's terminal, so the message has to
	// close that door explicitly.
	require.Contains(t, msg, "do not retry")
	require.Contains(t, msg, "nomad acl")
}
