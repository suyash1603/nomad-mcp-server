// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// nomadReturning starts a stub Nomad API that answers every request with the
// given status and body, and returns a client pointed at it.
//
// Errors are produced by driving the real github.com/hashicorp/nomad/api client
// rather than by hand-constructing api.UnexpectedResponseError, whose fields are
// unexported. That means these tests exercise the same error values the tools
// will actually see.
func nomadReturning(t *testing.T, status int, body string) *api.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	cfg := api.DefaultConfig()
	cfg.Address = srv.URL
	c, err := api.NewClient(cfg)
	require.NoError(t, err)
	return c
}

// jobsListErr performs a real API call that is expected to fail.
func jobsListErr(t *testing.T, c *api.Client) error {
	t.Helper()
	_, _, err := c.Jobs().List(nil)
	require.Error(t, err)
	return err
}

func TestStatusCodeFromRealClientError(t *testing.T) {
	// Bodies verified against Nomad 2.0.5 OSS.
	cases := []struct {
		status int
		body   string
	}{
		{403, "Permission denied"},
		{404, "job not found"},
		{501, "Nomad Enterprise only endpoint"},
		{400, "failed to read filter expression"},
		{500, "rpc error: no leader"},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := jobsListErr(t, nomadReturning(t, tc.status, tc.body))

			code, body, ok := StatusCode(err)
			require.True(t, ok, "should recognise a Nomad API error")
			require.Equal(t, tc.status, code)
			require.Contains(t, body, tc.body)
		})
	}
}

func TestStatusCodeIgnoresPlainErrors(t *testing.T) {
	_, _, ok := StatusCode(errors.New("something else went wrong"))
	require.False(t, ok)

	_, _, ok = StatusCode(nil)
	require.False(t, ok)
}

// TestStatusCodeTextualFallback covers the Nomad client code paths that format
// the status into a plain error string instead of returning the typed error.
func TestStatusCodeTextualFallback(t *testing.T) {
	err := fmt.Errorf("failed to query: Unexpected response code: 404 (job not found)")

	code, body, ok := StatusCode(err)
	require.True(t, ok)
	require.Equal(t, 404, code)
	require.Equal(t, "job not found", body)
}

func TestForbiddenNamesTheCapabilityAndNamespace(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 403, "Permission denied"))

	msg := MapError(err, ErrorContext{
		Op:         "list jobs",
		Namespace:  "prod",
		Capability: "list-jobs",
	}, nil)

	// Nomad's body is only "Permission denied"; everything useful must come
	// from the context the tool supplied.
	require.Contains(t, msg, "list-jobs")
	require.Contains(t, msg, "prod")
	require.Contains(t, msg, "NOMAD_TOKEN")
	require.NotContains(t, msg, "Unexpected response code")
}

func TestForbiddenWithoutCapabilityStillExplains(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 403, "Permission denied"))
	msg := MapError(err, ErrorContext{Op: "list jobs"}, nil)
	require.Contains(t, msg, "NOMAD_TOKEN")
	require.Contains(t, msg, "lacks the required permission")
}

func TestNotFoundSuggestsAListTool(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 404, "job not found"))

	msg := MapError(err, ErrorContext{
		Op:        "read job",
		Kind:      "job",
		Name:      "web",
		Namespace: "prod",
		ListTool:  "list_jobs",
	}, nil)

	require.Contains(t, msg, `No job named "web"`)
	require.Contains(t, msg, `namespace "prod"`)
	require.Contains(t, msg, "list_jobs")
}

func TestEnterpriseOnlyIsExplained(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 501, "Nomad Enterprise only endpoint"))

	msg := MapError(err, ErrorContext{Op: "list quotas", Address: "http://127.0.0.1:4646"}, nil)

	require.Contains(t, msg, "Nomad Enterprise")
	require.Contains(t, msg, "Community Edition")
	require.NotContains(t, msg, "501")
	require.True(t, IsEnterpriseOnly(err))
}

