# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Tool names, arguments and output shapes are covered by that guarantee: a
breaking change to any of them means a new major version.

## [1.4.0] — 2026-09-02

ACL policies, tokens and roles — read, create and update — behind an opt-in
switch. Nothing existing changed name, arguments or output shape, and a server
upgraded to this version offers exactly the tools it offered before unless the
new switch is set.

### Added

A new `acl` toolset of 11 tools, taking the catalog to 110 — 68 read-only, 42
mutating. It is the first toolset that `NOMAD_MCP_TOOLSETS=all` does not select.

**Reads.** `list_acl_policies` and `read_acl_policy` for what is granted;
`list_acl_tokens` and `read_acl_token` for who holds it; `list_acl_roles` and
`read_acl_role` for how it is bundled. `list_acl_tokens` flags expired and
management tokens in words, because an expired token is a common and confusing
cause of "Permission denied" and a management token in the list is a finding.

**Writes.** `write_acl_policy`, `create_acl_token`, `update_acl_token`,
`create_acl_role` and `update_acl_role`.

`update_acl_token` and `update_acl_role` read before they write. Nomad's update
endpoints replace the whole object, so sending only the changed fields would
clear the rest — renaming a token would silently strip its policies. An omitted
argument now means unchanged; a supplied list replaces its field in full, and
the response says what it displaced. `write_acl_policy` carries a policy's
workload attachment (`JobACL`) forward for the same reason.

`update_acl_role` is the widest-reaching write here: a role's policy list
applies to every token linked to it, so its response names that consequence
explicitly.

### Two new settings

| Environment variable | Flag | Default | Effect |
|---|---|---|---|
| `NOMAD_MCP_ENABLE_ACL` | `--enable-acl` | `false` | Whether the ACL tools are registered at all |
| `NOMAD_MCP_ALLOW_TOKEN_SECRETS` | `--allow-token-secrets` | `false` | Whether a response may contain a token's `SecretID` |

`NOMAD_MCP_ENABLE_ACL` is the only thing that offers the toolset.
`NOMAD_MCP_TOOLSETS=acl` on its own offers nothing and logs a warning at
startup — `--toolsets` is how operators narrow the catalog, so letting it widen
this one would be backwards.

### Still deliberately absent

- **No `bootstrap_acl_token`.** It mints a management token, which is the
  specific capability this project has refused from the start.
- **No delete tools** for policies, tokens or roles. Deletion is an
  availability change with no undo that can lock out the operator, including
  revoking the token this server authenticates with.
- **No token secrets by default.** `read_acl_token` and `create_acl_token`
  return the accessor ID and never the `SecretID`, even though Nomad returns it
  to the server on both endpoints. The operator retrieves it with `nomad acl
  token info <accessor_id>` at a terminal.

See [docs/SECURITY.md](docs/SECURITY.md) for the reasoning and how the four
gates compose.

## [1.3.0] — 2026-09-01

Capacity and sizing: three tools that do the arithmetic Nomad leaves to you.
Nothing existing changed name, arguments or output shape.

### Added

A new `capacity` toolset, taking the catalog to 99 — 62 read-only, 37 mutating.

**`get_cluster_capacity`** reports what the cluster has, what is allocated, and
what is left, broken down by node pool, datacenter or node class. Capacity on
nodes that are down, draining or ineligible is reported separately and never
counted as available.

Alongside every total it reports the largest amount actually placeable on a
**single node**, because that is the number that decides placement. Cluster-wide
free capacity is nearly meaningless in Nomad: ten nodes with 1GB free each
cannot run one 2GB task, and a cluster reporting 10GB free will still refuse it.

**`explain_placement`** works out node by node whether a job's task groups fit,
and for every node that cannot take them, which specific thing rules it out —
the datacenter, the node pool, the node's state, or how much CPU or memory is
short, with both numbers.

Nomad does not report this. A failed evaluation gives aggregate counters
("12 nodes filtered") and never says which node lacked what.

It evaluates datacenters, node pools, node state, resource fit and
`distinct_hosts`. It does **not** evaluate other constraint blocks, affinities,
spread or device requirements; those are listed unevaluated in the result, and
`plan_job` remains the authoritative answer.

**`analyze_job_resources`** compares what each task reserved with what it is
observed using across every running allocation, and reports OOM kills read from
task events — so they are found even for allocations that already died and even
when current usage looks fine.

