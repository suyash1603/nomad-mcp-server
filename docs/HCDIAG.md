# Support bundles with hcdiag

[hcdiag](https://github.com/hashicorp/hcdiag) is HashiCorp's own diagnostics
collector. It gathers what a support engineer asks for and what is tedious to
assemble by hand — agent configuration, host information, Nomad's debug bundle,
metrics and recent logs — into a single `.tar.gz`.

This server can run it for you, through one tool: `collect_hcdiag`.

---

## When to use it, and when not to

Use it when the problem is beyond what the read tools explain, when someone asks
for "an hcdiag" or "a support bundle", or before opening a support ticket.

Do **not** reach for it to answer a specific question. "Why won't this job
place?" is answered in seconds by `read_evaluation`, with the answer in front of
you. hcdiag produces an archive for a human to work through, and a real run
takes minutes.

---

## Enabling it

Off by default, and it is the only tool with its own switch:

```bash
NOMAD_MCP_ENABLE_HCDIAG=true
```

That is deliberate. Every other tool in this server calls Nomad's HTTP API. This
one **executes a program on the machine running the MCP server** and writes a
file there, which is a different class of capability and should be turned on
knowingly rather than inherited from a read-only/read-write decision that was
about the cluster.

You also need hcdiag itself:

```bash
brew install hashicorp/tap/hcdiag
# or download from https://releases.hashicorp.com/hcdiag/
```

**Install it on the host running this MCP server**, not on the Nomad servers.
That is where the process runs it. hcdiag in turn shells out to the `nomad` CLI,
so that needs to be on the same host and able to reach the cluster — if
`check_connection` passes, it can.

### Settings

| Variable | Default | What it does |
|---|---|---|
| `NOMAD_MCP_ENABLE_HCDIAG` | `false` | Master switch |
| `NOMAD_MCP_HCDIAG_PATH` | `hcdiag` | Binary path, or a name to find on `PATH` |
| `NOMAD_MCP_HCDIAG_DEST` | system temp dir | Directory bundles must be written under |
| `NOMAD_MCP_HCDIAG_TIMEOUT` | `10m` | Maximum time one collection may run |

A sensible production setup confines the output:

```bash
NOMAD_MCP_ENABLE_HCDIAG=true
NOMAD_MCP_HCDIAG_DEST=/var/lib/nomad-mcp/bundles
NOMAD_MCP_HCDIAG_TIMEOUT=15m
```

With `NOMAD_MCP_HCDIAG_DEST` set, a `destination` argument outside that
directory is refused — including by way of `..`, which is checked on cleaned
absolute paths rather than string prefixes.

---

## Using it

Ask for it in words. Some things that work:

```
"collect an hcdiag bundle for this cluster"
"grab a support bundle covering the last 2 hours"
"show me what an hcdiag would collect, without running it"
"collect hcdiag including Consul"
```

### Arguments

| Argument | Default | What it does |
|---|---|---|
| `since` | `72h` | How far back to collect logs and metrics |
| `dry_run` | `false` | List what would be gathered, gather nothing |
| `include_consul` | `false` | Also collect Consul diagnostics |
| `include_vault` | `false` | Also collect Vault diagnostics |
| `destination` | configured dir | Where to write the bundle |
| `config_file` | — | Path to an hcdiag HCL config, for advanced collections |

**`since` is the setting that matters for speed.** The 72-hour default is
hcdiag's own and is the main reason a run is slow. After an incident this
morning, `since: "2h"` collects far less and finishes far sooner.

**Start with `dry_run: true`.** It is fast, writes nothing, and lists exactly
what a real run would gather.

`include_consul` is worth setting when Nomad uses Consul for service discovery
and the symptom involves service registration or mesh connectivity.
`include_vault` when tasks get their secrets from Vault and templates are
failing to render. Both need that product's CLI and credentials on the same
host.

---

## What comes back

The path, the size, and a summary of what was collected:

```json
{
  "bundle_path": "/var/lib/nomad-mcp/bundles/hcdiag-2026-08-23T104500Z.tar.gz",
  "bundle_size": "14.2 MB",
  "duration": "3m41s",
  "collected": {
    "duration": "3m38s",
    "operations": 47,
    "hcdiag_version": "0.5.4",
    "operations_by_product": { "nomad": 31, "host": 16 },
    "failed_operations": ["nomad: nomad operator debug"]
  }
}
```

**The contents are never returned, and the tool tells the model not to read
them.** A bundle contains agent configuration, environment variables and logs —
on a real cluster, that means credentials. Putting it into a model's context
would defeat the point of every other control in this server. You get a path;
what happens to the file is your decision.

`failed_operations` is worth reading. hcdiag exits non-zero when some runners
fail but still writes a usable bundle for the rest, so a partial collection is
reported as `"partial": true` **with** the bundle rather than as a flat failure.
A bundle missing the one thing you needed otherwise looks identical to a
complete one.

### Before you send it anywhere

hcdiag applies its own redactions. Treat them as a safety net, not a guarantee.
Look inside before a bundle goes to a third party:

```bash
tar tzf hcdiag-*.tar.gz | head -50      # what is in it
tar xzf hcdiag-*.tar.gz                 # unpack and read
```

---

## How the tool is contained

This is the only place in the server that runs a subprocess, so the containment
is worth stating plainly:

- **Its own switch.** `NOMAD_MCP_ENABLE_HCDIAG` is independent of
  `NOMAD_MCP_READ_ONLY`, because that gate is about the cluster and this is
  about the host.
- **The binary comes from configuration, never from a tool argument.** A model
  that could choose the executable could run anything the server can.
- **No shell.** The command is an argv slice handed to `exec.Command`. There is
  no string for an argument to break out of, so nothing a caller supplies can
  become a second command.
- **Credentials go through the environment, not the command line**, so they are
  not visible in `ps` to every other user on the machine. The child gets a
  deliberately small environment — `PATH`, `HOME` and the `NOMAD_*` variables —
  rather than inheriting whatever this process was started with.
- **The output is bounded.** hcdiag's own stdout and stderr are truncated and
  passed through the same redactor as everything else, so a token echoed by a
  failing runner does not reach the context.
- **The run is bounded.** A collection that overruns
  `NOMAD_MCP_HCDIAG_TIMEOUT` has its process killed and is reported as a
  timeout.

### Why it counts as a read tool

`collect_hcdiag` is annotated read-only and therefore works in read-only mode,
which is worth explaining because it does write a file.

The annotation means *makes no changes to the cluster*, which is the sense this
server's gate enforces, and hcdiag changes nothing in Nomad. Annotating it
mutating would block it in read-only mode — and that is worse than it sounds: an
operator who wanted diagnostics would have to enable writes to get them,
unlocking `purge_node` and `delete_namespace` in order to collect a support
bundle. A narrow, explicit switch is the safer trade.

---

## When it does not work

| Symptom | Cause |
|---|---|
| "collect_hcdiag is disabled on this server" | `NOMAD_MCP_ENABLE_HCDIAG` is not set |
| "the hcdiag binary … was not found" | hcdiag is not installed on *this* host, or not on `PATH` |
| "no new bundle appeared" | hcdiag ran but produced nothing — usually a missing `nomad` CLI, or a Nomad it could not reach. Its own output is included and says which |
| `"partial": true` | Some runners failed; the bundle is still there and still useful |
| "did not finish within 10m" | Narrow `since`, or raise `NOMAD_MCP_HCDIAG_TIMEOUT` |
| "Refused: … is outside …" | `NOMAD_MCP_HCDIAG_DEST` confines where bundles may be written |
