// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools"
)

// fakeNomad serves just enough of the Nomad API for the resources to render.
//
// Anything not listed returns 404 with Nomad's own wording, which is also what
// several of these tools quietly rely on: they make best-effort extra calls
// (a deployment's allocations, a node's events) and must degrade rather than
// fail when those come back empty.
func fakeNomad(t *testing.T) *httptest.Server {
	t.Helper()

	routes := map[string]any{
		"/v1/status/leader": "10.0.0.1:4647",
		"/v1/agent/members": map[string]any{
			"ServerRegion": "global",
			"Members": []map[string]any{{
				"Name": "server-1.global", "Addr": "10.0.0.1", "Status": "alive",
				"Tags": map[string]string{"build": "1.9.0", "dc": "dc1", "region": "global"},
			}},
		},
		"/v1/nodes": []map[string]any{{
			"ID": "11111111-2222-3333-4444-555555555555", "Name": "node-1",
			"Status": "ready", "SchedulingEligibility": "eligible",
			"Datacenter": "dc1", "NodeClass": "", "Drain": false, "Version": "1.9.0",
		}},
		"/v1/node/11111111-2222-3333-4444-555555555555": map[string]any{
			"ID": "11111111-2222-3333-4444-555555555555", "Name": "node-1",
			"Status": "ready", "SchedulingEligibility": "eligible",
			"Datacenter": "dc1", "NodePool": "default", "Drain": false,
			"Attributes": map[string]string{"nomad.version": "1.9.0"},
		},
		"/v1/jobs": []map[string]any{{
			"ID": "web", "Name": "web", "Type": "service", "Status": "running",
			"Namespace": "default", "Priority": 50,
		}},
		"/v1/job/web": map[string]any{
			"ID": "web", "Name": "web", "Type": "service", "Status": "running",
			"Namespace": "default", "Priority": 50, "Datacenters": []string{"dc1"},
			"TaskGroups": []map[string]any{{
				"Name": "app", "Count": 2,
				"Tasks": []map[string]any{{"Name": "server", "Driver": "docker"}},
			}},
		},
		"/v1/allocation/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee": map[string]any{
			"ID": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "Name": "web.app[0]",
			"JobID": "web", "Namespace": "default", "TaskGroup": "app",
			"NodeID": "11111111-2222-3333-4444-555555555555", "NodeName": "node-1",
			"ClientStatus": "running", "DesiredStatus": "run",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, ok := routes[req.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "job not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nomad-Index", "1")
		w.Header().Set("X-Nomad-KnownLeader", "true")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testServer(t *testing.T, cfg *config.Config) (*server.MCPServer, *Registrar, *client.Provider) {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	p, err := client.New(cfg, logger)
	require.NoError(t, err)

	s := server.NewMCPServer("test", "test",
		server.WithResourceCapabilities(true, true))

	gate := client.NewGate(cfg.ReadOnly, logger)
	catalog := tools.InitTools(s, p, gate)

	r := New(p, catalog)
	r.Register(s)
	return s, r, p
}

func baseConfig(addr string) *config.Config {
	return &config.Config{
		NomadAddr:      addr,
		NomadNamespace: config.DefaultNomadNamespace,
		ReadOnly:       true,
		MaxLogBytes:    config.DefaultMaxLogBytes,
	}
}

// readResource drives a resources/read the way a client would, through the
// server's own dispatch, so the URI template matching is exercised too.
func readResource(t *testing.T, s *server.MCPServer, uri string) (text string, errMsg string) {
	t.Helper()

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/read",
		"params": map[string]any{"uri": uri},
	})
	require.NoError(t, err)

	resp := s.HandleMessage(context.Background(), msg)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))

	if decoded.Error != nil {
		return "", decoded.Error.Message
	}
	require.NotNil(t, decoded.Result)
	require.Len(t, decoded.Result.Contents, 1, "each resource returns exactly one document")
	require.Equal(t, uri, decoded.Result.Contents[0].URI)
	require.Equal(t, mimeJSON, decoded.Result.Contents[0].MIMEType)
	return decoded.Result.Contents[0].Text, ""
}