Nomad stores no usage history, so each measurement is a single instantaneous
sample taken during the call, not an average or a percentile. That is enough to
catch gross over-provisioning and imminent OOM, and not enough to size a spiky
workload; every result says so. Peak memory is reported only where the driver
measures it, rather than showing zero where it does not.

## [1.2.0] — 2026-08-29

Storage and integration troubleshooting: the two areas where the cause is
furthest from the symptom. Nothing existing changed name, arguments or output
shape.

### Added

Five tools, taking the catalog to 96 — 59 read-only, 37 mutating.

#### Vault and Consul, read from the Nomad side

**`diagnose_integrations`** scans task events cluster-wide for the failures
whose cause lives in Vault or Consul but whose only record is in Nomad: token
derivation, template rendering, Connect sidecar startup, service registration
and workload identity. It clusters and ranks the hits, and names the role or
path to investigate.

These are hard to find any other way because the task usually **never starts**,
so there are no logs — `read_allocation_logs` returns nothing and the job looks
fine.

It **reads Nomad only** and holds no Vault or Consul credentials. From the agent
configuration it reads only whether Vault is enabled and how many Consul
clusters are configured; never a token, address, role or TLS path. See
[docs/SECURITY.md](docs/SECURITY.md).

#### Storage

**`diagnose_volume`** follows a volume through its CSI plugin, its claims, the
allocations holding them and the nodes those are on — one call in place of five.
It detects **stale claims**: a volume still held by an allocation that is dead.
That is the most common cause of a volume that looks entirely healthy in
`read_volume` and will not attach, and it makes a new allocation sit pending
indefinitely with nothing obviously wrong.

**`list_csi_plugins`** and **`read_csi_plugin`** show healthy-versus-expected
controller and node counts, and name which specific instances are unhealthy.
When a volume will not mount, the answer is usually here rather than in the
volume — a plugin short of a node instance blocks placement on exactly those
nodes while the volume itself still looks fine.

#### Health checks

**`get_allocation_checks`** returns the health check results for an allocation:
the gap between "running" and "actually working". An allocation can be running
and failing every check it has, and nothing in `read_allocation` says so — which
is the usual explanation for a deployment that places allocations but never
progresses.

An allocation with no checks says so explicitly rather than reporting healthy.
No checks and no failing checks look identical in the data and mean opposite
things.

## [1.1.0] — 2026-08-27

Three investigation tools, for clusters large enough that reading one object at
a time stops working. Nothing existing changed name, arguments or output shape.

### Added

#### Investigation tools

A new `investigate` toolset. These fan out across many allocations and
correlate several object types, so one call replaces a sequence of five or six.

| Tool | What it answers |
|---|---|
| `find_problems` | "Is anything broken?" — one ranked list of everything currently wrong |
| `search_job_logs` | "Which replica is throwing this error?" — grep every allocation at once |
| `build_job_timeline` | "What happened, and in what order?" |

**`find_problems`** scans allocations, evaluations, deployments, jobs and nodes
concurrently and returns ranked findings — failed and lost allocations, blocked
evaluations, stuck deployments, queued work, nodes down or draining or
ineligible. Each finding carries a severity, a true total count, example IDs and
the specific tool to call next. It notices things a per-object read cannot, such
as every failed allocation sitting on one node.

**`search_job_logs`** reads every allocation of a job concurrently, matches on
the server side, and returns only matching lines with the allocation, node and
task they came from. Failed and lost allocations are searched first, so the
target cap keeps the ones most likely to explain a problem.

**`build_job_timeline`** merges job version submissions (with the fields each
one changed), evaluations, deployment start and completion, and per-task
allocation events into one chronological list. Ties within the same second break
in causal order — version, then evaluation, then allocation, then task event —
because Nomad records all four to the nanosecond but they routinely share a
second.

#### Honesty about what Nomad cannot do

Nomad's log API has no time filter. Logs are files on the client that Nomad
rotates, and a rescheduled allocation's logs go with it.

`search_job_logs` accepts `since` and `until`, applied to lines the workload
timestamped itself — bracketed and bare RFC3339, and space-separated dates. When
no line carried a parseable timestamp the filter had no effect, and
`time_filter_note` says exactly that instead of implying a time range that was
never enforced. A search that matched nothing reports that it is not proof the
event never happened, and a scan whose checks failed reports unknown rather than
healthy. Each of those three is covered by a test.

### Fixed

