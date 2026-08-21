// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// echoHandler records what the middleware put into the request context.
type echoHandler struct {
	called    bool
	token     string
	namespace string
	region    string
}

func (e *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.called = true
	ctx := r.Context()
	e.token, _ = ctx.Value(ctxKeyToken).(string)
	e.namespace, _ = ctx.Value(ctxKeyNamespace).(string)
	e.region, _ = ctx.Value(ctxKeyRegion).(string)
	w.WriteHeader(http.StatusOK)
}

// --- CORS -----------------------------------------------------------------

func TestStrictModeRejectsCrossOrigin(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{Mode: "strict"}, quietLogger())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, next.called, "a rejected origin must never reach the MCP handler")
}

// TestStrictModeAllowsNoOriginHeader: a native MCP client is not a browser and
// sends no Origin. Rejecting those would break every non-browser client.
func TestStrictModeAllowsNoOriginHeader(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{Mode: "strict"}, quietLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, next.called)
}

func TestStrictModeAllowsListedOrigin(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{
		Mode:           "strict",
		AllowedOrigins: []string{"https://app.example.com"},
	}, quietLogger())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, next.called)
	require.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, rec.Header().Values("Vary"), "Origin",
		"responses vary by Origin and must not be cached across origins")
}

func TestDevelopmentModeAllowsLocalhost(t *testing.T) {
	origins := []string{
		"http://localhost:3000",
		"https://localhost:8443",
		"http://127.0.0.1:6274",
		"http://[::1]:3000",
	}

	for _, origin := range origins {
		t.Run(origin, func(t *testing.T) {
			next := &echoHandler{}
			h := NewSecurityHandler(next, CORSConfig{Mode: "development"}, quietLogger())

			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// TestDevelopmentModeStillRejectsRemote: "development" relaxes localhost, not
// everything.
func TestDevelopmentModeStillRejectsRemote(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{Mode: "development"}, quietLogger())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, next.called)
}

// TestLocalhostPrefixIsNotSubstringMatched catches the classic bug where
// "http://localhost:" matching is done loosely enough that
// "http://localhost.evil.com" passes.
func TestLocalhostPrefixIsNotSubstringMatched(t *testing.T) {
	for _, origin := range []string{
		"http://localhost.evil.com",
		"https://127.0.0.1.evil.com",
		"http://notlocalhost:3000",
	} {
		t.Run(origin, func(t *testing.T) {
			next := &echoHandler{}
			h := NewSecurityHandler(next, CORSConfig{Mode: "development"}, quietLogger())

			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s must not be treated as localhost", origin)
		})
	}
}

func TestDisabledModeAllowsEverything(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{Mode: "disabled"}, quietLogger())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestPreflightShortCircuits(t *testing.T) {
	next := &echoHandler{}
	h := NewSecurityHandler(next, CORSConfig{
		Mode:           "strict",
		AllowedOrigins: []string{"https://app.example.com"},
	}, quietLogger())

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, next.called, "a preflight must not be forwarded to the MCP handler")
	require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), NomadTokenHeader)
}

func TestLoadCORSConfigDefaultsToStrict(t *testing.T) {
	got := LoadCORSConfig(&config.Config{})
	require.Equal(t, config.DefaultCORSMode, got.Mode)
	require.Equal(t, "strict", got.Mode, "the default must be the restrictive one")
}

// --- context middleware ---------------------------------------------------

func TestHeadersReachTheContext(t *testing.T) {
	next := &echoHandler{}
	h := NomadContextMiddleware(quietLogger())(next)

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set(NomadTokenHeader, "header-token")
	req.Header.Set(NomadNamespaceHeader, "header-ns")
	req.Header.Set(NomadRegionHeader, "header-region")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.True(t, next.called)
	require.Equal(t, "header-token", next.token)
	require.Equal(t, "header-ns", next.namespace)
	require.Equal(t, "header-region", next.region)
}

