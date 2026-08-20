// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A realistic Nomad ACL token: a v4 UUID.
const sampleToken = "3f9a1c2e-7b4d-4a11-9e2f-6c8d0b5a7e13"

func TestRedactsKnownLiteralToken(t *testing.T) {
	r := NewRedactor(sampleToken)

	got := r.String("connecting with token " + sampleToken + " to nomad")
	require.NotContains(t, got, sampleToken)
	require.Contains(t, got, Placeholder)
}

func TestRedactsLabelledSecrets(t *testing.T) {
	// Nothing here is a token this process knows about; the label alone must
	// be enough, because tokens turn up inside Nomad error bodies.
	r := NewRedactor()

	cases := []string{
		"NOMAD_TOKEN=" + sampleToken,
		"NOMAD_TOKEN: " + sampleToken,
		`{"SecretID":"` + sampleToken + `"}`,
		"X-Nomad-Token: " + sampleToken,
		"secret_id = " + sampleToken,
		"Authorization: Bearer " + sampleToken,
	}

	for _, in := range cases {
		t.Run(in[:min(len(in), 24)], func(t *testing.T) {
			got := r.String(in)
			require.NotContains(t, got, sampleToken, "token survived redaction in %q", in)
			require.Contains(t, got, Placeholder)
		})
	}
}

// TestKeepsAccessorID guards a deliberate exception. An AccessorID identifies a
// token but cannot authenticate as one, and it is often the only clue about
// which token was used.
func TestKeepsAccessorID(t *testing.T) {
	r := NewRedactor()
	in := `{"AccessorID":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`
	require.Equal(t, in, r.String(in))
}

// TestKeepsResourceUUIDs is the most important test in this file. Nomad
// allocation, evaluation, deployment and node IDs are all UUIDs. A redactor
// that scrubbed bare UUIDs would destroy exactly the identifiers needed to
// debug anything, so it must not.
func TestKeepsResourceUUIDs(t *testing.T) {
	r := NewRedactor(sampleToken)

	allocID := "7c1f4b3a-2e5d-4c6b-8a90-1d2e3f4a5b6c"
	in := "allocation " + allocID + " failed on node 9e8d7c6b-5a4f-3e2d-1c0b-a9b8c7d6e5f4"

	require.Equal(t, in, r.String(in),
		"resource UUIDs must survive redaction or debugging becomes impossible")
}

func TestIgnoresShortSecrets(t *testing.T) {
	// A literal this short would corrupt unrelated text if replaced globally.
	r := NewRedactor("abc")
	require.Equal(t, "abcdef", r.String("abcdef"))
}

func TestIgnoresEmptySecret(t *testing.T) {
	r := NewRedactor("", "   ")
	require.Equal(t, "hello", r.String("hello"))
}

func TestRedactError(t *testing.T) {
	r := NewRedactor(sampleToken)

	require.Empty(t, r.Error(nil))

	err := errors.New("request failed with NOMAD_TOKEN=" + sampleToken)
	require.NotContains(t, r.Error(err), sampleToken)
}

func TestRedactFields(t *testing.T) {
	r := NewRedactor(sampleToken)

	out := r.Fields(map[string]any{
		"msg":   "using " + sampleToken,
		"count": 3,
		"ok":    true,
	})

	require.NotContains(t, out["msg"].(string), sampleToken)
	require.Equal(t, 3, out["count"], "non-string values must pass through untouched")
	require.Equal(t, true, out["ok"])
}

func TestRedactorIsIdempotent(t *testing.T) {
	r := NewRedactor(sampleToken)
	once := r.String("NOMAD_TOKEN=" + sampleToken)
	require.Equal(t, once, r.String(once))
}

func TestEmptyStringUnchanged(t *testing.T) {
	require.Equal(t, "", NewRedactor(sampleToken).String(""))
}

// TestRedactionSurvivesMultipleOccurrences catches a redactor that only
// replaces the first match.
func TestRedactionSurvivesMultipleOccurrences(t *testing.T) {
	r := NewRedactor(sampleToken)
	in := sampleToken + " and again " + sampleToken
	got := r.String(in)
	require.NotContains(t, got, sampleToken)
	require.Equal(t, 2, strings.Count(got, Placeholder))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