- The binary inside the container image reported an empty commit and a
  1970 build date. The image build never passed `-ldflags` for
  `version.GitCommit` or `version.BuildDate`, so there was no way to tell which
  source a running container came from.
- A failed image job left the release marked Latest, advertising a container
  that was never pushed — which is how 1.0.0 shipped with no image at all.
  Releases are now created as a draft and published only once the binaries and
  the image are both up.

## [1.0.1] — 2026-08-27

Groundwork for running against large clusters. No tool was removed, renamed or
changed shape, and a server started with no new configuration behaves exactly
as 1.0.0 did.

### Added

#### Toolsets

`NOMAD_MCP_TOOLSETS` selects which groups of tools the server offers. The
default is `all`, which is the whole catalog as before.

```bash
NOMAD_MCP_TOOLSETS=jobs,allocs,deployments nomad-mcp-server stdio
```

| Toolset | Covers |
|---|---|
| `system` | Cluster status, regions, node pools, agent and scheduler config, Raft, Autopilot, search |
| `jobs` | Job specifications, versions, planning and submission |
| `allocs` | Allocations, task logs, allocation files, resource statistics |
| `nodes` | Client nodes, draining, eligibility |
| `deployments` | Deployments and scheduler evaluations |
| `catalog` | Namespaces, service registrations, storage volumes |
| `variables` | Nomad Variables |
| `diag` | hcdiag support-bundle collection |
| `enterprise` | Licence, quotas, Sentinel, Dynamic Application Sizing |

This exists for two reasons. Eighty-eight tool definitions are sent to the model
on every request, which is a real and growing cost; and a toolset that is never
registered is a scoping control in its own right — a server without the
`variables` toolset cannot read a Variable whatever its token permits.

An unknown toolset name is refused at startup, with the valid names in the
error. A server that came up quietly missing a third of its tools would be
indistinguishable, from the model's side, from one that never had them.

#### Filtering for large clusters

- `list_job_allocations` and `list_node_allocations` accept `status`, one or
  several of `pending`, `running`, `complete`, `failed`, `lost`, `unknown`.
  Neither endpoint paginates, so a job with hundreds of allocations previously
  returned all of them. `status="failed,lost"` is the common case.
- `list_services` accepts `prefix` and `filter`, matching the other paginated
  list tools.
- `list_volumes` accepts `node_id` and `plugin_id`, applied by the Nomad servers
  rather than here.

An empty result now says whether a filter caused it. "Nothing is failing" and
"your filter excluded everything" call for opposite next steps.

### Internal

A bounded fan-out helper (`utils.FanOut`) for tools that visit many
allocations or nodes in one call: a concurrency cap so a diagnostic does not
become a denial of service on the cluster it is diagnosing, a target cap, a
wall-clock budget, deterministic output order, and deduplicated errors. It
reports what it did *not* reach as prominently as what it found, so a sampled
scan cannot be read as an exhaustive one.

Nothing calls it yet. It ships now because the investigation tools that use it
land next, and they should all inherit the same limits rather than each
growing their own.

## [1.0.0] — 2026-08-26

First public release.

### What it is

An MCP server for HashiCorp Nomad. It gives an MCP client — Claude Code, Claude
Desktop, Cursor, VS Code — a set of tools for inspecting and operating a Nomad
cluster.

There are **88 tools**: 51 that only read, and 37 that make changes.

### Safe by default

Four settings decide what the server is allowed to do. All four default to the
safe option, so a server you start with no configuration can only read.

| Setting | Default | What it does |
|---|---|---|
| `NOMAD_MCP_READ_ONLY` | `true` | Refuses every tool that changes anything |
| `NOMAD_MCP_ALLOW_DESTRUCTIVE` | `true` | When `false`, allows writes but refuses anything that discards state or interrupts running work |
| `NOMAD_MCP_ALLOW_VARIABLE_READS` | `false` | Controls reading Nomad Variable *values*, separately from write access |
| `NOMAD_MCP_ALLOWED_NAMESPACES` | unset | Limits the server to named namespaces, checked before any request is sent |

Two more things are true regardless of configuration:

- **There are no ACL tools.** Nothing in this server creates, reads or deletes
  Nomad ACL tokens or policies.
- **Task logs, allocation files and job metadata are labelled as untrusted** in
  the output, because a workload writes them.

The full reasoning is in [docs/SECURITY.md](docs/SECURITY.md).

### Tools