func TestHeaderLookupIsCaseInsensitive(t *testing.T) {
	next := &echoHandler{}
	h := NomadContextMiddleware(quietLogger())(next)

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("x-nomad-token", "header-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)
	require.Equal(t, "header-token", next.token)
}

// TestTokenInQueryStringIsRejected: a token in a URL leaks into proxy logs,
// browser history and Referer headers. Rejecting loudly beats ignoring.
func TestTokenInQueryStringIsRejected(t *testing.T) {
	// The spellings matter more than the count. These lookups used to be
	// literal url.Values.Get calls, which are case-sensitive, so "nomad_token"
	// sailed through while "NOMAD_TOKEN" and "token" were both blocked — and
	// the original version of this test happened to list only the ones that
	// worked. The e2e HTTP suite caught it.
	for _, param := range []string{
		"X-Nomad-Token", "x-nomad-token", "NOMAD_TOKEN", "nomad_token", "nomadToken",
		"token", "TOKEN", "Token", "secret_id", "secretID", "SecretId",
		"acl_token", "auth_token",
	} {
		t.Run(param, func(t *testing.T) {
			next := &echoHandler{}
			h := NomadContextMiddleware(quietLogger())(next)

			req := httptest.NewRequest(http.MethodPost, "/mcp?"+param+"=leaked-token", nil)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.False(t, next.called)
			require.NotContains(t, rec.Body.String(), "leaked-token",
				"the rejection must not echo the token back")
		})
	}
}

// TestAddressInQueryStringIsRejected closes an SSRF hole: an attacker-supplied
// address would make this server send its Nomad token to a host of their choice.
func TestAddressInQueryStringIsRejected(t *testing.T) {
	for _, param := range []string{
		"NOMAD_ADDR", "nomad_addr", "nomadAddr", "address", "ADDRESS", "addr",
	} {
		t.Run(param, func(t *testing.T) {
			next := &echoHandler{}
			h := NomadContextMiddleware(quietLogger())(next)

			req := httptest.NewRequest(http.MethodPost, "/mcp?"+param+"=http://evil.example.com", nil)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.False(t, next.called)
		})
	}
}

// TestValuelessParameterIsNotRejected keeps the guard from overreaching. A bare
// "?token" with nothing after it discloses nothing, and refusing it would be a
// confusing 400 for a request that did nothing wrong.
func TestValuelessParameterIsNotRejected(t *testing.T) {
	for _, query := range []string{"?token", "?token=", "?token=%20"} {
		t.Run(query, func(t *testing.T) {
			next := &echoHandler{}
			h := NomadContextMiddleware(quietLogger())(next)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp"+query, nil))

			require.True(t, next.called, "an empty parameter is not a leak")
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// TestUnrelatedParametersPassThrough guards the other direction: normalising
// names must not start matching things that are not credentials.
func TestUnrelatedParametersPassThrough(t *testing.T) {
	for _, param := range []string{"namespace", "region", "job_id", "next_token", "per_page"} {
		t.Run(param, func(t *testing.T) {
			next := &echoHandler{}
			h := NomadContextMiddleware(quietLogger())(next)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp?"+param+"=value", nil))

			require.True(t, next.called, "%s is not a credential and must not be refused", param)
		})
	}
}

func TestNoHeadersLeavesContextClean(t *testing.T) {
	next := &echoHandler{}
	h := NomadContextMiddleware(quietLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	require.True(t, next.called)
	require.Empty(t, next.token)
	require.Empty(t, next.namespace)
}

// --- logging middleware ---------------------------------------------------

func TestLoggingMiddlewarePassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})
	h := LoggingMiddleware(quietLogger())(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	require.Equal(t, http.StatusTeapot, rec.Code)
	require.Equal(t, "hi", rec.Body.String())
}

// TestStatusRecorderSupportsFlush: streamable-HTTP uses SSE, which needs the
// wrapped writer to remain flushable.
func TestStatusRecorderSupportsFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	require.NotPanics(t, func() { s.Flush() })
	require.Implements(t, (*http.Flusher)(nil), s)
}

func TestStatusRecorderKeepsFirstStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	s.WriteHeader(http.StatusNotFound)
	s.WriteHeader(http.StatusInternalServerError)

	require.Equal(t, http.StatusNotFound, s.status)
}
