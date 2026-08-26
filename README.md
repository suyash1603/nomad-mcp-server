# nomad-mcp-server

A [Model Context Protocol](https://modelcontextprotocol.io) server for
[HashiCorp Nomad](https://developer.hashicorp.com/nomad). It gives an AI
assistant structured, safe access to a Nomad cluster: what is running, what is
broken, why, and — when you let it — how to fix it.

88 tools across jobs, allocations, nodes, node pools, deployments, namespaces,
volumes and variables, plus hcdiag support-bundle collection. Works against Nomad Community Edition and Enterprise, and
against a cluster running locally, on EC2, in Docker or anywhere else its HTTP
API is reachable.

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

## ⚠️ What it can see

Be clear about what you are connecting an AI model to. With a
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

A fourth control is available but not on by default:
`NOMAD_MCP_ALLOW_DESTRUCTIVE=false` permits writes while still refusing anything
that discards state or interrupts running work.

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

### Connecting to a cluster that is not on this machine

The default address assumes Nomad is local. For a cluster on EC2, in Docker, in
Kubernetes, or behind TLS, see **[docs/CONNECTING.md](docs/CONNECTING.md)** —
and when something does not work, ask the model to run `check_connection`, which
reports exactly which part of the chain is broken and what to do about it.

The one that catches everyone: **inside a container, `localhost` is the
container.** Use `host.docker.internal` on macOS and Windows, or
`--network host` on Linux.

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
| `NOMAD_MCP_ALLOW_DESTRUCTIVE` | `--allow-destructive` | `true` | Allow tools that discard state or interrupt running work |
| `NOMAD_MCP_MAX_LOG_BYTES` | `--max-log-bytes` | `65536` | Cap on log and file reads |
| `NOMAD_MCP_ENTERPRISE` | `--enterprise` | `auto` | Offer the Enterprise-only tools: `auto`, `true` or `false` |
| `NOMAD_MCP_TOOLSETS` | `--toolsets` | `all` | Which groups of tools to offer at all — see [Toolsets](#toolsets) |

### Support bundles

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `NOMAD_MCP_ENABLE_HCDIAG` | `--enable-hcdiag` | **`false`** | Allow `collect_hcdiag` to run the local `hcdiag` binary |
| `NOMAD_MCP_HCDIAG_PATH` | `--hcdiag-path` | `hcdiag` | Path to the binary, or a name to find on `PATH` |
| `NOMAD_MCP_HCDIAG_DEST` | `--hcdiag-dest` | temp dir | Directory bundles must be written under |
| `NOMAD_MCP_HCDIAG_TIMEOUT` | `--hcdiag-timeout` | `10m` | Maximum time one collection may run |

The binary is named by configuration and never by a tool argument — a model that
could choose the executable could run anything the server can. Setting
`NOMAD_MCP_HCDIAG_DEST` confines where bundles may be written; a `destination`
outside it is refused.

`NOMAD_MCP_ALLOW_DESTRUCTIVE=false` is a middle tier: writes work, but anything
that can discard state or interrupt running work is refused. `scale_task_group`
runs; `purge_node`, `delete_namespace` and `drain_node` do not. It defaults to
`true` so that turning writes on still means turning writes on — read-only is
already the default, and someone who deliberately disabled it is asking for
writes.

Both tiers classify from the annotations the tools already carry rather than
from a separate list, and both fail closed: a tool that forgot its annotation is
blocked, not quietly permitted.

### Toolsets

`NOMAD_MCP_TOOLSETS` decides which groups of tools the server offers. The
default, `all`, is the whole catalog.

```bash
NOMAD_MCP_TOOLSETS=jobs,allocs,deployments nomad-mcp-server stdio
```

| Toolset | Covers |
|---|---|
| `system` | Cluster status, regions, node pools, agent and scheduler config, Raft, Autopilot, `search` |
| `jobs` | Job specifications, versions, planning and submission |
| `allocs` | Allocations, task logs, allocation files, resource statistics |
| `nodes` | Client nodes, draining, eligibility |
| `deployments` | Deployments and scheduler evaluations |
| `catalog` | Namespaces, service registrations, storage volumes |
| `variables` | Nomad Variables |
| `diag` | `collect_hcdiag` |
| `enterprise` | Licence, quotas, Sentinel, Dynamic Application Sizing |

Two reasons to narrow it. Every tool definition is sent to the model on **every
request**, so a catalog this size is a standing context cost — a client that
only ever asks about jobs and allocations pays for eighty-eight tools to answer
questions about twenty-seven. And a toolset that is never registered is a
scoping control in its own right: a server without the `variables` toolset
cannot read a Nomad Variable whatever its token permits.

This is a separate axis from read-only mode. Toolsets decide what is *offered*;
`NOMAD_MCP_READ_ONLY` decides whether the mutating half of what is offered may
actually run. `NOMAD_MCP_TOOLSETS=jobs` still cannot stop a job unless writes
are also enabled.

An unknown name is refused at startup, with the valid names in the error
message. A server that came up quietly missing a third of its tools would look,
from the model's side, exactly like one that never had them.

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

88 tools: **51 read-only** and **37 mutating**. Twelve of them are Enterprise-only
and are not registered at all against a cluster identified as Community Edition —
see [docs/ENTERPRISE.md](docs/ENTERPRISE.md).

Mutating tools are listed even in read-only mode — `tools/list` describes the
server honestly, and a blocked call returns an explanation rather than an
"unknown tool" error that looks like a bug.

Legend: **R** read-only · **W** mutating · **W!** mutating and destructive
(can discard state or interrupt running work) · **E** requires Nomad Enterprise.

### Cluster, connection and search

| | Tool | What it does |
|---|---|---|
| R | `check_connection` | **Start here when anything fails.** Address, TLS, token, ACL state, edition and permission probes — each failure with a concrete fix |
| R | `get_cluster_status` | Leader, server peers and versions, edition, node counts by state — the whole cluster in one call |
| R | `get_scheduler_config` | Placement algorithm, preemption, and the two switches that silently stop a cluster scheduling |
| R | `get_autopilot_health` | Autopilot's verdict on the server fleet: quorum failure tolerance, which servers are voters, how far each trails the leader |
| R | `get_autopilot_config` | Dead-server cleanup, health thresholds, voter promotion delay and minimum quorum |
| R | `get_raft_config` | The Raft peer set and the quorum arithmetic, flagging orphaned entries that count toward quorum but no longer exist |
| R | `list_regions` | Regions this cluster knows about |
| R | `get_agent_config` | Identity and role of the agent this server is connected to (an allowlist, not a raw dump) |
| R | `search` | Prefix search across jobs, allocations, nodes, deployments, evaluations and more |
| W! | `set_scheduler_config` | Change scheduler configuration cluster-wide |
| W! | `set_autopilot_config` | Change Autopilot configuration — including whether it may remove servers from the Raft peer set |
| W! | `remove_raft_peer` | Remove a dead server from the Raft peer set, lowering the quorum requirement |
| W! | `transfer_leadership` | Hand Raft leadership to another server before taking the leader out of service |

### Support bundles

| | Tool | What it does |
|---|---|---|
| R | `collect_hcdiag` | Run [hcdiag](https://github.com/hashicorp/hcdiag) and produce a Nomad support bundle |

`collect_hcdiag` is the only tool that runs a program on the host rather than
calling Nomad's API, so it has its own switch and is **off by default**:

```bash
NOMAD_MCP_ENABLE_HCDIAG=true
```

It also needs `hcdiag` installed on the machine running this MCP server — not on
the Nomad servers — because that is where the process executes it.

It returns the bundle's **path and a summary of what was collected, never the
contents**. hcdiag gathers agent configuration, environment variables and logs,
which on a real cluster means credentials; the tool tells the model not to read
the bundle and to hand the path to you instead. hcdiag applies its own
redactions, but treat them as a safety net rather than a guarantee before
sending a bundle anywhere.

Start with `dry_run=true` to see what would be gathered. The default 72-hour
window is what makes a real run slow — narrow it with `since` when the problem
is recent.

See [docs/HCDIAG.md](docs/HCDIAG.md).

### Node pools

| | Tool | What it does |
|---|---|---|
| R | `list_node_pools` | Named groups of client nodes that jobs can target |
| R | `read_node_pool` | One pool: its nodes' states, job count, and **why nothing targeting it will place** |
| W | `create_node_pool` | Create or update a pool |
| W! | `delete_node_pool` | Delete a pool permanently |

### Jobs

| | Tool | What it does |
|---|---|---|
| R | `list_jobs` | Jobs in a namespace, with allocation counts rolled up |
| R | `read_job` | One job: task groups, drivers, images, resources, constraints |
| R | `read_job_summary` | Allocation counts per task group — the fastest way to see something is wrong |
| R | `list_job_allocations` | What is actually running for a job (`status` filters to, say, `failed,lost`) |
| R | `list_job_evaluations` | Scheduler decisions for a job — **where placement failures live** |
| R | `list_job_deployments` | Rollouts of a service job |
| R | `list_job_versions` | Version history with diffs between versions |
| R | `get_job_scale_status` | Desired and running counts, scaling policy bounds, recent events |
| R | `plan_job` | Dry-run a submission: what would be created, destroyed or replaced |
| R | `validate_job` | Check a jobspec parses and is legal, without submitting |
| R | `parse_job_hcl` | Convert HCL2 to Nomad's JSON job format |
| W! | `edit_job` | **Change one field of a running job** — image, count, env, CPU, memory — without rewriting its spec |
| W! | `run_job` | Submit a job, creating or replacing it wholesale |
| W! | `stop_job` | Stop a job, optionally purging it |
| W! | `scale_task_group` | Change how many allocations a task group runs |
| W! | `revert_job_version` | Roll a job back to an earlier version |
| W | `dispatch_parameterized_job` | Dispatch an instance of a parameterized job |
| W | `force_periodic_job` | Run a periodic job now |

> **Prefer `edit_job` over `run_job` for a job that already exists.** `run_job`
> replaces the job with whatever spec you submit, so anything the model did not
> think to include is dropped — and a spec reconstructed from a `read_job`
> projection always loses something. `edit_job` fetches the live job, changes the
> named fields, and carries the rest through untouched. `dry_run=true` plans it
> first.

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
| R | `list_node_allocations` | Everything running on one node (`status` filters it) |
| R | `get_node_stats` | The machine's real CPU, memory and disk use — **a full disk is a common, invisible cause of failure** |
| W | `set_node_eligibility` | Mark a node eligible or ineligible for new work |
| W | `set_node_meta` | Set or remove dynamic metadata, which is what job constraints match on |
| W | `force_evaluate_node` | Nudge the scheduler to reconsider a node |
| W! | `drain_node` | Drain a node, migrating its allocations away |
| W! | `restart_node_allocations` | **Restart everything running on a node**, in place |
| W! | `purge_node` | Remove a node from Nomad's state permanently |

> **There is no "restart a Nomad client" API.** The client agent is a process
> under the node's own init system, so only something on that machine can
> restart it. `restart_node_allocations` does what people almost always mean by
> the request — restart the work on the node, in place, without rescheduling —
> and says in its own output which of the two it did.
>
> `purge_node` refuses to run against a node that is still heartbeating. Purging
> a live node accomplishes nothing: the agent re-registers on its next beat, so
> you get the disruption without the cleanup.

### Deployments and evaluations

| | Tool | What it does |
|---|---|---|
| R | `list_deployments` | Rollouts in a namespace |
| R | `read_deployment` | One rollout, its allocations, and why it is stuck |
| R | `list_evaluations` | Scheduler evaluations — filter `Status == "blocked"` for capacity problems |
| R | `read_evaluation` | **Placement failures explained in plain language** |
| W! | `promote_deployment` | Promote canaries — all groups, or only the ones you name |
| W! | `pause_deployment` | Pause a rollout in place, or resume a paused one |
| W! | `unblock_deployment` | Force a health-blocked rollout to count as successful |
| W! | `set_deployment_alloc_health` | Mark allocations healthy or unhealthy by hand |
| W! | `fail_deployment` | Mark a deployment failed, stopping the rollout |

### Namespaces, services and volumes

| | Tool | What it does |
|---|---|---|
| R | `list_namespaces` | Namespaces defined in the cluster |
| R | `read_namespace` | One namespace: quota, node pool restrictions, metadata |
| R | `list_services` | Services in Nomad's own service discovery (`prefix`, `filter`) |
| R | `read_service` | Instances of one service: address, port, tags, owning alloc |
| R | `list_volumes` | CSI volumes or dynamic host volumes (`type` selects which; `node_id`, `plugin_id` scope it) |
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

### Enterprise only

Not registered against a cluster identified as Community Edition. See
[docs/ENTERPRISE.md](docs/ENTERPRISE.md).

| | Tool | What it does |
|---|---|---|
| R E | `get_license` | Modules covered, expiry, non-production status |
| R E | `list_quotas` | Resource quotas defined in the cluster |
| R E | `read_quota` | One quota's limits **alongside its usage**, and which namespaces it binds |
| R E | `list_sentinel_policies` | Admission policies, their scope and enforcement level |
| R E | `read_sentinel_policy` | One policy, including its source |
| R E | `list_recommendations` | Dynamic Application Sizing proposals, with the delta and direction |
| W E | `create_quota` | Create or update a quota |
| W E | `dismiss_recommendations` | Discard sizing proposals without applying them |
| W! E | `delete_quota` | Delete a quota — its namespaces become uncapped |
| W! E | `write_sentinel_policy` | Create or replace a policy. A bad one stops the cluster accepting jobs |
| W! E | `delete_sentinel_policy` | Delete a policy — whatever it enforced stops being enforced |
| W! E | `apply_recommendations` | Apply sizing proposals, which resubmits the jobs |

### Not included: ACL tools

There are deliberately no tools for creating, reading or writing ACL tokens or
policies. Other Nomad MCP servers expose these; one of them can mint a
management token directly into the model's context. The safest handling of that
capability is not to build it.

---

## Resources and prompts

**Resources** let you attach a Nomad object to a conversation directly —
`@`-mentions in Claude Code, the paperclip menu in Claude Desktop, the MCP panel
in Cursor, the `#` picker in VS Code:

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
- **`drain_node_safely`** — takes a node out of service in the order that does
  not lose work: check what is on it, confirm the rest of the cluster can absorb
  it, mark ineligible, drain, then verify the work actually landed somewhere.
  Ends differently for a node coming back than for a machine being destroyed.
  Arguments: `node_id` (required), `reason`, `permanent`.

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

## How this compares

There are other community MCP servers for Nomad, and no official HashiCorp one.
They are worth a look — this project makes a few different choices, and they are
the reason it exists rather than a claim that the alternatives are wrong:

- **Safe by default.** `NOMAD_MCP_READ_ONLY=true` is the default. Every mutating
  tool is refused by the framework rather than by each tool, with a message
  saying how to enable writes.
- **The typed Go client throughout.** Every operation goes through
  `github.com/hashicorp/nomad/api`, so there is no parallel set of hand-written
  request and response types to keep in step with Nomad.
- **Scoping as a first-class control.** A namespace allowlist enforced before
  the request is made, a separate gate for reading Variable *values*, and a byte
  cap on logs and file reads.
- **Annotations that mean something.** `readOnlyHint` on reads,
  `destructiveHint` and `idempotentHint` on writes, so a client that shows a
  confirmation prompt has something real to base it on.
- **Errors that name the fix.** A 403 says which capability your token is
  missing and in which namespace; a 404 points at the tool that lists what does
  exist; a refused connection names the address it tried.
- **Output shaped for a model.** Trimmed projections rather than raw API
  structs, and real pagination with `next_token` on every list tool.
- **No ACL tooling, deliberately.** No tool mints, reads or deletes Nomad ACL
  tokens or policies, in any form. See [docs/SECURITY.md](docs/SECURITY.md).
- **Deliberate parity with `vault-mcp-server`** — same layout, flags, env var
  names and subcommands, so knowing one means knowing the other.

## Documentation

| Document | For |
|---|---|
| [docs/QUICKSTART.md](docs/QUICKSTART.md) | **Start here.** Running in under ten minutes. |
| [docs/CONNECTING.md](docs/CONNECTING.md) | Pointing this at a cluster: local, Docker, EC2, Kubernetes, TLS, tokens |
| [docs/ENTERPRISE.md](docs/ENTERPRISE.md) | Community Edition vs Enterprise, and the twelve tools that need a licence |
| [docs/HCDIAG.md](docs/HCDIAG.md) | Support-bundle collection: enabling it, what it gathers, and handling the result |
| [docs/TESTING.md](docs/TESTING.md) | The full copy-pasteable test script, every path |
| [docs/SECURITY.md](docs/SECURITY.md) | Threat model: token scope, prompt injection, what a compromised client gets |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Guided tour of the codebase, layer by layer |

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