- **Cluster** — `get_cluster_status`, `list_regions`, `list_node_pools`,
  `read_node_pool`, `create_node_pool`, `delete_node_pool`, `get_agent_config`,
  `search`, `check_connection`
- **Servers** — `get_autopilot_health`, `get_autopilot_config`,
  `set_autopilot_config`, `get_raft_config`, `remove_raft_peer`,
  `transfer_leadership`
- **Jobs** — `list_jobs`, `read_job`, `read_job_summary`, `edit_job`,
  `list_job_allocations`, `list_job_evaluations`, `list_job_deployments`,
  `list_job_versions`, `get_job_scale_status`, `plan_job`, `validate_job`,
  `parse_job_hcl`, `run_job`, `stop_job`, `scale_task_group`,
  `revert_job_version`, `dispatch_parameterized_job`, `force_periodic_job`
- **Allocations** — `list_allocations`, `read_allocation`,
  `read_allocation_logs`, `list_allocation_files`, `read_allocation_file`,
  `get_allocation_stats`, `restart_allocation`, `stop_allocation`,
  `signal_allocation`
- **Nodes** — `list_nodes`, `read_node`, `list_node_allocations`,
  `get_node_stats`, `set_node_eligibility`, `set_node_meta`, `drain_node`,
  `force_evaluate_node`, `restart_node_allocations`, `purge_node`
- **Deployments and scheduling** — `list_deployments`, `read_deployment`,
  `list_evaluations`, `read_evaluation`, `promote_deployment`,
  `pause_deployment`, `unblock_deployment`, `set_deployment_alloc_health`,
  `fail_deployment`, `get_scheduler_config`, `set_scheduler_config`
- **Catalog** — `list_namespaces`, `read_namespace`, `create_namespace`,
  `delete_namespace`, `list_services`, `read_service`, `list_volumes`,
  `read_volume`
- **Variables** — `list_variables`, `read_variable`, `write_variable`,
  `delete_variable`
- **Enterprise (12)** — `get_license`, `list_quotas`, `read_quota`,
  `create_quota`, `delete_quota`, `list_sentinel_policies`,
  `read_sentinel_policy`, `write_sentinel_policy`, `delete_sentinel_policy`,
  `list_recommendations`, `apply_recommendations`, `dismiss_recommendations`
- **Diagnostics** — `collect_hcdiag`

Every tool carries an MCP annotation — `readOnlyHint` on reads,
`destructiveHint` and `idempotentHint` on writes — so a client that asks for
confirmation has something real to base it on.

### Notable tools

**`edit_job`** changes one thing about a running job: an image, a count, an
environment variable, a CPU or memory reservation. Nomad's only update path is
registering the whole job again, so without this the model has to rebuild the
full specification and resubmit it. Set `dry_run=true` to plan the change first.

**`check_connection`** reports the address, TLS setting, token, ACL state and
edition, and probes each permission. Every failure comes with a specific fix
rather than a status code.