func TestUnauthorizedMentionsTokenEnvVar(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 401, "ACL token required"))
	msg := MapError(err, ErrorContext{Op: "list jobs"}, nil)
	require.Contains(t, msg, "NOMAD_TOKEN")
}

func TestServerErrorSuggestsClusterStatus(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 500, "rpc error: no leader"))
	msg := MapError(err, ErrorContext{Op: "list jobs"}, nil)
	require.Contains(t, msg, "get_cluster_status")
}

func TestBadRequestSurfacesNomadExplanation(t *testing.T) {
	body := "failed to read filter expression: 1:4 (3): no match found"
	err := jobsListErr(t, nomadReturning(t, 400, body))
	msg := MapError(err, ErrorContext{Op: "list jobs"}, nil)
	require.Contains(t, msg, "no match found")
}

// TestConnectionRefused is the error every new user hits first, so its message
// has to name the address and the likely cause.
func TestConnectionRefused(t *testing.T) {
	cfg := api.DefaultConfig()
	// Port 1 is reserved and never listening.
	cfg.Address = "http://127.0.0.1:1"
	c, err := api.NewClient(cfg)
	require.NoError(t, err)

	_, _, callErr := c.Jobs().List(nil)
	require.Error(t, callErr)

	msg := MapError(callErr, ErrorContext{Op: "list jobs", Address: cfg.Address}, nil)
	require.Contains(t, msg, "http://127.0.0.1:1")
	require.Contains(t, msg, "Is the Nomad agent running")
}

func TestUnresolvableHost(t *testing.T) {
	cfg := api.DefaultConfig()
	cfg.Address = "http://nomad.invalid.this-tld-does-not-exist:4646"
	c, err := api.NewClient(cfg)
	require.NoError(t, err)

	_, _, callErr := c.Jobs().List(nil)
	require.Error(t, callErr)

	msg := MapError(callErr, ErrorContext{Op: "list jobs", Address: cfg.Address}, nil)
	require.Contains(t, msg, "could not be resolved")
	require.Contains(t, msg, "NOMAD_ADDR")
}

// TestMapErrorNeverLeaksToken checks the whole pipeline: a token echoed back
// inside a Nomad error body must not reach the model.
func TestMapErrorNeverLeaksToken(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 400, "bad token NOMAD_TOKEN="+sampleToken))

	msg := MapError(err, ErrorContext{Op: "list jobs"}, NewRedactor(sampleToken))

	require.NotContains(t, msg, sampleToken)
	require.Contains(t, msg, Placeholder)
}

func TestMapErrorHandlesNilRedactor(t *testing.T) {
	err := jobsListErr(t, nomadReturning(t, 400, "nope"))
	require.NotPanics(t, func() {
		_ = MapError(err, ErrorContext{Op: "list jobs"}, nil)
	})
}

func TestMapErrorNil(t *testing.T) {
	require.Empty(t, MapError(nil, ErrorContext{Op: "list jobs"}, nil))
}

// TestMapErrorNeverReturnsRawGoError asserts the invariant: no bare Go error
// ever reaches the model.
func TestMapErrorNeverReturnsRawGoError(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405, 409, 429, 500, 501, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := jobsListErr(t, nomadReturning(t, status, "some body"))
			msg := MapError(err, ErrorContext{
				Op: "do the thing", Kind: "job", Name: "web", Namespace: "default",
			}, nil)

			require.NotEmpty(t, msg)
			require.NotContains(t, msg, "Unexpected response code",
				"the raw Nomad client error must not reach the model")
		})
	}
}

func TestIsNotFoundAndIsForbidden(t *testing.T) {
	notFound := jobsListErr(t, nomadReturning(t, 404, "job not found"))
	require.True(t, IsNotFound(notFound))
	require.False(t, IsForbidden(notFound))

	forbidden := jobsListErr(t, nomadReturning(t, 403, "Permission denied"))
	require.True(t, IsForbidden(forbidden))
	require.False(t, IsNotFound(forbidden))

	require.False(t, IsNotFound(errors.New("plain")))
	require.False(t, IsEnterpriseOnly(errors.New("plain")))
}
