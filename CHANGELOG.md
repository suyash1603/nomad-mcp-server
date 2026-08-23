# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `0.x`, minor releases may contain breaking changes to tool
names, arguments and output shapes.

## [Unreleased]

### Added

**27 new tools — 81 in total, 47 read-only and 34 mutating.**

- *Node pools*: `read_node_pool`, `create_node_pool`, `delete_node_pool`.
  `read_node_pool` says why nothing targeting a pool will place, rather than
  leaving the counts for someone to interpret.
- *Jobs*: `edit_job` — change an image, count, environment variable, CPU or
  memory reservation on a live job without rewriting its specification. Nomad's
  only update path is registering a whole job, so the alternative was having the
  model rebuild the spec from a lossy `read_job` projection and submit that,
  silently dropping whatever the projection did not carry. `dry_run=true` plans
  the change first.
- *Nodes*: `get_node_stats`, `set_node_meta`, `force_evaluate_node`,
  `restart_node_allocations`, `purge_node`.
- *Deployments*: `pause_deployment`, `unblock_deployment`,
  `set_deployment_alloc_health`. `promote_deployment` now accepts `task_groups`
  to promote named groups instead of all of them.
- *Scheduler*: `get_scheduler_config`, `set_scheduler_config`. The former names
  the two settings that stop a cluster scheduling with no error anywhere —
  `reject_job_registration` and `pause_eval_broker`.
- *Connection*: `check_connection`, which reports address, TLS, token, ACL
  state, edition and per-capability permission probes, each failure with a
  concrete fix rather than a status code.
- *Enterprise (12, not registered against Community Edition)*: `get_license`,
  `list_quotas`, `read_quota`, `create_quota`, `delete_quota`,
  `list_sentinel_policies`, `read_sentinel_policy`, `write_sentinel_policy`,
  `delete_sentinel_policy`, `list_recommendations`, `apply_recommendations`,
  `dismiss_recommendations`.

**Edition detection.** The server probes the cluster once at startup, from the
agent's version string with the licence endpoint as tiebreaker, and drops the
Enterprise-only tools when it positively identifies Community Edition. An
unreachable or inconclusive probe offers them, so a server started before its
Nomad does not come up missing a third of its catalog. `NOMAD_MCP_ENTERPRISE`
overrides the decision with `auto`, `true` or `false`.

**A destructive-operation tier.** `NOMAD_MCP_ALLOW_DESTRUCTIVE=false` permits
writes while refusing anything that discards state or interrupts running work.
It defaults to `true`, so enabling writes still enables all of them, and it
classifies from the `destructiveHint` annotation the tools already carry rather
than from a list that could drift.

**A third prompt.** `drain_node_safely` orders a node evacuation so the step
everyone skips — confirming the rest of the cluster can absorb the load, before
the drain is issued — happens first. A drain with nowhere to reschedule to does
not fail; it loses the work.

### Documentation

- `docs/CONNECTING.md` — pointing the server at a cluster running locally, in
  Docker, on EC2, in Kubernetes or behind TLS, with a symptom-to-cause table.
- `docs/ENTERPRISE.md` — what differs between the two editions and how the
  server decides.

### Notes

- There is no Nomad API that restarts a client agent; the agent is a process
  under the node's own init system. `restart_node_allocations` does what the
  request usually means — restart the work on the node, in place — and says so
  in its own output rather than letting anyone believe otherwise.
- `purge_node` refuses a node that is still heartbeating. Nomad does not stop
  you, but the agent re-registers on its next beat, so the purge achieves
  nothing while the disruption is real.

## [0.1.0] — 2026-08-21

First release. Beta: tool names, output shapes and defaults may change before
1.0.

### Added

**Tools — 54 in total, 37 read-only and 17 mutating.**

- *Cluster*: `get_cluster_status`, `list_regions`, `list_node_pools`,
  `get_agent_config`, `search`.
- *Jobs*: `list_jobs`, `read_job`, `read_job_summary`, `list_job_allocations`,
  `list_job_evaluations`, `list_job_deployments`, `list_job_versions`,
  `get_job_scale_status`, `plan_job`, `validate_job`, `parse_job_hcl`,
  `run_job`, `stop_job`, `scale_task_group`, `revert_job_version`,
  `dispatch_parameterized_job`, `force_periodic_job`.
