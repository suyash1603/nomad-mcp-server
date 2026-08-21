# nomad-mcp-server

A [Model Context Protocol](https://modelcontextprotocol.io) server for
[HashiCorp Nomad](https://developer.hashicorp.com/nomad). It gives an AI
assistant structured, safe access to a Nomad cluster: what is running, what is
broken, and why.

It is built as a sibling of HashiCorp's
[`vault-mcp-server`](https://github.com/hashicorp/vault-mcp-server) — same
transports, same environment variables, same shape of configuration — so anyone
who has deployed that one already knows how to deploy this.

```
"Why isn't the payments-api job placing?"

  → read_job          the job asks for node.class = gpu
  → read_job_summary  1 allocation queued, 0 running
  → list_job_evaluations → read_evaluation

  "Task group 'api' could not be placed: the constraint
   ${node.class} = gpu filtered all 12 nodes. No node in
   this cluster has that class."
```

---

## ⚠️ Beta, and what it can see

**This is beta software.** The tool names, output shapes and defaults may change
before 1.0.

More importantly, be clear about what you are connecting an AI model to. With a
sufficiently privileged token this server can read:

- **Job specifications** — including task environment variable *names*, command
  lines, image references and constraints.
- **Task logs** — the stdout and stderr of your running workloads, whatever
  happens to be in them.
- **Allocation files** — the contents of files inside a running allocation's
  directory.
- **Nomad Variables** — your cluster's secret store, if you explicitly enable it.

Three defaults exist to make that manageable, and all three are on unless you
turn them off:

| Default | What it does |
|---|---|
| `NOMAD_MCP_READ_ONLY=true` | Every tool that would change the cluster is refused. |
| `NOMAD_MCP_ALLOW_VARIABLE_READS=false` | `read_variable` will not return values. `list_variables` still returns paths. |
| `NOMAD_MCP_ALLOWED_NAMESPACES` unset | Set it to confine the server to specific namespaces. |

**The single most effective control is the token.** This server can only do what
the token you give it can do. Give it a read-only ACL policy scoped to the
namespaces you actually want visible, not a management token. See
[docs/SECURITY.md](docs/SECURITY.md) for the full threat model, including how
prompt injection through job metadata and task logs is handled.

---

## Install

### Docker

```bash
docker run -i --rm \
  -e NOMAD_ADDR="http://host.docker.internal:4646" \
  -e NOMAD_TOKEN \
  -e NOMAD_NAMESPACE \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

### Binary

Download from [Releases](https://github.com/suyash1603/nomad-mcp-server/releases)
and put it on your `PATH`. Builds are published for macOS and Linux, `amd64` and
`arm64`.

### From source

```bash
go install github.com/suyash1603/nomad-mcp-server/cmd/nomad-mcp-server@latest
```

or

```bash
git clone https://github.com/suyash1603/nomad-mcp-server.git
cd nomad-mcp-server
make build          # → ./bin/nomad-mcp-server
```

### Check it works

```bash
nomad-mcp-server --version
NOMAD_ADDR=http://127.0.0.1:4646 nomad-mcp-server stdio
```

The second command should print a startup line to **stderr** and then wait.
Waiting silently is correct: stdout is the JSON-RPC channel and nothing else may
be written to it.

---

## Configure your MCP client

### Claude Code

```bash
claude mcp add nomad \
  -e NOMAD_ADDR=http://127.0.0.1:4646 \
  -- /absolute/path/to/nomad-mcp-server stdio
```

Then `claude mcp list` to confirm it connected.

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS,
`%APPDATA%\Claude\claude_desktop_config.json` on Windows:

```json
{
  "mcpServers": {
    "nomad": {
      "command": "/absolute/path/to/nomad-mcp-server",
      "args": ["stdio"],
      "env": {
        "NOMAD_ADDR": "http://127.0.0.1:4646",
        "NOMAD_MCP_READ_ONLY": "true"
      }
    }
  }
}
```

### Cursor

`.cursor/mcp.json` in the project, or `~/.cursor/mcp.json` globally:

```json
{
  "mcpServers": {
    "nomad": {
      "command": "/absolute/path/to/nomad-mcp-server",
      "args": ["stdio"],
      "env": {
        "NOMAD_ADDR": "http://127.0.0.1:4646"
      }
    }
  }
}
```

### VS Code

`.vscode/mcp.json`:

```json
{
  "servers": {
    "nomad": {
      "type": "stdio",
      "command": "/absolute/path/to/nomad-mcp-server",
      "args": ["stdio"],
      "env": {
        "NOMAD_ADDR": "http://127.0.0.1:4646"
      }
    }
  }
}
```

### Docker instead of a binary

Any of the above work with Docker by replacing `command` and `args`:

```json
{
  "command": "docker",
  "args": ["run", "-i", "--rm",
           "-e", "NOMAD_ADDR", "-e", "NOMAD_TOKEN",
           "ghcr.io/suyash1603/nomad-mcp-server:latest", "stdio"]
}
```

`-i` is required — without it the container has no stdin and the protocol never
starts.

---

## Configuration

Every setting is available as an environment variable **and** a command-line
flag. The flag wins when both are given.

### Nomad connection

These are the standard Nomad variables, read by the same code the `nomad` CLI
uses, so an environment that already works with `nomad status` works here.

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `NOMAD_ADDR` | `--nomad-addr` | `http://127.0.0.1:4646` | Address of the Nomad HTTP API |
| `NOMAD_TOKEN` | *(none — see below)* | — | ACL token |
| `NOMAD_REGION` | `--nomad-region` | agent's own | Region to target |
| `NOMAD_NAMESPACE` | `--nomad-namespace` | `default` | Default namespace for namespaced tools |
| `NOMAD_CACERT` | `--nomad-ca-cert` | — | PEM CA certificate file |
| `NOMAD_CAPATH` | `--nomad-ca-path` | — | Directory of PEM CA certificates |
| `NOMAD_CLIENT_CERT` | `--nomad-client-cert` | — | Client certificate for mTLS |
| `NOMAD_CLIENT_KEY` | `--nomad-client-key` | — | Client key for mTLS |
| `NOMAD_TLS_SERVER_NAME` | `--nomad-tls-server-name` | — | SNI host name |
| `NOMAD_SKIP_VERIFY` | `--nomad-skip-verify` | `false` | Skip TLS verification (insecure) |

> **`NOMAD_TOKEN` has no flag, deliberately.** A token passed as a command-line
> argument is visible to every other process on the machine through `ps`, and
> lands in your shell history. The `nomad` CLI makes the same choice. There is a
> test asserting the flag stays absent.

### Safety

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `NOMAD_MCP_READ_ONLY` | `--read-only` | **`true`** | Refuse every mutating tool |
| `NOMAD_MCP_ALLOWED_NAMESPACES` | `--allowed-namespaces` | *(all)* | Comma-separated namespace allowlist |
| `NOMAD_MCP_ALLOW_VARIABLE_READS` | `--allow-variable-reads` | `false` | Let `read_variable` return values |
| `NOMAD_MCP_MAX_LOG_BYTES` | `--max-log-bytes` | `65536` | Cap on log and file reads |

### Transport and logging

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `TRANSPORT_MODE` | `--transport-mode` | `stdio` | `stdio` or `http` when no subcommand is given |
| `TRANSPORT_HOST` | `--transport-host` | `127.0.0.1` | HTTP bind host |
| `TRANSPORT_PORT` | `--transport-port` | `8080` | HTTP bind port |
| `MCP_ENDPOINT` | `--mcp-endpoint` | `/mcp` | Path the MCP endpoint is served on |
| `MCP_ALLOWED_ORIGINS` | `--mcp-allowed-origins` | — | CORS origin allowlist |
| `MCP_CORS_MODE` | `--mcp-cors-mode` | `strict` | `strict`, `development` or `disabled` |
| `MCP_TLS_CERT_FILE` | `--mcp-tls-cert-file` | — | TLS certificate for the HTTP transport |
| `MCP_TLS_KEY_FILE` | `--mcp-tls-key-file` | — | TLS key |
| `MCP_RATE_LIMIT_GLOBAL` | `--mcp-rate-limit-global` | `10:20` | Global `rps:burst` (HTTP only) |
| `MCP_RATE_LIMIT_SESSION` | `--mcp-rate-limit-session` | `5:10` | Per-session `rps:burst` (HTTP only) |
| `NOMAD_MCP_LOG_LEVEL` | `--log-level` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `NOMAD_MCP_LOG_FILE` | `--log-file` | stderr | Path to a log file |

The HTTP transport **refuses to start** on a non-loopback address without TLS.
An MCP server holding a Nomad token should not be reachable off-box in
plaintext, and failing at startup beats a warning nobody reads.

---

## Tools

54 tools: **37 read-only** and **17 mutating**. Mutating tools are listed even in
read-only mode — `tools/list` describes the server honestly, and a blocked call
returns an explanation rather than an "unknown tool" error that looks like a bug.

Legend: **R** read-only · **W** mutating · **W!** mutating and destructive
(can discard state or interrupt running work).

### Cluster and search

| | Tool | What it does |
|---|---|---|
| R | `get_cluster_status` | Leader, server peers and versions, node counts by state — the whole cluster in one call |
| R | `list_regions` | Regions this cluster knows about |
| R | `list_node_pools` | Named groups of client nodes that jobs can target |
| R | `get_agent_config` | Identity and role of the agent this server is connected to (an allowlist, not a raw dump) |
| R | `search` | Prefix search across jobs, allocations, nodes, deployments, evaluations and more |

### Jobs

| | Tool | What it does |
|---|---|---|
| R | `list_jobs` | Jobs in a namespace, with allocation counts rolled up |
| R | `read_job` | One job: task groups, drivers, images, resources, constraints |
| R | `read_job_summary` | Allocation counts per task group — the fastest way to see something is wrong |
| R | `list_job_allocations` | What is actually running for a job |
| R | `list_job_evaluations` | Scheduler decisions for a job — **where placement failures live** |
| R | `list_job_deployments` | Rollouts of a service job |
| R | `list_job_versions` | Version history with diffs between versions |
| R | `get_job_scale_status` | Desired and running counts, scaling policy bounds, recent events |
| R | `plan_job` | Dry-run a submission: what would be created, destroyed or replaced |
| R | `validate_job` | Check a jobspec parses and is legal, without submitting |
| R | `parse_job_hcl` | Convert HCL2 to Nomad's JSON job format |
| W! | `run_job` | Submit a job, creating or updating it |
| W! | `stop_job` | Stop a job, optionally purging it |
| W! | `scale_task_group` | Change how many allocations a task group runs |
| W! | `revert_job_version` | Roll a job back to an earlier version |
| W | `dispatch_parameterized_job` | Dispatch an instance of a parameterized job |
| W | `force_periodic_job` | Run a periodic job now |

> `plan_job` and `parse_job_hcl` change nothing, but Nomad requires
> `submit-job`/`plan-job` and `parse-job` respectively. A token with only
> `read-job` gets a 403 from them. Their descriptions say so.

### Allocations

| | Tool | What it does |
|---|---|---|
| R | `list_allocations` | Allocations across a namespace, newest first |
| R | `read_allocation` | One allocation: task states, exit codes, restarts, reschedule history |
| R | `read_allocation_logs` | **stdout/stderr of a task — the primary tool for finding out why something failed** |
| R | `list_allocation_files` | Files inside a running allocation's directory |
| R | `read_allocation_file` | Contents of one such file |
| R | `get_allocation_stats` | Live CPU and memory use per task |
| W! | `restart_allocation` | Restart tasks in place |
| W! | `stop_allocation` | Stop one allocation; Nomad reschedules it elsewhere |
| W! | `signal_allocation` | Send a Unix signal to a task |

### Nodes

| | Tool | What it does |
|---|---|---|
| R | `list_nodes` | Client nodes: status, pool, class, drain state, healthy drivers |
| R | `read_node` | One node in detail, with a diagnosis when it cannot take work |
| R | `list_node_allocations` | Everything running on one node |
| W | `set_node_eligibility` | Mark a node eligible or ineligible for new work |
| W! | `drain_node` | Drain a node, migrating its allocations away |

### Deployments and evaluations

| | Tool | What it does |
|---|---|---|
| R | `list_deployments` | Rollouts in a namespace |
| R | `read_deployment` | One rollout, its allocations, and why it is stuck |
| R | `list_evaluations` | Scheduler evaluations — filter `Status == "blocked"` for capacity problems |
| R | `read_evaluation` | **Placement failures explained in plain language** |
| W! | `promote_deployment` | Promote canaries so a rollout can continue |
| W! | `fail_deployment` | Mark a deployment failed, stopping the rollout |

### Namespaces, services and volumes

| | Tool | What it does |
|---|---|---|
| R | `list_namespaces` | Namespaces defined in the cluster |
| R | `read_namespace` | One namespace: quota, node pool restrictions, metadata |
| R | `list_services` | Services in Nomad's own service discovery |
| R | `read_service` | Instances of one service: address, port, tags, owning alloc |
| R | `list_volumes` | CSI volumes or dynamic host volumes (`type` selects which) |
| R | `read_volume` | One volume: plugin, capacity, schedulability, mounts |
| W | `create_namespace` | Create or update a namespace |
| W! | `delete_namespace` | Delete a namespace permanently |

### Variables

| | Tool | What it does |
|---|---|---|
| R | `list_variables` | Variable **paths** and timestamps — never values |
| R | `read_variable` | Variable contents. **Off unless `NOMAD_MCP_ALLOW_VARIABLE_READS=true`**; `keys_only=true` works either way |
| W! | `write_variable` | Create or **replace** a variable (Nomad's endpoint replaces; dropped keys are reported) |
| W! | `delete_variable` | Delete a variable and its contents |

### Not included: ACL tools

There are deliberately no tools for creating, reading or writing ACL tokens or
policies. Other Nomad MCP servers expose these; one of them can mint a
management token directly into the model's context. The safest handling of that
capability is not to build it.

---

## Resources and prompts

**Resources** let you attach a Nomad object to a conversation directly — the
paperclip menu in Claude Desktop, `@`-mentions in Claude Code:

| URI | Contents |
|---|---|
| `nomad://cluster` | Cluster health |
| `nomad://jobs` | Jobs in the default namespace |
| `nomad://jobs/{namespace}/{job_id}` | One job |
| `nomad://allocs/{alloc_id}` | One allocation |
| `nomad://nodes/{node_id}` | One client node |

Each returns exactly what the equivalent tool returns — there is a test
asserting the bytes are identical — so an attached job and a `read_job` call
never disagree.

**Prompts** are workflows you start deliberately:

- **`troubleshoot_failing_job`** — walks the job → summary → *evaluation or
  allocation* → logs chain in the order that actually finds the cause. Arguments:
  `job_id` (required), `namespace`, `symptom`.
- **`explain_cluster_health`** — quorum and version skew first, then nodes,
  blocked scheduling, stuck rollouts and failing jobs. Argument: `namespace`.

---

## HTTP transport

For a shared deployment where several clients connect to one server:

```bash
nomad-mcp-server streamable-http --transport-port 8080
```

- Endpoint: `POST /mcp` · Health: `GET /health`
- Each request may carry its own `X-Nomad-Token`, `X-Nomad-Namespace` and
  `X-Nomad-Region` headers, so one server can serve callers with different
  permissions.
- Credentials in **query strings are refused with a 400**, not ignored. A token
  in a URL ends up in every access log between the client and here, and an
  address in a URL would make this server an SSRF gadget.
- `Origin` is validated on every request. The default `strict` mode rejects all
  cross-origin requests; without this, any page you visit could drive a server
  running on your localhost.

---

## Documentation

| Document | For |
|---|---|
| [docs/TEAM-TESTING.md](docs/TEAM-TESTING.md) | **Start here** if someone sent you this repo. Under ten minutes. |
| [docs/TESTING.md](docs/TESTING.md) | The full copy-pasteable test script, every path |
| [docs/SECURITY.md](docs/SECURITY.md) | Threat model: token scope, prompt injection, what a compromised client gets |
| [docs/WALKTHROUGH.md](docs/WALKTHROUGH.md) | Narrated tour of the entire codebase |
| [docs/TESTING.md](docs/TESTING.md) | The full copy-pasteable test script |
| [docs/SECURITY.md](docs/SECURITY.md) | Threat model and safety defaults |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to add a tool |

---

## Development

```bash
make build        # compile to ./bin
make test         # unit tests; Nomad is faked, no cluster needed
make test-race    # the same under the race detector
make test-e2e     # against a real `nomad agent -dev`
make test-http    # the streamable-HTTP transport, end to end
make check        # fmt, vet, test — what CI runs
make inspector    # MCP Inspector against a fresh build
```

The e2e suite starts its own throwaway Nomad on unused ports and skips with an
explanation if `nomad` is missing — or if it is an **Enterprise** build, which
cannot run `nomad agent -dev` without a licence.

---

## License

[MPL-2.0](LICENSE) — the same license Nomad and `vault-mcp-server` use.

This is an independent project. It is not affiliated with or endorsed by
HashiCorp.
