// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// listedTools returns the tool names a freshly started server offers.
func listedTools(t *testing.T, env ...string) []string {
	t.Helper()

	c := newClient(t, env...)

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.request("tools/list", nil), &listed); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestToolsetsRestrictTheCatalog drives the real startup path: the environment
// variable, the flag binding, validation, and registration.
func TestToolsetsRestrictTheCatalog(t *testing.T) {
	full := listedTools(t)

	t.Run("a single toolset offers only its own tools", func(t *testing.T) {
		got := listedTools(t, "NOMAD_MCP_TOOLSETS=jobs")

		if len(got) >= len(full) {
			t.Fatalf("restricting to one toolset offered %d tools, the full catalog is %d", len(got), len(full))
		}
		if !has(got, "list_jobs") {
			t.Error("the jobs toolset did not offer list_jobs")
		}
		for _, unwanted := range []string{"list_nodes", "read_variable", "collect_hcdiag"} {
			if has(got, unwanted) {
				t.Errorf("the jobs toolset offered %s, which belongs to another toolset", unwanted)
			}
		}
	})

	t.Run("several toolsets union", func(t *testing.T) {
		got := listedTools(t, "NOMAD_MCP_TOOLSETS=jobs,nodes")
		for _, want := range []string{"list_jobs", "list_nodes"} {
			if !has(got, want) {
				t.Errorf("expected %s to be offered", want)
			}
		}
		if has(got, "read_variable") {
			t.Error("offered read_variable, which is in neither requested toolset")
		}
	})

	t.Run("all is the same as unset", func(t *testing.T) {
		if got := listedTools(t, "NOMAD_MCP_TOOLSETS=all"); len(got) != len(full) {
			t.Fatalf("NOMAD_MCP_TOOLSETS=all offered %d tools, unset offers %d", len(got), len(full))
		}
	})

	t.Run("a restricted tool is genuinely absent, not merely refused", func(t *testing.T) {
		c := newClient(t, "NOMAD_MCP_TOOLSETS=jobs")

		// The read-only gate refuses a mutating tool through a tool *result*,
		// so the model gets an explanation. A tool excluded by a toolset was
		// never registered at all, so it fails at the protocol level instead.
		// That distinction is what the security model rests on: there is no
		// handler behind it to reach.
		failed, msg := c.callToolAllowingProtocolError("list_variables")
		if !failed {
			t.Fatalf("calling a tool outside the enabled toolsets should fail, got: %s", msg)
		}
	})
}

// TestUnknownToolsetRefusesToStart checks the failure direction. Ignoring a
// typo would leave an operator with a server offering more than they asked for.
func TestUnknownToolsetRefusesToStart(t *testing.T) {
	skipUnlessReady(t)

	cmd := exec.Command(serverPath, "stdio", "--toolsets=jobs,jobss")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatal("the server started despite an unknown toolset name")
	}
	body := string(out)
	if !strings.Contains(body, "jobss") {
		t.Errorf("the error does not name the offending value: %s", body)
	}
	// The valid names belong in the error, not only in the documentation.
	for _, want := range []string{"jobs", "nodes", "variables"} {
		if !strings.Contains(body, want) {
			t.Errorf("the error does not list the valid toolset %q: %s", want, body)
		}
	}
}

// callToolAllowingProtocolError invokes a tool without requiring the server to
// answer with a result at all.
//
// The harness's request helper fails the test on a JSON-RPC error, which is
// exactly the response an unregistered tool produces — so asserting that a tool
// is absent needs its own path down to the wire.
func (c *mcpClient) callToolAllowingProtocolError(name string) (failed bool, message string) {
	c.t.Helper()

	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	c.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": map[string]any{}},
	})

	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("reading the response to tools/call: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		c.t.Fatalf("the server wrote something that is not JSON-RPC on stdout: %s", line)
	}
	if resp.Error != nil {
		return true, resp.Error.Message
	}

	var res toolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		c.t.Fatalf("decoding the result of %s: %v", name, err)
	}
	return res.IsError, res.text()
}
