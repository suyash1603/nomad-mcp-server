# Testing

Every command in order, what you should see, and what it means if you see
something else. Copy-paste it top to bottom; nothing here depends on anything
outside this repository except Go, Nomad, Docker and Node.

If you only have ten minutes, read [TEAM-TESTING.md](TEAM-TESTING.md) instead.

---

## 0. Before anything else: the Nomad binary

This is the step that most often goes wrong, and its failure does not look like
what it is.

```bash
nomad version
```

**Expect** a Community Edition version, e.g.

```
Nomad v2.0.4
```

**If you see `+ent`** — for example `Nomad v1.9.7+ent` — stop. An Enterprise
binary refuses `nomad agent -dev` outright:

```
invalid license config: empty license
```

That error does not mention the licence file it wants, does not mention
`-dev`, and reads like a bug in the agent. It is not: Enterprise needs a licence
even to run a throwaway dev agent. Install Community Edition:

```bash
brew uninstall nomad          # if it came from the hashicorp/tap ent formula
brew install nomad
which nomad                   # confirm which one is on PATH now
```

The e2e suite detects this and skips with an explanation rather than failing
mysteriously, but the manual steps below will not work until it is fixed.

```bash
go version                    # 1.26 or newer
docker version                # only needed for §9
```

---

## 1. Build

```bash
git clone https://github.com/suyash1603/nomad-mcp-server.git
cd nomad-mcp-server
make build
```

**Expect** no output and a binary at `./bin/nomad-mcp-server`.

```bash
./bin/nomad-mcp-server --version
```

**Expect** four lines — name, version, commit, build date. The commit should be
a real SHA; if it is blank you built with plain `go build` rather than
`make build`, which skips the ldflags. Harmless, but the version output will be
less useful.

```bash
./bin/nomad-mcp-server --help
```

**Expect** a usage block listing `stdio` and `streamable-http`, then every flag
with its environment variable in brackets. Every flag has one except
`--nomad-token`, which does not exist — see [SECURITY.md](SECURITY.md).

---

## 2. Unit tests

No cluster needed. Nomad is faked with `httptest`.

```bash
make test
```

**Expect** `ok` for six packages and `no test files` for the rest. Takes about
fifteen seconds.

```bash
make test-race
```

**Expect** the same, slower. Any `DATA RACE` output is a real failure — the
server holds per-session clients in a `sync.Map` and both transports touch them
concurrently.

```bash
make test-cover
```

**Expect** coverage figures per package. The `pkg/tools/*` subpackages report
`0.0%`, which is an artefact — they have no test files of their own and are
exercised from `pkg/tools`. For the real number:

```bash
go test -coverpkg=./pkg/... -coverprofile=/tmp/cover.out ./... >/dev/null
go tool cover -func=/tmp/cover.out | tail -1
```

**Expect** roughly `52%`.

---

## 3. End-to-end tests

These start their own Nomad on unused ports, build the binary, and drive it as a
subprocess. Nothing to set up.

```bash
make test-e2e
```

**Expect** about fifteen seconds and every test passing, including the whole
troubleshooting chain against the real example jobspecs. Watch for this line in
the output — it is the thing the server exists to produce:

```
explanation: This evaluation could not place all of its allocations.
task group "impossible": no nodes were eligible for evaluation at all...
```

**If every test SKIPs**, `nomad` is missing or Enterprise. Go back to §0.

```bash
make test-http
```

**Expect** seven passing tests covering the HTTP transport: health, handshake,
tool call, credential rejection, TLS enforcement, CORS, and the read-only gate.

---

## 4. A dev agent you can poke at

The remaining sections need a Nomad you control. **Leave this running in its own
terminal.**

```bash
nomad agent -dev -bind 127.0.0.1 -config scripts/dev-agent.hcl
```

**Expect** log lines ending with something like
`client: node registration complete`.

> `scripts/dev-agent.hcl` sets `client.cpu_total_compute`. On Apple Silicon
> Nomad mis-fingerprints CPU frequency — an M4 Pro reports `4` where it means
> `4000` — leaving the node with about 40 MHz of allocatable compute. Every
> example job then fails with `Dimension "cpu" exhausted`, which looks exactly
> like a broken jobspec. Without this config file the rest of this document will
> not behave as described.

In a second terminal:

```bash
export NOMAD_ADDR=http://127.0.0.1:4646
nomad node status
```

**Expect** one node, `ready`, `eligible`.

Load the three example jobs:

```bash
nomad job run examples/hello-service.nomad.hcl
nomad job run examples/unplaceable.nomad.hcl
nomad job run examples/batch-report.nomad.hcl
nomad job status
```

**Expect** `hello-service` running, `unplaceable` pending forever (by design —
it asks for a node class nothing has), and `batch-report` with one group that
completes and one that fails (also by design). Those three cover the healthy
path, the placement-failure path and the runtime-failure path.

---

## 5. MCP Inspector

```bash
make inspector
```

