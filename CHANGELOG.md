# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `0.x`, minor releases may contain breaking changes to tool
names, arguments and output shapes.

## [Unreleased]

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
