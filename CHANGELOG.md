# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `0.x`, a minor release may change tool names, arguments and
output shapes.

## [Unreleased]

### Added

- **Three Autopilot tools**, bringing the catalog to 85 tools — 50 that only
  read and 35 that make changes.

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

### Changed

- The `explain_cluster_health` prompt now reads `get_autopilot_health` as its
  second step. It settles the quorum question that server peer count alone can
  only estimate.

## [0.1.0] — 2026-08-23

First public release.

**This is beta software.** Tool names, output shapes and default settings may
change before 1.0.

### What it is

An MCP server for HashiCorp Nomad. It gives an MCP client — Claude Code, Claude
Desktop, Cursor, VS Code — a set of tools for inspecting and operating a Nomad
cluster.

There are **82 tools**: 48 that only read, and 34 that make changes.

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
`drain_node_safely`. The last one checks that the rest of the cluster can absorb
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

[Unreleased]: https://github.com/suyash1603/nomad-mcp-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/suyash1603/nomad-mcp-server/releases/tag/v0.1.0
