// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

//go:build e2e

// Package e2e drives the real, built server binary against a real Nomad agent.
//
// The unit tests put a fake Nomad behind the tool handlers. That covers the
// projections and the error mapping, which is where most bugs live — but it
// cannot catch the ones that matter most on the day someone installs this: a
// field Nomad renamed, a flag the binary does not accept, an env var that never
// reaches the client, a jobspec that will not parse. Everything here is exactly
// the code path a user gets.
//
// Nothing is mocked. A throwaway `nomad agent -dev` is started on ports nobody
// else is using, the real jobspecs from examples/ are submitted through the MCP
// run_job tool, and every assertion is made by speaking JSON-RPC to the built
// binary over stdio.
//
// Run with: make test-e2e
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// agent is a throwaway Nomad running for the lifetime of the test binary.
type agent struct {
	addr string // http://127.0.0.1:PORT
	cmd  *exec.Cmd
	log  *os.File

	// exited closes when the agent process dies, so waitReady can fail in a
	// second rather than sitting out its whole timeout. An agent that cannot
	// start — a port clash, a broken fingerprint, a licence problem — is the
	// common case, and waiting 90 seconds to be told "no leader yet" hides the
	// real error in the log.
	exited  chan struct{}
	waitErr error
}

var (
	shared     *agent
	serverPath string
	skipReason string
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	if reason := unavailable(); reason != "" {
		// Skipping is reported per-test rather than here, so the reason is
		// visible in the test output instead of buried in setup logs.
		skipReason = reason
		return m.Run(), nil
	}

	dir, err := os.MkdirTemp("", "nomad-mcp-e2e")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)

	serverPath = filepath.Join(dir, "nomad-mcp-server")
	build := exec.Command("go", "build", "-o", serverPath, "./cmd/nomad-mcp-server")
	build.Dir = ".."
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return 0, fmt.Errorf("building the server binary: %w", err)
	}

	shared, err = startAgent(dir)
	if err != nil {
		return 0, err
	}
	defer shared.stop()

	if err := shared.waitReady(90 * time.Second); err != nil {
		shared.dumpLog()
		return 0, err
	}

	return m.Run(), nil
}

// unavailable reports why the e2e suite cannot run, or "" if it can.
func unavailable() string {
	path, err := exec.LookPath("nomad")
	if err != nil {
		return "the 'nomad' binary is not on PATH; install Nomad to run the e2e suite"
	}

	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("'nomad version' failed: %v", err)
	}

	// An Enterprise binary refuses `nomad agent -dev` outright with
	// "invalid license config: empty license", and the error does not obviously
	// point at the licence. Detecting it here turns a confusing failure into a
	// clear skip.
	if strings.Contains(string(out), "+ent") {
		return "the 'nomad' on PATH is an Enterprise build, which cannot run 'nomad agent -dev' " +
			"without a licence. Install Nomad Community Edition to run the e2e suite."
	}
	return ""
}

func startAgent(dir string) (*agent, error) {
	httpPort, rpcPort, serfPort := freePort(), freePort(), freePort()

	// The CPU override is the Apple Silicon fingerprinting workaround; see
	// scripts/dev-agent.hcl for why. It is harmless everywhere else.
	cfg := fmt.Sprintf(`
bind_addr = "127.0.0.1"
log_level = "WARN"

ports {
  http = %d
  rpc  = %d
  serf = %d
}

client {
  cpu_total_compute = 24000
}
`, httpPort, rpcPort, serfPort)

	cfgPath := filepath.Join(dir, "agent.hcl")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return nil, err
	}

	logFile, err := os.Create(filepath.Join(dir, "agent.log"))
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("nomad", "agent", "-dev", "-config", cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting nomad agent: %w", err)
	}

	a := &agent{
		addr:   fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		cmd:    cmd,
		log:    logFile,
		exited: make(chan struct{}),
	}
	go func() {
		a.waitErr = cmd.Wait()
		close(a.exited)
	}()

	return a, nil
}

// waitReady blocks until the agent has a leader and a client node that is ready.
//
// Both conditions matter. A leader alone means the server is up but nothing can
// be placed yet, and a test that submitted a job at that moment would see a
// placement failure that is a race rather than a finding.
func (a *agent) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string

	for time.Now().Before(deadline) {
		select {
		case <-a.exited:
			return fmt.Errorf("the nomad agent exited during startup (%v); its log follows", a.waitErr)
		default:
		}

		leader, err := a.get("/v1/status/leader")
		if err != nil || len(leader) < 3 {
			last = "no leader yet"
			time.Sleep(250 * time.Millisecond)
			continue
		}

		nodes, err := a.get("/v1/nodes")
		if err != nil {
			last = "nodes endpoint not answering"
			time.Sleep(250 * time.Millisecond)
			continue
		}

		var stubs []struct {
			Status                string
			SchedulingEligibility string
		}
		if err := json.Unmarshal(nodes, &stubs); err == nil {
			for _, n := range stubs {
				if n.Status == "ready" && n.SchedulingEligibility == "eligible" {
					return nil
				}
			}
		}
		last = "no ready client node yet"
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("nomad agent never became ready within %s (%s)", timeout, last)
}