**`collect_hcdiag`** collects a HashiCorp
[hcdiag](https://github.com/hashicorp/hcdiag) support bundle.

- Off by default. Enable it with `NOMAD_MCP_ENABLE_HCDIAG=true`.
- It is the only tool that runs a program on your machine. Every other tool
  calls the Nomad API.
- It returns the bundle's file path, not the bundle's contents.
- Setup and safety notes: [docs/HCDIAG.md](docs/HCDIAG.md)

### Servers, Autopilot and Raft

- **Three Autopilot tools and three Raft tools** — three that report on the
  server fleet's Autopilot and Raft state, and three that repair a quorum
  problem once it is understood.

  - `get_autopilot_health` reports Autopilot's assessment of the server fleet:
    how many more servers can be lost before quorum goes, which servers are
    voters, how far each trails the leader, and how long each has been stable.
    A failure tolerance of `0` is reported as degraded even when every server
    is currently healthy, because that cluster looks fine right up until the
    next server dies and takes the whole control plane with it.
  - `get_autopilot_config` reports the settings behind those verdicts:
    dead-server cleanup, the contact and trailing-log thresholds, the voter
    promotion delay and the minimum quorum. `cleanup_dead_servers = false` is
    called out specifically — it is the usual reason a cluster that was rolled
    through a server replacement still counts the servers it no longer has.

  - `set_autopilot_config` changes those settings. It is annotated
    **destructive**, so it is refused when `NOMAD_MCP_ALLOW_DESTRUCTIVE=false`:
    turning `cleanup_dead_servers` on gives Autopilot permission to remove
    servers from the Raft peer set, which is right for pruning replaced servers
    and wrong for servers that are merely unreachable. It reads the current
    configuration first so an omitted argument keeps its value rather than
    resetting, and writes with a compare-and-set on the modify index, so a
    concurrent change by another operator is refused rather than overwritten.
    Durations are given as strings (`"200ms"`, `"2m"`); a bare number is
    refused, since the units it meant cannot be recovered.

  Autopilot governs the server fleet and Raft quorum, which is a different
  subsystem from the scheduler configuration tools; all three say so, since the
  two are easy to confuse.

- **Three Raft tools.** Autopilot reports on the servers it knows about; Raft
  counts the entries in its configuration. They differ in the case that matters,
  and until now nothing here could see the difference.

  - `get_raft_config` reports the peer set and the quorum arithmetic, flagging
    entries Nomad shows as `(unknown)` — servers that were destroyed or replaced
    but never removed. Those still count toward quorum while contributing
    nothing to it, which is how a cluster with three live servers turns out to
    need three of five votes to elect a leader.
  - `remove_raft_peer` removes such an entry, which lowers the quorum
    requirement and can be what lets a cluster elect a leader again. It refuses
    to remove the leader, and refuses to remove a peer Autopilot currently
    reports as healthy — the `purge_node` guard applied to servers. That health
    check is best-effort by design: it is answered by the leader, so it fails on
    precisely the leaderless cluster where removing a dead peer is the repair,
    and a failure to check reports itself rather than blocking the fix.
  - `transfer_leadership` hands leadership to another server before taking the
    leader out of service. It refuses a non-voter, an orphaned entry and an
    unhealthy target, and reports that the transfer completes asynchronously
    rather than implying it is already done.

  Both writes are annotated destructive, so they are refused under
  `NOMAD_MCP_ALLOW_DESTRUCTIVE=false`.

### Enterprise detection

The server checks the cluster once at startup and hides the 12 Enterprise-only
tools when it confirms you are running Community Edition. If the check is
inconclusive — for example the server started before Nomad did — it offers the
tools rather than hiding them.

Override the result with `NOMAD_MCP_ENTERPRISE=auto|true|false`. See
[docs/ENTERPRISE.md](docs/ENTERPRISE.md).

### Resources and prompts

Five resources: `nomad://cluster`, `nomad://jobs`,
`nomad://jobs/{namespace}/{job_id}`, `nomad://allocs/{alloc_id}` and
`nomad://nodes/{node_id}`. Each returns exactly what the matching tool returns.

Three prompts: `troubleshoot_failing_job`, `explain_cluster_health` and
`drain_node_safely`. `explain_cluster_health` reads `get_autopilot_health` as
its second step, settling the quorum question that server peer count alone can
only estimate. `drain_node_safely` checks that the rest of the cluster can absorb
the load *before* the drain is issued — a drain with nowhere to reschedule to
does not fail, it loses the work.

### Transports

`stdio` and `streamable-http`, plus a deprecated `http` alias for consistency
with `vault-mcp-server`.

### Configuration

All ten standard `NOMAD_*` variables are supported, plus `TRANSPORT_*` and
`MCP_*` matching `vault-mcp-server`. Every setting is also a command-line flag,
and a flag beats an environment variable.

`NOMAD_TOKEN` is deliberately environment-only — there is no `--nomad-token`
flag, because command-line arguments are visible in the process list.

### Error messages

Nomad's errors are translated into messages that name the fix:

- A **403** names the capability your token is missing, and the namespace. Nomad
  never includes this — its 403 body is only ever `Permission denied`.
- A **404** points at the tool that lists what does exist.
- A **connection failure** names the address it tried, and the setting to change.
- A **501** explains that the endpoint needs Nomad Enterprise.

### Tested

- Unit tests against a fake Nomad built from the real `api` structs.
- End-to-end tests that start a throwaway `nomad agent -dev` and drive the
  built binary as a subprocess.
- A separate suite for the HTTP transport, covering CORS rejection, the health
  endpoint, and the refusal to bind `0.0.0.0` without TLS.
- A test per write tool asserting it is refused when `NOMAD_MCP_READ_ONLY=true`.

[1.0.0]: https://github.com/suyash1603/nomad-mcp-server/releases/tag/v1.0.0