// TestTemplateVarUnwrapsURITemplateValues is a regression test.
//
// mcp-go copies yosida95/uritemplate's Value.V straight into Arguments, and
// that field is []string even for a single-valued variable. The first version
// of this package asserted .(string), which yielded "" for every match and made
// all three templated resources fail as "missing segment" while the URI
// matching itself was working perfectly.
func TestTemplateVarUnwrapsURITemplateValues(t *testing.T) {
	require.Equal(t, "hello-service",
		templateVar(map[string]any{"job_id": []string{"hello-service"}}, "job_id"),
		"a []string is what mcp-go actually stores")

	require.Equal(t, "hello-service",
		templateVar(map[string]any{"job_id": "hello-service"}, "job_id"),
		"a plain string must still work")

	require.Equal(t, "hello-service",
		templateVar(map[string]any{"job_id": []any{"hello-service"}}, "job_id"),
		"a JSON round trip produces []any")

	require.Equal(t, "", templateVar(map[string]any{}, "job_id"))
	require.Equal(t, "", templateVar(map[string]any{"job_id": []string{}}, "job_id"))
	require.Equal(t, "", templateVar(map[string]any{"job_id": []string{"  "}}, "job_id"))
	require.Equal(t, "", templateVar(map[string]any{"job_id": 42}, "job_id"))
}

func TestResourcesAreListedAndDescribed(t *testing.T) {
	s, _, _ := testServer(t, baseConfig("http://127.0.0.1:1"))

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/list",
	})
	require.NoError(t, err)

	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	require.NoError(t, err)

	var decoded struct {
		Result struct {
			Resources []struct {
				URI         string `json:"uri"`
				Name        string `json:"name"`
				Description string `json:"description"`
				MIMEType    string `json:"mimeType"`
			} `json:"resources"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))

	// Concrete resources must exist, not just templates. Several MCP clients
	// never call resources/templates/list, and to them a templates-only server
	// looks like it exposes nothing at all.
	require.NotEmpty(t, decoded.Result.Resources, "at least one concrete resource must be listed")

	for _, res := range decoded.Result.Resources {
		require.True(t, strings.HasPrefix(res.URI, "nomad://"),
			"%s should be under the nomad:// scheme", res.URI)
		require.NotEmpty(t, res.Name, "%s needs a name", res.URI)
		require.Equal(t, mimeJSON, res.MIMEType, "%s should declare JSON", res.URI)
		require.Greater(t, len(res.Description), 80,
			"%s needs a description a model can act on, got %q", res.URI, res.Description)
	}
}

func TestResourceTemplatesAreListedAndDescribed(t *testing.T) {
	s, _, _ := testServer(t, baseConfig("http://127.0.0.1:1"))

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/templates/list",
	})
	require.NoError(t, err)

	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	require.NoError(t, err)

	var decoded struct {
		Result struct {
			ResourceTemplates []struct {
				URITemplate string `json:"uriTemplate"`
				Name        string `json:"name"`
				Description string `json:"description"`
				MIMEType    string `json:"mimeType"`
			} `json:"resourceTemplates"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))

	got := map[string]string{}
	for _, tpl := range decoded.Result.ResourceTemplates {
		got[tpl.URITemplate] = tpl.Description
		require.NotEmpty(t, tpl.Name, "%s needs a name", tpl.URITemplate)
		require.Equal(t, mimeJSON, tpl.MIMEType)
		require.Contains(t, tpl.Description, "Example:",
			"%s should show a concrete URI; a template with no example is guesswork", tpl.URITemplate)
	}

	// These three are named in the requirements, so they are asserted by URI
	// rather than by count.
	for _, want := range []string{
		"nomad://jobs/{namespace}/{job_id}",
		"nomad://allocs/{alloc_id}",
		"nomad://nodes/{node_id}",
	} {
		require.Contains(t, got, want)
	}
}

func TestReadingEveryResourceReturnsJSON(t *testing.T) {
	nomad := fakeNomad(t)
	s, _, _ := testServer(t, baseConfig(nomad.URL))

	cases := []struct {
		uri  string
		want string
	}{
		{"nomad://cluster", "10.0.0.1:4647"},
		{"nomad://jobs", "web"},
		{"nomad://jobs/default/web", "web"},
		{"nomad://allocs/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "aaaaaaaa"},
		{"nomad://nodes/11111111-2222-3333-4444-555555555555", "node-1"},
	}

	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			text, errMsg := readResource(t, s, tc.uri)
			require.Empty(t, errMsg, "reading %s should succeed", tc.uri)

			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &parsed),
				"resource contents must be valid JSON, got %q", text)
			require.Contains(t, text, tc.want)
		})
	}
}