- *Allocations*: `list_allocations`, `read_allocation`, `read_allocation_logs`,
  `list_allocation_files`, `read_allocation_file`, `get_allocation_stats`,
  `restart_allocation`, `stop_allocation`, `signal_allocation`.
- *Nodes*: `list_nodes`, `read_node`, `list_node_allocations`,
  `set_node_eligibility`, `drain_node`.
- *Scheduler*: `list_deployments`, `read_deployment`, `list_evaluations`,
  `read_evaluation`, `promote_deployment`, `fail_deployment`.
- *Catalog*: `list_namespaces`, `read_namespace`, `create_namespace`,
  `delete_namespace`, `list_services`, `read_service`, `list_volumes`,
  `read_volume`.
- *Variables*: `list_variables`, `read_variable`, `write_variable`,
  `delete_variable`.

**Resources.** `nomad://cluster`, `nomad://jobs`,
`nomad://jobs/{namespace}/{job_id}`, `nomad://allocs/{alloc_id}` and
`nomad://nodes/{node_id}`. Each returns exactly what the equivalent tool
returns, byte for byte.

**Prompts.** `troubleshoot_failing_job` and `explain_cluster_health`.

**Transports.** `stdio` and `streamable-http`, plus a deprecated `http` alias
matching `vault-mcp-server`.

**Safety.**
- `NOMAD_MCP_READ_ONLY` defaults to `true`. The gate derives from each tool's
  MCP `readOnlyHint`, and an unannotated tool is treated as mutating.
- `NOMAD_MCP_ALLOW_VARIABLE_READS` defaults to `false`, gating Variable values
  independently of write access.
- `NOMAD_MCP_ALLOWED_NAMESPACES` confines the server to named namespaces,
  enforced before any request is made.
- No ACL tools exist, in any form.
- Task logs, allocation files and job metadata are labelled as untrusted output.

**Configuration.** All ten standard `NOMAD_*` variables, plus
`TRANSPORT_*`/`MCP_*` matching `vault-mcp-server`. Every setting is also a flag,
and a flag beats the environment. `NOMAD_TOKEN` is deliberately
environment-only.

**Error mapping.** Nomad's errors are translated into messages that name the
fix: a 404 points at the relevant list tool, a 403 names the capability the
endpoint needed (Nomad's 403 body never does), a connection failure names
`NOMAD_ADDR`, and a 501 explains that the endpoint requires Nomad Enterprise.

**Testing.** Unit tests against a fake Nomad built from the real `api` structs;
end-to-end tests that start a throwaway `nomad agent -dev` and drive the built
binary as a subprocess; a separate suite for the HTTP transport. 164 unit test
functions, 16 e2e, 52% statement coverage.

### Fixed

Found during development, listed because each is a class of mistake rather than
a one-off:

- **Rate limiting no longer applies on the stdio transport.** A normal
  troubleshooting sequence — a dozen tool calls in a couple of seconds — was
  being refused partway through, and `MCP_RATE_LIMIT_*` are scoped to the HTTP
  subcommands, so a throttled stdio user had no flag to raise.
- **Exit codes no longer vanish from failed tasks.** The projection read
  `ExitCode` from the last task event, but a task that fails often enough for
  Nomad to stop restarting it ends on `Not Restarting`, which carries no exit
  code; `omitempty` then hid the zero.
- **The query-string credential guard now normalises parameter names.**
  `url.Values.Get` is case-sensitive, so `nomad_token` passed straight through
  while `NOMAD_TOKEN` and `token` were both blocked.
- **`drain_node` is no longer annotated idempotent.** Its `deadline` is relative
  to when the drain is issued, so re-draining a node pushes its forced eviction
  out by another full deadline — a client skipping re-confirmation on an
  "idempotent" retry would move a production node's eviction clock silently.
- **Resource URI template variables are unwrapped correctly.** mcp-go stores a
  matched variable as `[]string`, not `string`, so every templated resource read
  failed as "missing segment" while the URI matching underneath worked.

[Unreleased]: https://github.com/suyash1603/nomad-mcp-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/suyash1603/nomad-mcp-server/releases/tag/v0.1.0
