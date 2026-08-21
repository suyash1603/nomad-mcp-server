// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The streamable-HTTP transport has a whole layer the stdio transport does not:
// CORS, per-request Nomad settings lifted out of headers, session identity, and
// a refusal to bind anywhere public without TLS. None of that is reachable from
// the stdio tests, and all of it is what someone deploying this on a shared box
// actually depends on.
//
// Run with: make test-http

type httpServer struct {
	t    *testing.T
	base string
	cmd  *exec.Cmd
}

// startHTTP launches the built binary on the streamable-HTTP transport.
func startHTTP(t *testing.T, env ...string) *httpServer {
	t.Helper()
	skipUnlessReady(t)

	port := freePort()

	cmd := exec.Command(serverPath, "streamable-http",
		"--transport-host", "127.0.0.1",
		"--transport-port", fmt.Sprint(port),
	)
	cmd.Env = append(os.Environ(),
		"NOMAD_ADDR="+shared.addr,
		"NOMAD_MCP_READ_ONLY=true",
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = &testWriter{t: t}
	cmd.Stderr = &testWriter{t: t}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	s := &httpServer{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd}
	s.waitHealthy()
	return s
}

func (s *httpServer) waitHealthy() {
	s.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("the HTTP server never became healthy")
}

// post sends one JSON-RPC message to /mcp and returns the status, headers and
// decoded body.
//
// The Accept header carries both types because the streamable-HTTP transport
// may answer either with a plain JSON body or with a single SSE event,
// depending on the request. Both are handled below.
func (s *httpServer) post(body any, headers map[string]string) (int, http.Header, json.RawMessage) {
	s.t.Helper()

	data, err := json.Marshal(body)
	if err != nil {
		s.t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, s.base+"/mcp", bytes.NewReader(data))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("posting to /mcp: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, decodeMCPBody(raw)
}

// decodeMCPBody unwraps an SSE frame if there is one, and otherwise returns the
// body untouched.
func decodeMCPBody(raw []byte) json.RawMessage {
	text := string(raw)
	if !strings.Contains(text, "data:") {
		return json.RawMessage(raw)
	}
	for _, line := range strings.Split(text, "\n") {
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			return json.RawMessage(strings.TrimSpace(payload))
		}
	}
	return json.RawMessage(raw)
}

// session completes the handshake and returns the session id the server issued.
func (s *httpServer) session() string {
	s.t.Helper()

	status, header, body := s.post(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e-http", "version": "0"},
		},
	}, nil)

	if status != http.StatusOK {
		s.t.Fatalf("initialize returned %d: %s", status, body)
	}

	id := header.Get("Mcp-Session-Id")
	if id == "" {
		s.t.Fatal("the server issued no Mcp-Session-Id")
	}

	s.post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]string{"Mcp-Session-Id": id})

	return id
}

func TestHTTPHealthEndpoint(t *testing.T) {
	s := startHTTP(t)

	resp, err := http.Get(s.base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health returned %d: %s", resp.StatusCode, body)
	}

	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("health should return JSON, got: %s", body)
	}

	// A health endpoint that reported the token would be a credential leak
	// through an unauthenticated path.
	if strings.Contains(string(body), "NOMAD_TOKEN") || strings.Contains(string(body), "SecretID") {
		t.Errorf("the health endpoint must not expose credentials: %s", body)
	}
	t.Logf("health: %s", body)
}

func TestHTTPHandshakeAndToolCall(t *testing.T) {
	s := startHTTP(t)
	id := s.session()

	status, _, body := s.post(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	}, map[string]string{"Mcp-Session-Id": id})

	if status != http.StatusOK {
		t.Fatalf("tools/list returned %d: %s", status, body)
	}

	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decoding tools/list: %v (body: %s)", err, body)
	}
	if len(listed.Result.Tools) < 50 {
		t.Fatalf("expected the full catalog over HTTP, got %d", len(listed.Result.Tools))
	}

	status, _, body = s.post(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "get_cluster_status", "arguments": map[string]any{}},
	}, map[string]string{"Mcp-Session-Id": id})

	if status != http.StatusOK {
		t.Fatalf("tools/call returned %d: %s", status, body)
	}
	if !strings.Contains(string(body), "leader") {
		t.Errorf("get_cluster_status over HTTP should report a leader; got %s", body)
	}
}

