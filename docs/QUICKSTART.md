# Quickstart

Ten minutes from nothing to a working setup: a throwaway Nomad on your laptop,
the server wired into your MCP client, and five things to ask it.

Nothing here touches a real cluster. If you would rather point it at one, skip
to [Pointing it at a real cluster](#pointing-it-at-a-real-cluster) at the end —
but read the one paragraph there first.

**You need:** Go 1.26+, and the `nomad` binary.

---

## 1. Check your Nomad is Community Edition (30 seconds)

```bash
nomad version
```

If that says **`+ent`** — e.g. `Nomad v1.9.7+ent` — stop and fix it first:

```bash
brew uninstall nomad
brew install nomad
```

An Enterprise binary refuses `nomad agent -dev` with `invalid license config:
empty license`, which does not obviously point at the licence and will waste
your afternoon. This is the single most common way this goes wrong.

---

## 2. Build (1 minute)

```bash
git clone https://github.com/suyash1603/nomad-mcp-server.git
cd nomad-mcp-server
make build
```

Sanity check, no cluster needed:

```bash
make test
```

All green? Good — the whole thing works without you configuring anything.

---

## 3. Start a throwaway Nomad (1 minute)

**In its own terminal, and leave it running:**

```bash
nomad agent -dev -bind 127.0.0.1 -config scripts/dev-agent.hcl
```

> The `-config` flag is not optional on Apple Silicon. Nomad mis-fingerprints
> CPU frequency there, leaving the node with ~40 MHz of allocatable compute, and
> every example job fails with `Dimension "cpu" exhausted` — which looks like a
> broken jobspec rather than a fingerprinting bug.

Back in your first terminal, load three deliberately different jobs:

```bash
export NOMAD_ADDR=http://127.0.0.1:4646
nomad job run examples/hello-service.nomad.hcl    # healthy
nomad job run examples/unplaceable.nomad.hcl      # never places, on purpose
nomad job run examples/batch-report.nomad.hcl     # one group fails, on purpose
```

Those three are the healthy case, the *scheduler could not place this* case, and
the *task ran and died* case. Nomad keeps those last two in completely different
places, which is most of what makes Nomad debugging annoying — and most of what
this server is for.

---

## 4. Wire it into your client (1 minute)

### Claude Code

```bash
claude mcp add nomad \
  -e NOMAD_ADDR=http://127.0.0.1:4646 \
  -- "$PWD/bin/nomad-mcp-server" stdio

claude mcp list          # should show nomad, connected
```

### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` and
restart the app:

```json
{
  "mcpServers": {
    "nomad": {
      "command": "/absolute/path/to/nomad-mcp-server/bin/nomad-mcp-server",
      "args": ["stdio"],
      "env": { "NOMAD_ADDR": "http://127.0.0.1:4646" }
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
      "command": "/absolute/path/to/nomad-mcp-server/bin/nomad-mcp-server",
      "args": ["stdio"],
      "env": { "NOMAD_ADDR": "http://127.0.0.1:4646" }
    }
  }
}
```

### VS Code

`.vscode/mcp.json`. Note the key is `servers`, not `mcpServers`, and each entry
names its `type`:

```json
{
  "servers": {
    "nomad": {
      "type": "stdio",
      "command": "/absolute/path/to/nomad-mcp-server/bin/nomad-mcp-server",
      "args": ["stdio"],
      "env": { "NOMAD_ADDR": "http://127.0.0.1:4646" }
    }
  }
}
```

---

## 5. Five things to ask (5 minutes)

Ask these in order. The interesting one is the second.

**1. "What's running on my Nomad cluster?"**

Should give you a real summary — leader, node count, all three jobs — and notice
that `unplaceable` is not healthy.

**2. "Why is the unplaceable job not placing?"** ← *the one that matters*

Should go to the **evaluations**, not the logs, and come back naming the exact
constraint:

> task group "impossible": no nodes were eligible for evaluation at all, which
> usually means the job's datacenters or node_pool match nothing in this cluster

That job has zero allocations, so it has no logs. A model that goes hunting for
logs finds nothing and concludes the job is fine. Getting this right is the
whole design — **if it goes to the logs instead, that is the most valuable thing
you could report back.**

**3. "Show me the stderr of the failing batch-report allocation"**

Should find the failed allocation in the `flaky` group and quote the actual
error out of its stderr.

**4. "Stop the hello-service job"**

Should **refuse**, explain that the server is read-only by default, and tell you
which environment variable would change that. Then check it really did nothing:

```bash
nomad job status hello-service | head -3
```

**5. Try a prompt or a resource.**

Run `troubleshoot_failing_job` with `job_id=unplaceable`, or attach
`nomad://jobs/default/hello-service` as a resource. How you reach them depends
on the client:

| Client | Prompts | Resources |
|---|---|---|
| Claude Code | `/nomad:troubleshoot_failing_job` | `@nomad://jobs/default/hello-service` |
| Claude Desktop | the `+` menu | the paperclip menu |
| Cursor | the prompt picker | attach from the MCP panel |
| VS Code | `/mcp.nomad.troubleshoot_failing_job` | the `#` picker |

---

## 6. Tear down

```bash
nomad job stop -purge hello-service
nomad job stop -purge unplaceable
nomad job stop -purge batch-report
claude mcp remove nomad
# Ctrl-C the dev agent — it keeps no state outside its temp directory
```

---

## If something looks wrong

Open an issue: **https://github.com/suyash1603/nomad-mcp-server/issues**

Most useful, roughly in order:

1. **A question it answered badly**, with the question and what it did. Tool
   descriptions are written for a model to act on; if it picks the wrong tool,
   the description is wrong.
2. **A tool that returned something confusing** — too much output, too little,
   or a field whose meaning is not obvious.
3. **An error message that did not tell you what to do next.** Every error is
   supposed to name the fix.
4. **Anything that failed on your machine** but not on the instructions here.

Please do not paste real cluster addresses, tokens or job specs into an issue.

---

## Pointing it at a real cluster

You can, and it is designed for it — but read this first.

**The token is the control.** This server can do exactly what the token you give
it can do, and no less. It can read job specs, task logs and allocation files,
all of which routinely contain things you would not paste into a chat window.
Give it a **read-only ACL policy scoped to one namespace**, not your management
token.

The defaults are on your side: writes are refused (`NOMAD_MCP_READ_ONLY=true`),
Variable values are not returned (`NOMAD_MCP_ALLOW_VARIABLE_READS=false`), the
ACL tools are not offered at all (`NOMAD_MCP_ENABLE_ACL=false`), and you can pin
it to specific namespaces with `NOMAD_MCP_ALLOWED_NAMESPACES`.

```bash
claude mcp add nomad-staging \
  -e NOMAD_ADDR=https://nomad.staging.example.com:4646 \
  -e NOMAD_TOKEN="$STAGING_READONLY_TOKEN" \
  -e NOMAD_MCP_ALLOWED_NAMESPACES=staging \
  -- /path/to/nomad-mcp-server stdio
```

[docs/SECURITY.md](SECURITY.md) has the full threat model, including what
happens if a workload writes something hostile into its own logs.

For anything more thorough than this page, see [docs/TESTING.md](TESTING.md).
