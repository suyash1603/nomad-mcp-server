// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package nomadtest is a fake Nomad HTTP API for unit tests.
//
// The tools are thin: they call the Nomad Go client, project the result and map
// the error. Almost every bug they can have is in the projection or the
// mapping, and both are exercised by feeding the real api.Client canned
// responses. That is what this package is for — it stands up an httptest server
// speaking enough of Nomad's wire protocol that the genuine client talks to it
// without knowing the difference.
//
// Two things it deliberately does rather than being a simple stub map:
//
//   - It records every request. A tool that quietly drops the namespace,
//     forgets to forward a filter, or never sends the pagination token still
//     returns plausible-looking output, so the assertions that matter are often
//     about what went out rather than what came back.
//   - It defaults to a small but internally consistent cluster: one server, one
//     client node, a healthy job and a stuck one, with allocations and
//     evaluations that refer to each other by the same IDs. Tests that need
//     something else override a single route instead of building a world.
package nomadtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Well-known IDs from the default fixture.
//
// They are exported so tests can refer to the fixture without copying UUIDs
// around, and they are obviously synthetic so nobody mistakes one for a real
// cluster's identifier.
const (
	NodeID       = "11111111-1111-1111-1111-111111111111"
	NodeName     = "client-1"
	HealthyJob   = "web"
	StuckJob     = "stuck"
	AllocID      = "22222222-2222-2222-2222-222222222222"
	FailedAlloc  = "33333333-3333-3333-3333-333333333333"
	EvalID       = "44444444-4444-4444-4444-444444444444"
	BlockedEval  = "55555555-5555-5555-5555-555555555555"
	DeploymentID = "66666666-6666-6666-6666-666666666666"
)

// Request is one call the fake received.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

// Namespace returns the namespace the caller asked for, or "" if it did not.
func (r Request) Namespace() string { return r.Query.Get("namespace") }

// Token returns the ACL token the caller sent, or "" if it sent none.
func (r Request) Token() string { return r.Header.Get("X-Nomad-Token") }

// Server is a fake Nomad agent.
type Server struct {
	*httptest.Server

	t *testing.T

	mu       sync.Mutex
	requests []Request
	routes   map[string]http.HandlerFunc
}

// New starts a fake Nomad agent preloaded with the default fixture.
//
// It is closed automatically when the test finishes.
func New(t *testing.T) *Server {
	t.Helper()

	s := &Server{t: t, routes: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)

	s.loadDefaults()
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   string(body),
	})
	handler, ok := s.routes[r.Method+" "+r.URL.Path]
	if !ok {
		handler, ok = s.routes[r.URL.Path]
	}
	s.mu.Unlock()

	if !ok {
		// Nomad's own wording. Several tools branch on the body text, so an
		// invented message here would make a passing test meaningless.
		notFound(w, "not found")
		return
	}
	handler(w, r)
}

// Handle installs a raw handler for a path.
//
// The path may be "/v1/jobs" to match any method, or "POST /v1/jobs" to match
// one. A method-qualified route wins over a bare one.
func (s *Server) Handle(path string, fn http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = fn
}

// JSON installs a route that returns body as JSON with a complete set of
// Nomad's query-meta headers.
func (s *Server) JSON(path string, body any) {
	s.Handle(path, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, body, "")
	})
}

// Page installs a list route that returns body plus a next-page token, so a
// test can prove a tool surfaces pagination rather than silently truncating.
func (s *Server) Page(path string, body any, nextToken string) {
	s.Handle(path, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, body, nextToken)
	})
}

// Status installs a route that fails with a status and a body.
//
// Use the Nomad* constants below for the bodies Nomad actually sends; the
// error mapping keys off them.
func (s *Server) Status(path string, code int, body string) {
	s.Handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
}

// Bodies Nomad returns, verified against a live agent.
const (
	BodyJobNotFound   = "job not found"
	BodyAllocNotFound = "alloc not found"
	BodyDenied        = "Permission denied"
	BodyEnterprise    = "Nomad Enterprise only endpoint"
)

// Forbidden makes a path return Nomad's 403, which never names the capability
// that was missing.
func (s *Server) Forbidden(path string) { s.Status(path, http.StatusForbidden, BodyDenied) }

// NotFound makes a path return a 404 with the given body.
func (s *Server) NotFound(path, body string) { s.Status(path, http.StatusNotFound, body) }

// EnterpriseOnly makes a path return the 501 a Community Edition agent sends
// for an Enterprise endpoint.
func (s *Server) EnterpriseOnly(path string) {
	s.Status(path, http.StatusNotImplemented, BodyEnterprise)
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// Last returns the most recent request whose path contains substr.
//
// It fails the test if there is none, because every caller of this is about to
// assert on the result and a nil check at each site adds nothing.
func (s *Server) Last(substr string) Request {
	s.t.Helper()

	reqs := s.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if strings.Contains(reqs[i].Path, substr) {
			return reqs[i]
		}
	}
	s.t.Fatalf("no request to a path containing %q; got %s", substr, s.paths())
	return Request{}
}

// Called reports whether any request hit a path containing substr.
func (s *Server) Called(substr string) bool {
	for _, r := range s.Requests() {
		if strings.Contains(r.Path, substr) {
			return true
		}
	}
	return false
}

// Reset forgets every recorded request, leaving the routes alone.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

func (s *Server) paths() string {
	var b strings.Builder
	for _, r := range s.Requests() {
		b.WriteString("\n  " + r.Method + " " + r.Path)
	}
	if b.Len() == 0 {
		return " (no requests at all)"
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, body any, nextToken string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")

	// The Go client discards parseQueryMeta's error, so a missing header is
	// silently a zero value rather than a failure. They are all set anyway:
	// a test that means to exercise pagination should not be relying on that.
	h.Set("X-Nomad-Index", "42")
	h.Set("X-Nomad-LastContact", "0")
	h.Set("X-Nomad-KnownLeader", "true")
	if nextToken != "" {
		h.Set("X-Nomad-NextToken", nextToken)
	}

	_ = json.NewEncoder(w).Encode(body)
}

func notFound(w http.ResponseWriter, body string) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, body)
}