// TestHTTPRejectsCredentialsInTheQueryString is a leak test.
//
// A token in a URL lands in every access log, proxy log and browser history
// between the client and this server, and an address in a URL is an SSRF lever.
// Both are refused outright rather than ignored, so a client doing it finds out.
func TestHTTPRejectsCredentialsInTheQueryString(t *testing.T) {
	s := startHTTP(t)

	// Every spelling, because url.Values lookups are case-sensitive and a
	// literal blocklist would let through whichever one nobody thought of.
	for _, param := range []string{
		"token=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"TOKEN=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"nomad_token=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"NOMAD_TOKEN=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"nomadToken=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"X-Nomad-Token=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"secret_id=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"secretID=9f2b7c11-4d3e-4a55-b0c8-1e7d9a2f3b64",
		"nomad_addr=http://attacker.invalid:4646",
		"NOMAD_ADDR=http://attacker.invalid:4646",
		"nomadAddr=http://attacker.invalid:4646",
		"address=http://attacker.invalid:4646",
	} {
		req, err := http.NewRequest(http.MethodPost, s.base+"/mcp?"+param, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s should be refused with 400, got %d: %s", param, resp.StatusCode, body)
		}
	}
}

// TestHTTPPerRequestTokenIsAccepted covers the multi-tenant path: an HTTP
// caller supplies its own Nomad token per request rather than the server
// holding one for everybody.
func TestHTTPPerRequestTokenIsAccepted(t *testing.T) {
	s := startHTTP(t)

	status, header, body := s.post(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e-http", "version": "0"},
		},
	}, map[string]string{"X-Nomad-Token": "3f8a1c60-9d24-4b17-8e05-6c2a9f1d4b73"})

	if status != http.StatusOK {
		t.Fatalf("initialize with a token header returned %d: %s", status, body)
	}
	if header.Get("Mcp-Session-Id") == "" {
		t.Error("a request carrying X-Nomad-Token should still get a session")
	}

	// The dev agent has ACLs disabled, so the token is accepted and ignored.
	// What is being checked here is that supplying one is not itself an error.
}

// TestHTTPRefusesPublicBindWithoutTLS checks the server fails closed at startup
// rather than warning and binding anyway.
func TestHTTPRefusesPublicBindWithoutTLS(t *testing.T) {
	skipUnlessReady(t)

	cmd := exec.Command(serverPath, "streamable-http",
		"--transport-host", "0.0.0.0",
		"--transport-port", fmt.Sprint(freePort()),
	)
	cmd.Env = append(os.Environ(), "NOMAD_ADDR="+shared.addr)

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("binding 0.0.0.0 without TLS should have failed")
	}

	text := string(out)
	if !strings.Contains(text, "TLS is required") {
		t.Errorf("the error should say TLS is required; got:\n%s", text)
	}
	if !strings.Contains(text, "MCP_TLS_CERT_FILE") {
		t.Errorf("the error should name the setting that fixes it; got:\n%s", text)
	}
}

// TestHTTPRejectsAForeignOrigin covers the browser threat: a page on another
// site making requests to a server bound to localhost.
func TestHTTPRejectsAForeignOrigin(t *testing.T) {
	s := startHTTP(t)

	req, err := http.NewRequest(http.MethodPost, s.base+"/mcp", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://attacker.invalid")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("an unlisted Origin should be refused; got 200: %s", body)
	}
}

// TestHTTPReadOnlyGateStillApplies proves the gate is not a stdio-only feature.
func TestHTTPReadOnlyGateStillApplies(t *testing.T) {
	s := startHTTP(t)
	id := s.session()

	_, _, body := s.post(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "stop_job",
			"arguments": map[string]any{"job_id": "anything"},
		},
	}, map[string]string{"Mcp-Session-Id": id})

	if !strings.Contains(string(body), "read-only") {
		t.Errorf("a mutating call over HTTP must still be refused; got %s", body)
	}
}