func (a *agent) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, a.addr+path, nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	return buf[:n], nil
}

func (a *agent) stop() {
	if a == nil || a.cmd == nil || a.cmd.Process == nil {
		return
	}
	_ = a.cmd.Process.Kill()
	if a.exited != nil {
		<-a.exited
	}
	if a.log != nil {
		_ = a.log.Close()
	}
}

// dumpLog prints the agent's log when setup failed, because otherwise the only
// symptom is a timeout with no explanation.
func (a *agent) dumpLog() {
	if a == nil || a.log == nil {
		return
	}
	data, err := os.ReadFile(a.log.Name())
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "--- nomad agent log ---")
	fmt.Fprintln(os.Stderr, string(data))
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// --- the MCP client ---------------------------------------------------------

// mcpClient speaks JSON-RPC to the built binary over stdio.
//
// It is deliberately a real subprocess rather than an in-process server. The
// binary's flag parsing, environment handling and stdout discipline are all
// part of what this suite exists to check, and none of them are exercised by
// calling NewServer directly.
type mcpClient struct {
	t   *testing.T
	cmd *exec.Cmd
	in  *os.File
	out *bufio.Reader

	mu sync.Mutex
	id int
}

// newClient starts the server with the given extra environment.
func newClient(t *testing.T, env ...string) *mcpClient {
	t.Helper()
	skipUnlessReady(t)

	cmd := exec.Command(serverPath, "stdio")
	cmd.Env = append(os.Environ(),
		"NOMAD_ADDR="+shared.addr,
		"NOMAD_MCP_READ_ONLY=true",
	)
	cmd.Env = append(cmd.Env, env...)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd.Stdin = inR
	cmd.Stdout = outW
	// The server logs to stderr precisely so stdout stays a clean protocol
	// channel. Sending it to the test log makes a failure diagnosable.
	cmd.Stderr = &testWriter{t: t}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = inR.Close()
	_ = outW.Close()

	c := &mcpClient{
		t:   t,
		cmd: cmd,
		in:  inW,
		// Tool results carry whole job specifications and log bodies, so the
		// default scanner buffer is nowhere near enough.
		out: bufio.NewReaderSize(outR, 1<<20),
	}

	t.Cleanup(func() {
		_ = inW.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = outR.Close()
	})

	c.initialize()
	return c
}

func skipUnlessReady(t *testing.T) {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("server stderr: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *mcpClient) initialize() {
	c.t.Helper()

	c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e", "version": "0"},
	})
	c.notify("notifications/initialized")
}

func (c *mcpClient) send(msg any) {
	c.t.Helper()

	data, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("writing to the server: %v", err)
	}
}

func (c *mcpClient) notify(method string) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method})
}

// request sends one call and returns its result, failing the test on a
// protocol-level error.
func (c *mcpClient) request(method string, params any) json.RawMessage {
	c.t.Helper()

	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.send(msg)

	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("reading the response to %s: %v", method, err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		c.t.Fatalf("the server wrote something that is not JSON-RPC on stdout: %s", line)
	}
	if resp.Error != nil {
		c.t.Fatalf("%s failed: %s", method, resp.Error.Message)
	}
	return resp.Result
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (r toolResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// callTool invokes a tool and returns the raw result, error or not.
func (c *mcpClient) callTool(name string, args map[string]any) toolResult {
	c.t.Helper()

	if args == nil {
		args = map[string]any{}
	}
	raw := c.request("tools/call", map[string]any{"name": name, "arguments": args})

	var res toolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		c.t.Fatalf("decoding the result of %s: %v", name, err)
	}
	return res
}

// tool invokes a tool, requires success, and decodes its JSON payload.
func (c *mcpClient) tool(name string, args map[string]any) map[string]any {
	c.t.Helper()

	res := c.callTool(name, args)
	if res.IsError {
		c.t.Fatalf("%s failed: %s", name, res.text())
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.text()), &out); err != nil {
		c.t.Fatalf("%s did not return JSON: %s", name, res.text())
	}
	return out
}

// toolFails invokes a tool and requires it to have refused or failed.
func (c *mcpClient) toolFails(name string, args map[string]any) string {
	c.t.Helper()

	res := c.callTool(name, args)
	if !res.IsError {
		c.t.Fatalf("%s should have failed, but returned: %s", name, res.text())
	}
	return res.text()
}

// example reads one of the real jobspecs from examples/.
func example(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "examples", name))
	if err != nil {
		t.Fatalf("reading the example jobspec: %v", err)
	}
	return string(data)
}

// eventually retries until cond passes or the deadline expires.
//
// Nomad is eventually consistent about almost everything a test wants to
// assert: a job is registered before its evaluation exists, and an allocation
// is placed before its task is running. Polling is not flakiness-hiding here,
// it is the actual contract.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}