This runs `npx @modelcontextprotocol/inspector ./bin/nomad-mcp-server` and opens
a browser.

**In the Inspector:**

1. Set the transport to **STDIO**, command to `./bin/nomad-mcp-server`,
   arguments to `stdio`, and add `NOMAD_ADDR=http://127.0.0.1:4646` to the
   environment. Click **Connect**.

   **Expect** a green connected indicator.

2. **Tools** tab → **List Tools**.

   **Expect 81 tools** against Nomad Enterprise, or 69 against Community Edition —
   the twelve Enterprise-only tools are not registered where they cannot work. Read a few descriptions. Each should say what the tool
   is for and when to reach for it, not merely restate its name — that is what a
   model actually reads when deciding what to call.

3. Call **`get_cluster_status`** with no arguments.

   **Expect** JSON naming a leader at `127.0.0.1:4647`, one server, one ready
   node.

4. Call **`read_job_summary`** with `job_id` = `unplaceable`.

   **Expect** `"healthy": false`, one queued allocation, and a `note` telling
   you to run `list_job_evaluations`. That pointer is the whole design: zero
   allocations means the answer is in the evaluations, not the logs.

5. Call **`list_job_evaluations`** with `job_id` = `unplaceable`, take the `id`
   from the result, then call **`read_evaluation`** with it.

   **Expect** an `explanation` field in plain English naming the task group and
   the constraint. This is the payoff — Nomad reports this as a map of counters,
   and turning it into a sentence is most of what this server does.

6. Call **`read_allocation_logs`** with the `alloc_id` of a failed
   `batch-report` allocation, `task` = `process`, `log_type` = `stderr`.
   (The failing group is `flaky`, whose task is `process`. The `report` group's
   `generate` task succeeds — use it to see the healthy case.)

   **Expect** the log text, plus a note labelling it as untrusted output.

7. **Resources** tab → **List Resources**.

   **Expect** `nomad://cluster` and `nomad://jobs`, and under templates the three
   `nomad://jobs/{namespace}/{job_id}`-style URIs. Read
   `nomad://jobs/default/hello-service` and confirm it matches what `read_job`
   returned.

8. **Prompts** tab.

   **Expect** `troubleshoot_failing_job` and `explain_cluster_health`. Get the
   first with `job_id` = `unplaceable` and read it — it should name the tools in
   the order to call them and say the server is in read-only mode.

---

## 6. Proving the safety gate

This is the test worth doing by hand, because it is the one thing you are
trusting.

### Refused by default

```bash
NOMAD_ADDR=http://127.0.0.1:4646 ./bin/nomad-mcp-server stdio <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stop_job","arguments":{"job_id":"hello-service"}}}
EOF
```

**Expect** the third response to contain `"isError":true` and a message saying
the server is in read-only mode, naming both `NOMAD_MCP_READ_ONLY` and
`--read-only`, and telling the model **not to retry**.

Confirm nothing happened:

```bash
nomad job status hello-service | head -5
```

**Expect** still `running`.

### Allowed when you turn it off

```bash
NOMAD_ADDR=http://127.0.0.1:4646 NOMAD_MCP_READ_ONLY=false \
  ./bin/nomad-mcp-server stdio <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stop_job","arguments":{"job_id":"hello-service"}}}
EOF
```

**Expect** a success result with an `eval_id`, and a startup warning on stderr:
`read-only mode is OFF: mutating tools are enabled and can change this cluster`.

```bash
nomad job status hello-service | head -5
nomad job run examples/hello-service.nomad.hcl    # put it back
```

### The variable gate is separate

```bash
NOMAD_ADDR=http://127.0.0.1:4646 NOMAD_MCP_READ_ONLY=false \
  ./bin/nomad-mcp-server stdio <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_variable","arguments":{"path":"anything"}}}
EOF
```

**Expect** a refusal even though writes are enabled. Read-only protects the
cluster from changes; this protects secrets from disclosure. They are different
concerns and are controlled separately.

### The namespace allowlist

```bash
NOMAD_ADDR=http://127.0.0.1:4646 NOMAD_MCP_ALLOWED_NAMESPACES=production \
  ./bin/nomad-mcp-server stdio <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_jobs","arguments":{"namespace":"default"}}}
EOF
```

**Expect** a refusal naming `production`. The request is never sent to Nomad.

---

## 7. Claude Code

```bash
claude mcp add nomad \
  -e NOMAD_ADDR=http://127.0.0.1:4646 \
  -- "$PWD/bin/nomad-mcp-server" stdio

claude mcp list
```

**Expect** `nomad` listed and connected. Then start `claude` and try these, in
this order — they walk the same path the troubleshooting prompt does:

| Ask | What should happen |
|---|---|
| *"What's running on my Nomad cluster?"* | Calls `get_cluster_status` and `list_jobs`; names all three example jobs and flags `unplaceable` |
| *"Why is the unplaceable job not placing?"* | Goes to the **evaluations**, not the logs, and reports the constraint by name |
| *"Show me the last 50 lines of stderr for the failing batch-report alloc"* | Finds the failed allocation, calls `read_allocation_logs`, quotes the error |
| *"Is any node in my cluster unable to take work?"* | Calls `list_nodes`; correctly reports that the one node is fine |
| *"Stop the hello-service job"* | **Refuses**, explains read-only mode, and tells you how to enable writes |

If the second one goes looking for logs instead of evaluations, that is worth
reporting — the whole design assumes it will not.

Try the prompt directly too: `/nomad:troubleshoot_failing_job` with
`job_id=unplaceable`, or attach `@nomad://jobs/default/hello-service` as a
resource.

Remove it again when you are done:

```bash
claude mcp remove nomad
```

---

## 8. HTTP transport

```bash
make run-http
```

**Expect** `starting StreamableHTTP server` on `127.0.0.1:8080`. In another
terminal:

```bash
curl -s http://127.0.0.1:8080/health | jq
```

**Expect** `{"status":"ok", ...}` with the version, transport and `read_only`
flag. **No token, ever** — this endpoint is unauthenticated.

A full handshake:

```bash
curl -s -D /tmp/h -X POST http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'

SESSION=$(grep -i mcp-session-id /tmp/h | tr -d '\r' | awk '{print $2}')
echo "session: $SESSION"

curl -s -X POST http://127.0.0.1:8080/mcp \
  -H "Mcp-Session-Id: $SESSION" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_cluster_status","arguments":{}}}'
```

**Expect** the cluster status. Now the things that should *fail*:

```bash
# A token in the URL — refused with 400, not ignored
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  'http://127.0.0.1:8080/mcp?nomad_token=abc123def456' -d '{}'

# A foreign Origin — refused
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8080/mcp \
  -H 'Origin: https://attacker.invalid' -d '{}'
```

**Expect** `400` and `403`. If either returns `200`, that is a security bug —
please report it.

And the startup refusal:

```bash
./bin/nomad-mcp-server streamable-http --transport-host 0.0.0.0
```

**Expect** it to exit immediately with `TLS is required when binding to a
non-localhost address`. It fails closed rather than warning and binding anyway.

Stop the server with Ctrl-C.

---

## 9. Docker

```bash
make docker-build
```

**Expect** a successful multi-stage build tagged `docker.io/nomad-mcp-server:dev`.

```bash
docker run -i --rm \
  -e NOMAD_ADDR="http://host.docker.internal:4646" \
  -e NOMAD_MCP_READ_ONLY=true \
  docker.io/nomad-mcp-server:dev stdio <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"docker","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_cluster_status","arguments":{}}}
EOF
```

**Expect** the cluster status.

**If you get `connection refused`**, the container cannot see your host's Nomad.
`host.docker.internal` works on Docker Desktop for Mac and Windows. On Linux,
add `--add-host=host.docker.internal:host-gateway`, or use `--network host` and
`NOMAD_ADDR=http://127.0.0.1:4646`.

**`-i` is not optional.** Without it the container has no stdin, the server sees
EOF immediately, and it exits before you can talk to it.

The HTTP transport in Docker:

```bash
make docker-run-http
curl -s http://127.0.0.1:8080/health | jq .status
```

**Expect** `"ok"`. Note the Makefile target binds `0.0.0.0` *inside* the
container, which is correct there — the container's network namespace is the
boundary, and the published port is still `127.0.0.1:8080` on your host.

---

## 10. Teardown

```bash
# stop the example jobs
nomad job stop -purge hello-service
nomad job stop -purge unplaceable
nomad job stop -purge batch-report

# stop the dev agent: Ctrl-C in its terminal
# it keeps no state outside its temp directory

# remove the MCP client entry if you added one
claude mcp remove nomad

# clean the build
make clean
docker rmi docker.io/nomad-mcp-server:dev
```

The dev agent writes only to a temporary directory it manages itself; there is
nothing left behind to clean up.

---

## When something goes wrong

| Symptom | Likely cause |
|---|---|
| `invalid license config: empty license` | Enterprise `nomad` on PATH. See §0. |
| `Dimension "cpu" exhausted` on the examples | Started the agent without `-config scripts/dev-agent.hcl`. See §4. |
| Every e2e test SKIPs | `nomad` missing or Enterprise. The skip message says which. |
| `connection refused` from a tool | `NOMAD_ADDR` wrong, or the agent is not running. The error message says both. |
| `Permission denied` from every tool | Your `NOMAD_TOKEN` lacks the capability. Nomad never says which one, so the tool names the capability it needed. |
| `requires Nomad Enterprise` | Genuinely an Enterprise-only endpoint (quotas, Sentinel, recommendations). Not an error. |
| The MCP client shows no tools | Something wrote to stdout. Check stderr; the server logs there precisely so stdout stays a clean protocol channel. |
| Rate limit refusals | HTTP transport only. Raise `MCP_RATE_LIMIT_SESSION`. Stdio is not rate limited. |