// TestResourceAndToolAgreeExactly is the reason this package delegates instead
// of projecting Nomad a second time.
func TestResourceAndToolAgreeExactly(t *testing.T) {
	nomad := fakeNomad(t)
	s, _, p := testServer(t, baseConfig(nomad.URL))

	viaResource, errMsg := readResource(t, s, "nomad://jobs/default/web")
	require.Empty(t, errMsg)

	var readJob server.ServerTool
	for _, tool := range tools.Catalog(p) {
		if tool.Tool.Name == "read_job" {
			readJob = tool
		}
	}
	require.NotNil(t, readJob.Handler, "read_job must be in the catalog")

	var req mcp.CallToolRequest
	req.Params.Name = "read_job"
	req.Params.Arguments = map[string]any{"job_id": "web", "namespace": "default"}

	res, err := readJob.Handler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.Equal(t, resultText(res), viaResource,
		"an attached resource and the equivalent tool call must return identical bytes")
}

// TestResourceErrorsAreMappedNotRaw checks that a failed read still explains
// itself. A resource read has no retry loop, so a bare "404" would leave the
// user with nothing to act on.
func TestResourceErrorsAreMappedNotRaw(t *testing.T) {
	nomad := fakeNomad(t)
	s, _, _ := testServer(t, baseConfig(nomad.URL))

	text, errMsg := readResource(t, s, "nomad://jobs/default/does-not-exist")
	require.Empty(t, text)
	require.NotEmpty(t, errMsg)

	require.Contains(t, errMsg, "does-not-exist")
	require.Contains(t, errMsg, "list_jobs",
		"a not-found error should point at the tool that lists what does exist")
	require.NotContains(t, errMsg, "Unexpected response code",
		"the raw Go client error must not reach the user")
}

// TestNamespaceAllowlistCoversResources proves resources inherit the gate
// rather than routing around it. They delegate to the tools, so they get the
// namespace check for free — this asserts that stays true.
func TestNamespaceAllowlistCoversResources(t *testing.T) {
	nomad := fakeNomad(t)
	cfg := baseConfig(nomad.URL)
	cfg.AllowedNamespaces = []string{"production"}

	s, _, _ := testServer(t, cfg)

	_, errMsg := readResource(t, s, "nomad://jobs/default/web")
	require.NotEmpty(t, errMsg, "a namespace outside the allowlist must be refused")
	require.Contains(t, strings.ToLower(errMsg), "namespace")
}

// TestNoResourceCanChangeTheCluster is the safety invariant for this package.
//
// Resources have no destructive-hint annotation and no client-side
// confirmation: attaching one is a click. So every resource must be backed by a
// tool the gate classifies as read-only, and the ledger it checks is built by
// Register itself rather than kept by hand.
func TestNoResourceCanChangeTheCluster(t *testing.T) {
	logger := log.New()
	logger.SetOutput(io.Discard)

	cfg := baseConfig("http://127.0.0.1:1")
	p, err := client.New(cfg, logger)
	require.NoError(t, err)

	s := server.NewMCPServer("test", "test", server.WithResourceCapabilities(true, true))
	gate := client.NewGate(true, logger)
	catalog := tools.InitTools(s, p, gate)

	r := New(p, catalog)
	r.Register(s)

	delegated := r.Delegated()
	require.NotEmpty(t, delegated, "Register must record what it wired")

	for _, name := range delegated {
		require.False(t, gate.IsMutating(name),
			"resource is backed by %q, which the gate treats as mutating", name)
	}
}

// TestResourcesReferenceRealTools catches a resource pointing at a tool that
// has been renamed or removed, which would otherwise only show up when a user
// tried to attach it.
func TestResourcesReferenceRealTools(t *testing.T) {
	s, r, p := testServer(t, baseConfig("http://127.0.0.1:1"))
	_ = s

	registered := map[string]bool{}
	for _, tool := range tools.Catalog(p) {
		registered[tool.Tool.Name] = true
	}

	for _, name := range r.Delegated() {
		require.True(t, registered[name],
			"a resource delegates to %q, which is not in the tool catalog", name)
	}
}
