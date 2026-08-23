# Community Edition and Enterprise

This server works against both builds of Nomad. It works out what it is talking
to and behaves accordingly, so there is nothing to configure in the normal case.

## What the server does about it

At startup it probes the cluster once:

1. **The agent's version string.** Enterprise builds carry a `+ent` suffix —
   `1.9.3+ent`, `1.10.0+ent.hsm`. Reading it needs no special capability.
2. **The licence endpoint.** `GET /v1/operator/license` returns a licence on
   Enterprise and a `501 Nomad Enterprise only endpoint` on Community Edition,
   which makes it conclusive on its own. It is also where the licence detail
   comes from, when the token may read it.

The result is cached for fifteen minutes and reported by `get_cluster_status`
and `check_connection`.

Twelve of the server's 81 tools call endpoints that exist only in Enterprise. On
a cluster identified as Community Edition they are **not registered at all**, so
the model never sees a tool it cannot use. On Enterprise, or when the probe is
inconclusive, they are offered.

Offering them on an inconclusive probe is deliberate. A server started before its
Nomad, or given a token that cannot read the agent, should not come up silently
missing a third of its catalog. If such a tool is called against Community
Edition anyway, Nomad's 501 is translated into a plain sentence rather than
surfaced as a status code:

```
Cannot list quotas: this requires Nomad Enterprise, and the cluster at
https://nomad.example.com:4646 is running Nomad Community Edition.
```

## Overriding the decision

`NOMAD_MCP_ENTERPRISE` takes three values:

| Value | Behaviour |
|---|---|
| `auto` *(default)* | Probe the cluster. Hide the Enterprise tools only on a positive identification of Community Edition |
| `true` | Always offer them, without probing |
| `false` | Never offer them |

`false` is worth setting on a Community Edition cluster if you would rather the
model never saw those twelve tools at all — a smaller catalog is a slightly
sharper one. `true` is for a cluster the probe cannot identify but you know is
Enterprise.

## The Enterprise-only tools

### Licence

| Tool | What it does |
|---|---|
| `get_license` | Modules covered, expiry, whether it is a non-production licence |

Reading the licence needs `operator:read`, which most read tokens do not carry.
The signed licence blob and customer identifier are deliberately not returned —
neither helps diagnose anything, and both are the sort of thing that should not
end up in a chat transcript.

An expiring licence is surfaced as a warning at thirty days, because Enterprise
features stop applying when it lapses.

### Resource quotas

| Tool | What it does |
|---|---|
| `list_quotas` | Quotas defined in the cluster, with what each caps |
| `read_quota` | One quota's limits alongside its current usage, and which namespaces it is attached to |
| `create_quota` | Create or update a quota |
| `delete_quota` | Delete one permanently |

A quota caps the total CPU and memory that jobs in its attached namespaces may
**reserve**. Two things about that are easy to get wrong, and both tools say so
in their output:

- **Usage is reservation, not consumption.** A namespace can exhaust its quota
  while its tasks sit idle. Stopping a job frees its reservation; a task using
  less than it asked for does not.
- **A quota constrains nothing until a namespace points at it.** Creating one
  changes nothing on its own. Attach it with `create_namespace`. `read_quota`
  warns when a quota has no namespaces attached, which is a common and
  completely silent misconfiguration.

Lowering a limit below current usage does not stop anything running. Existing
allocations keep their reservations; what changes is that nothing new can be
placed until usage falls back under the cap.

### Sentinel policies

| Tool | What it does |
|---|---|
| `list_sentinel_policies` | Policies, their scope and enforcement level |
| `read_sentinel_policy` | One policy, including its source |
| `write_sentinel_policy` | Create or replace a policy |
| `delete_sentinel_policy` | Delete one permanently |

Sentinel runs at job submission and can reject a job that breaks a rule. When
`run_job` or `plan_job` fails with a policy error rather than a scheduling
error, these are what rejected it.

`write_sentinel_policy` is annotated destructive, which is not about the write
itself: a policy with a mistake in it at `hard-mandatory` stops the entire
cluster accepting jobs, including the deploys someone needs during an incident.
The tool's description tells the model to introduce new policies at `advisory` —
which logs and allows — and raise the level only after real submissions have
confirmed it passes what it should.

`hard-mandatory` cannot be overridden by anyone, including whoever wrote it.

Policy source is returned labelled as untrusted input, for the same reason job
metadata and task logs are: it is text from the cluster, read here as data.

### Dynamic Application Sizing

| Tool | What it does |
|---|---|
| `list_recommendations` | Nomad's CPU and memory proposals, with the delta and direction |
| `apply_recommendations` | Apply them, which resubmits the jobs |
| `dismiss_recommendations` | Discard proposals without applying them |

DAS watches what tasks actually consume and proposes reservations from it. This
is the honest answer to "is anything over-provisioned", because it comes from
observed usage rather than guesswork.

`apply_recommendations` is not a settings change: it rewrites the job's resource
block and resubmits, which starts a deployment and replaces the running
allocations. Lowering a reservation is the risky direction — the recommendation
is based on usage that has happened, which does not include the peak that has
not, and a task cut to its steady-state memory gets OOM-killed by its next
spike.

An empty recommendation list is not an error. It usually means DAS is not
enabled, has not gathered enough data yet, or the current reservations already
match usage.

## Enterprise features that are not separate tools

Some Enterprise functionality is reached through tools that exist on both
editions, and simply does more where it is licensed:

- **Node pool scheduler configuration.** `create_node_pool` accepts
  `scheduler_algorithm` and `memory_oversubscription`; both are per-pool
  overrides that only exist on Enterprise. The tool omits the block entirely
  when neither is asked for, so it does not send an Enterprise-only field to a
  Community cluster for a setting nobody requested.
- **Cluster scheduler configuration.** `set_scheduler_config` accepts
  `scheduler_algorithm = "spread"` and `memory_oversubscription`, both
  Enterprise. Everything else on that tool works on either edition.
- **Namespace quota attachment.** `read_namespace` reports a namespace's quota
  on either edition; only Enterprise has quotas to report.

## Deliberately absent

There are no tools for creating, reading or writing ACL tokens or policies on
either edition. Other Nomad MCP servers expose these; one of them can mint a
management token directly into the model's context. The safest handling of that
capability is not to build it.

There is no `apply_license` tool. Installing a licence is a one-time operator
action with no diagnostic value, and putting a licence blob through a model's
context achieves nothing that `nomad license put` does not do better.
