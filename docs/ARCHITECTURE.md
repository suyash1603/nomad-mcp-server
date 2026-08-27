# Architecture

A guided tour of the codebase, layer by layer: what each piece does and why it
is the way it is. Read it before adding a tool — it will save you reading the
source cold.

---

## Background: what an MCP server actually is

Worth ten lines, because everything below assumes it.

The Model Context Protocol is JSON-RPC 2.0 between an **MCP client** (Claude
Code, Claude Desktop, Cursor, the MCP Inspector) and an **MCP server** (this
program). The client asks the server what it can do, then calls into it on the
model's behalf.

A server exposes three kinds of thing:

- **Tools** — functions the model can call, each with a JSON Schema for its
  parameters. This is 95% of what this project is.
- **Resources** — readable documents addressed by URI (`nomad://jobs/default/web`).
  The client decides what to pull into context; the model does not "call" them.
- **Prompts** — named, parameterised prompt templates the *user* picks from a
  menu, typically as slash commands.

A session opens with `initialize`, where both sides exchange capabilities and
agree a protocol version. After that the client sends `tools/list`,
`tools/call`, `resources/read`, and so on.

Two transports carry those messages, and this server speaks both:

- **stdio** — the client launches the server as a subprocess and talks over its
  stdin/stdout. The consequence that bites people: **stdout is the protocol
  channel.** One stray `fmt.Println` corrupts the JSON-RPC stream and the client
  drops the connection. Every log line in this codebase goes to stderr or a file.
- **streamable-HTTP** — the server is long-running and the client POSTs to an
  endpoint (`/mcp`), with responses returned directly or streamed back as SSE.

`github.com/mark3labs/mcp-go` implements the protocol machinery. We supply
tool definitions and handlers; it does framing, dispatch, and schema.

---

## The shape of the repository

```
cmd/nomad-mcp-server/   the CLI: commands, flags, server bootstrap
pkg/config/             every configuration knob, in one table
pkg/tools/              the tool catalog, one package per domain, grouped into
                        toolsets in toolsets.go
pkg/utils/              shared helpers
version/                version string, injected at build time
docs/                   this file and its siblings
```

This mirrors `hashicorp/vault-mcp-server` deliberately — same layout, same
libraries, same flag names — so a HashiCorp maintainer reading it recognises
everything. `pkg/config/` is the one addition; upstream reads `os.Getenv` inline
at each use site, which does not satisfy the rule this project holds to: a flag
beats an environment variable, for every setting.

---

## `version/`

**`version/VERSION`** holds `1.0.0-dev`, and nothing else. It is a plain file so
that a release process can bump the version without touching Go source.

**`version/version.go`** embeds that file with `//go:embed` and splits it on the
first `-` into `Version` (`1.0.0`) and `VersionPrerelease` (`dev`). A trailing
prerelease marker means "not a final release".

`GitCommit` and `BuildDate` are declared empty and filled in **by the linker** —
see `LDFLAGS` in the `Makefile`. That is why `--version` can report the exact
commit a binary came from. `BuildDate` is the HEAD commit's date rather than the
wall clock, so building the same commit twice produces the same output.

---

## `pkg/config/config.go` — one table, everything derived

This is the most opinionated file in the skeleton, so it gets the most space.

Every setting is reachable three ways — a CLI flag, an
environment variable, and a default — with **flag beating env beating default**.
Written by hand that is four things to keep in sync per setting (flag
declaration, viper binding, env binding, default) across ~25 settings. They
would drift.

Instead there is one table:

```go
type setting struct {
    key   string   // the flag name, and the viper key
    env   string   // the environment variable
    kind  kind     // string, bool, or int
    def   any      // the default
    scope scope    // root command, or the HTTP commands
    usage string   // help text
}

var settings = []setting{
    {"read-only", EnvReadOnly, kindBool, DefaultReadOnly, scopeRoot,
        "Refuse every mutating tool. ..."},
    ...
}
```

`RegisterFlags` walks it once and does all four jobs. Adding a knob is one row.

### How the precedence actually works

viper resolves a key in a fixed order: explicit `Set` → flag → env → config file
→ default. The subtlety is what "flag" means: `BindPFlag` consults the flag's
`Changed()` field, so an *unset* flag does not shadow the environment. Binding
both `BindPFlag` and `BindEnv` on the same key therefore gives exactly the
required precedence for free, with no manual comparison.

`SetDefault` is also called, rather than relying on the flag's own default. The
flag default is only consulted at the very bottom of viper's chain, and being
explicit keeps `Load()` correct even for a key whose flag was never registered.

### `--nomad-token` does not exist

`NOMAD_TOKEN` is the one setting with no flag. A secret passed as a command-line
argument is world-readable through `ps` and lands in shell history. The `nomad`
CLI makes the same call. It is bound to viper with a bare `BindEnv`, and there is
a test (`TestNoTokenFlag`) asserting the flag is *rejected*, so nobody adds it
back for convenience later.

### Lists are strings, not `StringSlice`

`--allowed-namespaces` and `--mcp-allowed-origins` are declared as plain strings
and split by `splitList`. Using pflag's `StringSlice` looks more natural and is a
trap: viper's `GetStringSlice` does not split an *environment variable* on
commas, so `NOMAD_MCP_ALLOWED_NAMESPACES=prod,staging` would yield a
single-element slice containing `"prod,staging"` — an allowlist matching a
namespace that cannot exist, silently denying both. Splitting by hand keeps the
flag and the env var behaving identically. `TestAllowedNamespacesFromEnvSplitsOnComma`
pins this down.

### `Validate()` runs at startup

Everything checkable is checked once, before a port is bound or a byte is
written to stdout: CORS mode, transport mode, log level, positive
`max-log-bytes`, well-formed `rps:burst` rate limits, and cert/key pairs that
must be set together. Error strings name the **environment variable**, not the
internal key, because that is what the operator has to go and fix:

```
Error: invalid MCP_CORS_MODE "loose": must be strict, development, or disabled
```

The alternative — discovering a bad value on the model's first tool call — turns
a config typo into a debugging session.

### `httpModeImpliedByEnv`

Parity with `vault-mcp-server`: setting `TRANSPORT_HOST`, `TRANSPORT_PORT`, or
`MCP_ENDPOINT` selects HTTP mode even when `TRANSPORT_MODE` itself is unset.
Without it, `TRANSPORT_PORT=9000 nomad-mcp-server` would serve stdio and quietly
ignore the port.

Upstream implements this by checking `os.Getenv` in `main()` *before* cobra runs,
which means flags cannot influence the decision. Here it happens inside `Load()`,
after normal precedence has been applied, so `--transport-mode` still wins.

---

## `cmd/nomad-mcp-server/main.go` — commands and bootstrap

Three commands, mirroring upstream exactly:

| Command | Purpose |
|---|---|
| `stdio` | the default; JSON-RPC over stdin/stdout |
| `streamable-http` | HTTP transport |
| `http` | deprecated alias for `streamable-http` |

The command was originally specified as `http`. Upstream actually named it
`streamable-http` and kept `http` working as a deprecated alias, so that is what
this does — parity beats the spec's shorthand, and nobody's config breaks.

Running the binary with **no** subcommand dispatches on the resolved transport
mode, so `TRANSPORT_MODE=http nomad-mcp-server` works with no arguments. That
matters because MCP client config blocks often cannot pass arguments.

**`setup()`** is the shared preamble: load config, build the logger, return both.
Every command starts with it, so a bad configuration produces one clear error
before anything binds a port.

**`NewServer()`** builds the `*server.MCPServer` shared by both transports and
declares its capabilities — tools, resources, prompts. `server.WithRecovery()` is
on: a panic inside one tool handler becomes an error on that one call instead of
taking down the process and, with it, the user's whole session.

**`runStdioServer()`** wires the MCP server to stdin/stdout and blocks until
SIGINT/SIGTERM. Its startup banner goes to **stderr** — see the note about stdout
above.

**`runHTTPServer()`** serves the MCP endpoint plus `/health`, and refuses to
start on a non-loopback address without TLS:

```
Error: TLS is required when binding to a non-localhost address (0.0.0.0).
Set MCP_TLS_CERT_FILE and MCP_TLS_KEY_FILE
```

An MCP server holding a Nomad token should not be reachable off-box in
plaintext, and failing closed at startup is better than a warning nobody reads.
Timeouts are set explicitly; `IdleTimeout` is deliberately long (60 minutes)
because MCP clients hold connections open between calls.

**`logStartup()`** records the effective configuration on every start, and is
written so that it *cannot* log the token — it emits `nomad_token_set=true`, a
boolean, never the value. It also warns loudly when a safety property has been
turned off (`read-only` off, variable reads on, TLS verification disabled), so
those choices show up in the log of any session where they were made.

---

## `cmd/nomad-mcp-server/init.go` — flags and logging

`init()` registers the subcommands and makes the single `config.RegisterFlags`
call that declares every flag.

`initLogger()` defaults the sink to **stderr** and switches to a file when
`--log-file` is set. The log file is opened `0600`: it will contain job specs,
error bodies, and cluster detail, so it should not be world-readable. Log level
is parsed here, and an invalid level is an error rather than a silent fallback.

---

## `pkg/utils/logging.go` — the slog bridge

`mcp-go` v0.58 wants a `*slog.Logger`; the rest of this codebase uses logrus.
Rather than run two independent log streams — a real hazard in stdio mode, where
one of them finding stdout corrupts the protocol — `SlogFromLogrus` implements
`slog.Handler` and forwards records into the same logrus logger. One logger, one
sink, one level.

---

## `pkg/tools/toolsets.go` and `tools.go` — the catalog

`toolsetDefs` in `toolsets.go` is the single source of truth for what the server
exposes: an ordered list of named groups, each with a `build` function returning
its tools. `Catalog` flattens it, and `CatalogFor` applies the two filters.

The tools sit behind a function rather than in a field so that `ToolsetNames`
can read the names without constructing anything — validation needs the names at
startup, and building the catalog needs a live `Provider`.

Deriving the catalog from the groups, rather than keeping a group list beside a
flat catalog, is what makes *every tool belongs to exactly one toolset* true by
construction. There is nowhere else a tool could be registered from, and
`TestEveryToolBelongsToExactlyOneToolset` asserts the two views agree on the
count.

`CatalogFor(p, includeEnterprise, toolsets)` applies two independent filters:

- **Toolsets** — the operator's choice about what this server offers at all. Nil
  or empty means every one of them, so the default behaves exactly as it did
  before the setting existed.
- **Edition** — drops the Enterprise-only tools against a cluster known to be
  Community Edition, so the model is not offered tools that can only fail.

An unknown toolset name is rejected in `setup()` rather than in
`config.Validate`, because `pkg/config` cannot import `pkg/tools` — tools
already depends on config through the client. The flag's help text therefore
spells the names out by hand in `config.ToolsetsFlagUsage`, and
`TestToolsetFlagUsageListsEveryToolset` over in `pkg/tools` is what stops the
two drifting apart.

Two further decisions are encoded in `InitTools`, because they shape every
domain package:

- **One subpackage per domain, one file per tool**, matching what
  `vault-mcp-server` actually does. One file per *domain* is the obvious split,
  but jobs alone is 17 tools, which would be a ~1500-line file.
- **Mutating tools are registered even in read-only mode**, and refused at call
  time. Hiding them would make `tools/list` lie about the server's shape, and the
  model would get "no such tool" — indistinguishable from a bug — instead of an
  explanation telling the operator which flag to flip.

  Toolsets are the deliberate exception. A tool excluded by a toolset really is
  absent, because there the operator's intent is "this server does not do that
  at all" rather than "not right now".

---

## `Makefile`

`make build` cross-compiles for the host with `CGO_ENABLED=0` for a static
binary, injecting `GitCommit` and `BuildDate` via `-ldflags`. Architecture is
derived from `uname` rather than `go env`, so the docker targets work on a
machine with no Go installed — the same trick upstream uses.

Targets worth knowing: `make check` (fmt + vet + test) is what CI will run,
`make inspector` builds and launches the MCP Inspector in one step, and
`make test-e2e` runs the end-to-end suite against a real `nomad agent -dev`.

---

## `Dockerfile`

Multi-stage, defaulting to a `scratch` image containing the static binary and
nothing else — no shell, no package manager. CA certificates are copied from an
alpine stage so the binary can verify TLS against a real cluster.

`ENV NOMAD_ADDR="http://host.docker.internal:4646"` is a small kindness: inside a
container, `127.0.0.1` is the container, so the obvious default would fail to
reach a Nomad agent running on the host.

`ENTRYPOINT` is the binary with `CMD ["stdio"]`, which is the shape
`docker run -i --rm <img>` needs when an MCP client launches the image directly.
The `release` stage takes a CI-built binary from `dist/` instead of compiling
in-image.

---

---

# The client layer

Everything between a tool handler and the Nomad API: how the client is built,
who is allowed to call what, and how failures are explained.

## `pkg/client/client.go` — the Provider

`Provider` is what every tool handler will be given. It hands out Nomad API
clients and carries the config, logger and redactor.

**Building a client.** `buildClient` starts from `api.DefaultConfig()` and then
overrides. That is deliberate: `api.DefaultConfig()` already understands all ten
`NOMAD_*` variables, TLS ones included, so starting there means we inherit the
`nomad` CLI's exact behaviour rather than reimplementing it. Overriding
afterwards from the resolved `Config` is what keeps a flag beating the
environment.

One subtlety worth recording, because getting it wrong silently disables TLS
verification: `api.NewClient` only wires up the CA file, client certificate and
SNI name **if `HttpClient` is nil** — it calls `api.ConfigureTLS` itself in that
branch. Supplying our own `HttpClient`, which is what `vault-mcp-server` does for
Vault, would mean `TLSConfig` is ignored entirely. So we assign `TLSConfig` and
leave `HttpClient` alone.

**One client per session.** `FromContext` looks up the MCP session and returns
its client. Each cache entry holds the client *and* a SHA-256 of the token it was
built with, and both must match to reuse it. A session ID is not a credential; if
it were treated as one, a leaked or guessed ID would inherit someone else's
token. When the token changes mid-session the client is rebuilt rather than
reused, because the cached one still carries the old credential.

Only the hash is stored, never the token, so a heap dump of the cache yields
nothing usable.

**`ResolveNamespace`** decides which namespace a call targets — tool argument,
then request header, then server default — and then enforces the allowlist. The
check happens *here*, before the API call, so a disallowed namespace never
reaches Nomad and never lands in its audit log.

It also closes a bypass that is easy to miss: Nomad's list endpoints accept `*`
to mean "every namespace", which would defeat an allowlist entirely. With an
allowlist configured, `*` is refused.

The refusal message says which knob is responsible:

> namespace "secret-ns" is not permitted by this server's configuration. Allowed
> namespaces: [prod, staging]. This is enforced by NOMAD_MCP_ALLOWED_NAMESPACES,
> not by your Nomad token

Without that last clause, a user spends their time re-checking ACL policies for a
restriction that has nothing to do with Nomad.

## `pkg/client/readonly.go` — the gate

The project's headline safety property, implemented as **one piece of
middleware** wrapping every tool handler rather than a check at the top of each
mutating handler. Two reasons: a new write tool cannot forget to include it, and
the behaviour can be tested once against the gate instead of once per tool.

**Classification is derived, not listed.** `Classify` reads the MCP read-only
annotation the tool already carries. There is no hand-maintained list of
mutating tool names to drift out of sync with reality.

It **fails closed**. A tool counts as read-only only if it says so explicitly;
anything unannotated, and any name the gate has never seen, is treated as
mutating. So forgetting an annotation makes a read tool stop working — loud and
immediately obvious — rather than making a write tool quietly callable.

The refusal message is written for a model, and its most important line is the
instruction *not* to retry:

> This is the default and is not something you can change from here — no other
> tool will work around it, so do not retry.

Without that, a model reads "refused" as "try harder" and goes hunting for
another route to the same effect. It then names both ways to lift the
restriction, and points at the read-only tools that still work.

## `pkg/client/ratelimit.go`

Two limits, doing different jobs. The **global** limit protects the Nomad cluster
from this server as a whole. The **per-session** limit stops one runaway client —
usually a model in a retry loop — from consuming the whole global budget and
starving every other session.

Throttling is returned as a tool *result*, not a Go error, so the model sees the
explanation and can act on it.

## `pkg/client/middleware.go` — the HTTP stack

Wrapped so that logging sees everything, then headers are lifted into the
context, then the origin is validated before anything reaches MCP.

**Origin validation is not decoration.** An MCP server on localhost holding a
Nomad token is reachable by any page the user visits. Without origin checks, a
malicious site could drive this server from the browser — the DNS-rebinding class
of attack. Strict mode is the default and rejects every cross-origin request.

A request with **no** `Origin` header is allowed: native MCP clients are not
browsers and never send one, so rejecting them would break every non-browser
client while stopping no attack.

`NomadContextMiddleware` reads the token, namespace and region from **headers
only**, and actively **rejects** requests that put them in the query string:

- A token in a URL leaks into proxy logs, browser history and `Referer` headers.
- An address in a URL turns this server into an SSRF gadget that will happily
  send its Nomad token to a host of the caller's choosing.

Both get a 400 rather than being ignored, so a client doing it finds out
immediately. The rejection never echoes the offending value back.

## `pkg/tools/investigate/` — the investigation tools

The other tool packages are organised by which Nomad endpoint they call. This
one is organised by what the tools are *for*, because none of them maps to a
single endpoint: `find_problems` reads allocations, evaluations, deployments,
jobs and nodes; `build_job_timeline` merges four object types; `search_job_logs`
reads one job's allocations and then a log stream per task per allocation.

The distinction is the reason the package exists. An inspection tool maps one
endpoint to one result and leaves the joining to the caller, which is fine on a
small cluster and stops working on a large one — not because the calls get
slower, but because the model runs out of context before it finishes joining.

Three rules hold across all three tools:

- **Everything fans out under `utils.FanOut`**, so concurrency, target count and
  wall-clock time are bounded. `find_problems` fans out over its *checks* rather
  than over targets, which is what lets one check failing — a token without
  `node:read`, usually — cost only that check's findings.
- **Rank, never enumerate.** Results are capped and report the total they were
  drawn from, so `count` is trustworthy even when `examples` is trimmed.
- **Say what was not covered.** A sampled scan reported as exhaustive is the
  failure these tools exist to avoid.

That last rule is not decoration. Three specific claims are guarded by tests
because a model will otherwise make them:

- A log search that matched nothing is **not** proof the event never happened.
  Logs rotate and rescheduled allocations take theirs with them.
- A `since`/`until` window on a log search applies only to lines the workload
  timestamped itself. When none were parseable the filter had *no effect*, and
  the result says so rather than implying a time range it never enforced.
- A scan whose checks failed is **unknown**, not healthy.

### Reusing the projection layer

`find_problems` renders a placement failure through `projection.Evaluation`
rather than reading the metric itself. That is not merely tidiness. Nomad
reports a failure as a set of counters, and the most common real case —
a constraint that matches no node at all — arrives with *every counter empty*
and `NodesEvaluated` at zero. An early version of this scan read
`ConstraintFiltered` directly and reported "1 evaluation recorded placement
failures" with no reason attached, which is the least useful possible answer.
The projection layer already turns that shape into a sentence; the e2e suite
submits `examples/unplaceable.nomad.hcl` specifically to keep it doing so.

---

## `pkg/utils/fanout.go` — bounded fan-out

`FanOut` applies a function to many targets — allocations, nodes — under three
independent limits: a **concurrency cap** so a diagnostic does not become a
denial of service on the cluster it is diagnosing, a **target cap** so a huge
cluster cannot fill the model's context, and a **wall-clock budget** so a call
returns even when forty nodes are unreachable.

Three properties are worth knowing:

- **Output follows input order**, not completion order. Results are written into
  a slice indexed by target. A tool whose output reorders itself between
  identical calls is much harder to reason about, for a person and for a model
  comparing two runs.
- **A failing target does not abort the fan-out.** On a large cluster some
  fraction of nodes is always unreachable, and that must not cost the caller
  every other answer. Errors are counted, deduplicated, ranked by frequency and
  capped — three hundred copies of "connection refused" is how a tool meant to
  save context exhausts it.
- **What it did not reach is reported as prominently as what it found.**
  `Sampled`, `TimedOut` and `Failed` are structured fields *and* a prose `Note`,
  because a model reading a result with items in it tends to answer from the
  items and skip the metadata. A sampled scan reported as exhaustive is the
  failure mode this guards against — "nothing is failing" when sixty
  allocations were never checked.

Its consumers are the three tools in `pkg/tools/investigate/`, which is why the
limits live here rather than in any one of them.

---

## `pkg/utils/redact.go`

Two strategies. **Pattern** redaction catches anything *labelled* as a secret,
which covers values this process has never seen — a token echoed back inside a
Nomad error body. **Literal** redaction catches the specific secrets this process
holds, which covers values that appear with no label at all.

Two deliberate exceptions, both of which would make the tool worse if redacted:

- **Bare UUIDs are never redacted.** Nomad allocation, evaluation, deployment and
  node IDs are all UUIDs. A redactor that scrubbed them would strip exactly the
  identifiers needed to debug anything. `TestKeepsResourceUUIDs` pins this.
- **`AccessorID` is kept.** It identifies an ACL token but cannot authenticate as
  one, and it is often the only clue about which token was used.

The redactor is **idempotent**, which took a bug to get right: redacted text gets
passed through again routinely — scrubbed once for a log line, again on the way
to the model — and the first implementation re-matched its own `[REDACTED]`
marker and appended a bracket each time.

## `pkg/utils/errors.go`

Turns a Nomad error into a sentence a model can act on. The design is driven by
one observed fact:

> **Nomad's 403 body is only ever the string `Permission denied`.**

Verified against Nomad 2.0.5 OSS with ACLs enabled. Nomad never names the missing
capability, so the "your NOMAD_TOKEN lacks capability X in namespace Y" message
is impossible to produce from the error alone. `ErrorContext` is how the tool
supplies what the error cannot: the capability its endpoint documents, the
namespace, the resource kind and name, and the tool to run next.

Other mappings, all from observed behaviour:

| Seen | Becomes |
|---|---|
| 404 `job not found` | `No job named "web" in namespace "prod". Try list_jobs …` |
| 501 `Nomad Enterprise only endpoint` | "this requires Nomad Enterprise, and the cluster … is Community Edition" |
| `connection refused` | "Is the Nomad agent running, and is NOMAD_ADDR correct?" |
| 500 | points at `get_cluster_status`, since no-leader is the usual cause |

`StatusCode` prefers the typed `api.UnexpectedResponseError` but falls back to
parsing `"Unexpected response code: 404 (job not found)"` out of the message,
because some Nomad client paths format the status into a plain error string.

The tests build these errors by driving the **real** Nomad API client against an
`httptest` server, rather than hand-constructing error values whose fields are
unexported — so they exercise the same values the tools will actually see.

## What the client layer does not do

No tools are registered, so `tools/list` still returns `[]` and
`gate.MutatingTools()` is empty. The gate, limiter, provider and error mapping
are all wired into `NewServer` and waiting for the tool layer to give them something to
guard.

---

# The read tools

Thirty-seven tools across seven domain packages. The interesting part is not any
one of them; it is the handful of decisions that repeat in all thirty-seven, so
this section covers those and then points at the places where a domain breaks
the pattern for a reason.

## Why `pkg/tools/<domain>/` and not one file per domain

One file per domain is the obvious split. Seventeen job tools in one
file is about 1,500 lines, and `vault-mcp-server` does not actually do that
either — it uses a package per domain. So: `pkg/tools/jobs/`, `pkg/tools/allocs/`,
`pkg/tools/nodes/`, `pkg/tools/scheduler/`, `pkg/tools/catalog/`,
`pkg/tools/system/`, `pkg/tools/variables/`, with roughly one file per tool
inside each.

`pkg/tools/toolsets.go` is the only file that knows the whole catalog, and
`tools.go` derives from it:

```go
func Toolsets(p *client.Provider) []Toolset
func Catalog(p *client.Provider) []server.ServerTool
func CatalogFor(p *client.Provider, includeEnterprise bool, toolsets []string) []server.ServerTool
func InitTools(s *server.MCPServer, p *client.Provider, gate *client.Gate) []server.ServerTool
```

`Catalog` is exported purely so the tests can walk every tool without starting a
server — it ignores both filters for exactly that reason. That is also what makes the catalog-wide invariants in
`pkg/tools/tools_test.go` possible — every tool annotated, every name
well-formed, every description long enough to be usable.

## The shape every tool has

```go
func ListDeployments(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription("…"),
		utils.ReadOnlyTool(),
		utils.NamespaceParam(),
		utils.RegionParam(),
		utils.PrefixParam("deployments"),
		utils.FilterParam(`Status == "running"`),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool:    mcp.NewTool("list_deployments", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { … },
	}
}
```

Five things happen in every handler, in this order:

1. `p.ResolveNamespace(ctx, req.GetString("namespace", ""))` — applies the
   allowlist. This runs **before** the Nomad client is even fetched, so a
   forbidden namespace never produces a request.
2. `p.FromContext(ctx)` — the per-session client, so an HTTP caller's own
   `X-Nomad-Token` is used rather than the server's.
3. Build `api.QueryOptions`, with `utils.PageFrom(req).Apply(…)` layering
   pagination on top.
4. On error, `utils.MapError` with an `ErrorContext` that names the operation,
   the capability the endpoint needs, and the list tool to fall back to.
5. Project into a small struct and return it via `utils.JSONResult`.

Step 4 is the one worth dwelling on. Nomad's 403 body is *always* the string
`Permission denied` and never names the capability that was missing — this was
checked against a live agent, not assumed. So the tool has to supply that
itself:

```go
Capability: "read-job",
```

Without it the best the server could say is "permission denied", which tells the
user nothing they can fix.

## Projection, not passthrough

`pkg/tools/projection/projection.go` holds the three shapes that show up
everywhere: `AllocStub`, `Eval` and `Deploy`. Nomad's `AllocationListStub` has
about forty fields; `AllocStub` has sixteen, and adds two Nomad does not have —
`short_id`, because that is what the `nomad` CLI shows and what a human will
paste back, and `needs_attention`, a boolean derived from the task states so a
model scanning a list does not have to re-derive it per item.

`projection.Evaluation` does the most work. Nomad reports a placement failure as
`FailedTGAllocs`, a map of task group to `*api.AllocationMetric` full of counters
like `ClassFiltered` and `DimensionExhausted`. Counters are not an explanation,
so `failure()` turns them into one:

> task group "impossible": no nodes were eligible for evaluation at all, which
> usually means the job's datacenters or node_pool match nothing in this cluster.

That sentence is the whole point of the tool. A model reading
`{"NodesEvaluated": 0}` has to know a great deal about Nomad's scheduler to get
anywhere; a model reading that sentence does not.

## Truncation has a direction

`utils.TruncateTail` keeps the **end** of a string and `TruncateHead` keeps the
beginning. Log reads use `TruncateTail`, because a crash message is the last
thing a process writes, and a naive head-truncation would reliably discard the
only line anyone wanted. `NOMAD_MCP_MAX_LOG_BYTES` (64 KiB by default) caps it,
and the result carries `truncated`, `original_bytes` and `returned_bytes` so the
model knows it is looking at a slice.

## Where the domains break the pattern

**`system.GetAgentConfig`** is an allowlist, not a dump. `/v1/agent/self`
includes TLS file paths and the Consul and Vault integration blocks; returning
it wholesale would put all of that into the model's context for no benefit. The
tool names the fields it will return and ignores the rest.

**`jobs.ReadJob`** lists task environment variables **by key only**. A jobspec's
`env` block is a routine place to find a password, and the key alone is enough
to answer "is this configured".

**`jobs.PlanJob` and `jobs.ParseJobHCL`** are annotated read-only, because they
change nothing — but Nomad requires `namespace:submit-job` or
`namespace:plan-job` for plan and `namespace:parse-job` for parse. A token with
only `read-job` gets a 403 from a tool the server has just told it is read-only,
which looks like a bug. `plan.go` carries an `aclNote` const that is appended to
both descriptions and to their error mapping.

**`allocs`** is the one domain that talks to client nodes rather than servers.
`/v1/client/…` calls go to the node the allocation is on, and the Go client
falls back to a server if the node is unreachable — costing up to
`api.ClientConnTimeout` per call. Log reads therefore carry their own 30-second
`logReadTimeout`. Logs and file contents are also explicitly labelled as
untrusted in the output, because they are written by the workload.

**`catalog.ListVolumes` / `ReadVolume`** take a `type` argument that is either
`csi` or `host`. A single volume tool pair looks like the obvious shape, but
Nomad serves CSI volumes and dynamic host volumes from two different APIs with
two different shapes, and
pretending otherwise would mean silently returning only half the volumes.

**`variables.ListVariables`** returns paths and never values — not as a filter,
but because Nomad's list endpoint genuinely does not include them. That is what
makes it safe to leave always-on while `read_variable` stays gated.

## Pagination

Every list tool takes `page_size` and `next_token`, and returns `next_token` when
more results exist. `utils.NextTokenNote` turns that into a sentence in the
`note` field, because a model that ignores a JSON field will usually still read
prose telling it there is more.

---

# The write tools

Seventeen tools, taking the catalog to fifty-four. Every one of them is refused
in the default configuration, and the test suite proves it for each individually.

## The annotation is the mechanism

```go
utils.MutatingTool(destructive, idempotent bool) mcp.ToolOption
```

sets `ReadOnlyHint: false` plus the destructive and idempotent hints. The gate
in `pkg/client/readonly.go` classifies on exactly that annotation:

```go
readOnly := tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint
g.mutating[tool.Name] = !readOnly
```

A tool with no annotation is treated as mutating and refused. Forgetting the
annotation therefore breaks the tool loudly in read-only mode rather than
quietly opening a hole, which is the correct direction for that mistake to fail
in. `IsMutating` returns `true` for a name it has never seen, for the same
reason.

Mutating tools are registered even when the server is read-only. `tools/list`
then describes the server honestly, and a blocked call returns an explanation
rather than "unknown tool", which would look like a bug rather than a policy.

## What the refusal says

`refusalMessage` names the tool, says the server is read-only, gives **both**
the environment variable and the flag that would change it, and says *do not
retry*. That last part matters: without it a model will try the same call two or
three more times, and each attempt costs a turn.

## Choosing the two hints

`destructive` means the call can discard state or interrupt running work.
`idempotent` means repeating it changes nothing further. Clients use both to
decide whether to ask the user before proceeding, so they are chosen per tool
rather than set to a blanket value:

| Tool | destructive | idempotent | Why |
|---|---|---|---|
| `run_job` | true | false | An update rolls the running allocations; each submit is a new version |
| `stop_job` | true | false | Interrupts running work, and each call yields a new evaluation |
| `scale_task_group` | true | true | Can remove allocations, but the same target count is the same end state |
| `set_node_eligibility` | false | true | Pure state flag |
| `drain_node` | true | false | Moves running allocations, and re-issuing restarts the deadline |
| `delete_variable` | true | true | Discards a secret; the second delete leaves the same absence |

A separate test asserts that every tool marked destructive says so in its own
description as well. Four of them did not, when the test was first written —
`scale_task_group`, `revert_job_version`, `stop_allocation` and
`fail_deployment`. The descriptions were fixed, not the test.

## `write_variable` replaces, and says so

Nomad's variable write endpoint replaces the whole item set; it does not merge.
A model that reads a variable, changes one key and writes it back would silently
delete every key it did not include. The tool cannot change the endpoint's
semantics, so it does the next best thing: it reads the existing variable first,
diffs the key sets, and returns any dropped keys in `keys_removed` with a
warning. Silently losing a key from a secret store is the worst failure mode
available to this server, and it should be impossible to do accidentally.

## No ACL tools

Both prior-art Nomad MCP servers expose ACL token creation; one of them can mint
a management token straight into the model's context. The decision here was to
not build them at all, so there is nothing to gate and nothing to get wrong. It
is a deliberate exclusion, not an oversight.

---

# Resources and prompts

Tools are what the model decides to call. Resources and prompts are what the
*user* reaches for: a resource is the paperclip menu or an `@`-mention, a prompt
is a slash command. Same cluster, different door.

## `pkg/resources` — one renderer, two front doors

The obvious way to build this is to write a second set of projections that turn
a Nomad job into resource contents. That is a mistake, and it is a slow one: the
two sets drift within a release or two, and a user who attaches a job then gets
a subtly different view from the one the model gets when it calls `read_job` on
the same job.

So nothing here projects Nomad. Each resource delegates to the tool that already
knows how to render that object and returns its JSON verbatim:

```go
func New(p *client.Provider, catalog []server.ServerTool) *Registrar
```

The catalog is passed in rather than rebuilt, which is why `InitTools` now
returns it. `TestResourceAndToolAgreeExactly` reads `nomad://jobs/default/web`
and calls `read_job` on the same job, and asserts the two are byte-identical.

Delegating also means resources inherit the namespace allowlist, the redaction
and the error mapping for free, rather than needing them re-applied.
`TestNamespaceAllowlistCoversResources` pins that down.

### What is registered

| URI | Backed by |
|---|---|
| `nomad://cluster` | `get_cluster_status` |
| `nomad://jobs` | `list_jobs` |
| `nomad://jobs/{namespace}/{job_id}` | `read_job` |
| `nomad://allocs/{alloc_id}` | `read_allocation` |
| `nomad://nodes/{node_id}` | `read_node` |

The three templates are the core of it. The two concrete resources on top are
there for a practical reason: several MCP clients never call
`resources/templates/list`, so a server with nothing but templates shows an
empty attachment menu and looks broken. The indexes give those clients somewhere
to start, and their descriptions name the URI shapes.

### Everything here is read-only, structurally

A resource has no destructive-hint annotation and no confirmation flow in any
client — attaching one is a click. So a mutating resource would be a cluster
change a user could trigger from an autocomplete list. There will not be one,
and `TestNoResourceCanChangeTheCluster` enforces it against a ledger that
`Register` builds as it wires each resource, rather than a list maintained
alongside it.

### The `[]string` trap

This one cost a debugging round. mcp-go matches a resource URI against the
template with `yosida95/uritemplate`, then copies the matched value into
`request.Params.Arguments`:

```go
request.Params.Arguments[name] = value.V
```

`Value.V` is a **`[]string`** — RFC 6570 variables can expand to lists, so even a
plain `{job_id}` arrives as a one-element slice. The first version of this
package asserted `.(string)`, which silently yielded `""`, and every templated
read failed as "missing segment" while the URI matching underneath was working
perfectly. `templateVar` now handles `string`, `[]string` and `[]any`, and
`TestTemplateVarUnwrapsURITemplateValues` is there so it stays handled.

### Errors become errors

A tool reports a recoverable failure through `IsError` on the result, because
the model is expected to read it and try something else. A resource read has no
such loop — the client asked for one URI and either gets it or does not — so
`call()` turns a tool error into a real Go error. The message is still the
mapped, redacted one, so a bad URI produces:

> No job named "does-not-exist" in namespace "default". Try list_jobs to see
> what exists, or check whether it lives in a different namespace.

rather than a bare 404.

## `pkg/prompts` — procedure, not capability

Two prompts, and they make no API calls at all. That is deliberate:
`explain_cluster_health` is most useful precisely when the cluster is
unreachable, and a prompt that failed to render because Nomad was down would be
useless at the one moment it was needed. The model does the fetching.

Both prompts open with two things the model would otherwise have to discover the
hard way:

- **The server mode.** Told read-only up front, the model produces
  recommendations; left to discover it by being refused, it wastes a turn and
  the user reads the refusal as a malfunction. When writes *are* enabled the
  note says so and still says "diagnose first" — restarting a failing allocation
  destroys the evidence that would have explained it.
- **That Nomad's output is untrusted.** Job metadata, task names and log output
  are written by the workloads. The prompt says to treat all of it as data, and
  to report an injection attempt as a finding rather than acting on it or
  silently ignoring it.

### `troubleshoot_failing_job`

Arguments: `job_id` (required), `namespace`, `symptom`.

The procedure exists because of one branch that is not obvious and that a model
will get wrong left to itself. Nomad keeps *"the scheduler could not place
this"* and *"the task ran and then died"* in completely different objects, and
when placement fails there is no allocation, so there are no logs. A model that
starts at the logs finds nothing and concludes the job is fine.

So step 2 is `read_job_summary`, and it forks:

- allocations exist but are unhealthy → allocations, then logs
- zero allocations, or fewer than asked for → **evaluations**, and explicitly
  *not* logs

Walked against the `unplaceable` example on a live agent, the chain lands on:

> task group "impossible": no nodes were eligible for evaluation at all, which
> usually means the job's datacenters or node_pool match nothing in this cluster.

Both `read_job_summary` and `list_job_allocations` also point at
`list_job_evaluations` in their own `note` fields, so a model that ignores the
prompt still gets nudged onto the same path.

### `explain_cluster_health`

One optional `namespace`, which scopes only the job-level checks; server and
node health are cluster-wide.

The ordering rule here is that the first two checks make the rest meaningless if
they are wrong. No leader means nothing can be scheduled and the rest is noise,
so that is checked first and reported alone. Version skew across servers is a
half-finished upgrade and is worth flagging even when nothing looks broken.

It also tells the model how to read a *failure* of one of these calls: a 403 is
a token-scope problem and not a cluster problem, and an Enterprise-only endpoint
is not a finding at all. Both are things this server will genuinely return
against a Community Edition cluster.

### Wiring

```go
catalog := tools.InitTools(s, provider, gate)
resources.New(provider, catalog).Register(s)
prompts.New(provider).Register(s)
```

Three lines in `NewServer`, after the tools and before the read-only log line.

---

# The tests

Three layers, three `make` targets, and one rule shared by all of them: a test
that can pass while the thing it names is broken is worse than no test, because
it also stops anyone from looking.

    make test        unit — a fake Nomad, no cluster needed
    make test-e2e    the built binary against a real `nomad agent -dev`
    make test-http   the same, over the streamable-HTTP transport

## `internal/nomadtest` — a fake Nomad worth trusting

The tools are thin: call the Go client, project the result, map the error.
Almost every bug they can have is in the projection or the mapping, and both
are exercised by feeding the *real* `api.Client` canned responses. So
`internal/nomadtest` stands up an `httptest` server speaking enough of Nomad's
wire protocol that the genuine client cannot tell the difference.

Two decisions make it useful rather than merely present.

**The fixture is built from the real `api` structs.** Not JSON literals:

```go
s.JSON("/v1/jobs", []*api.JobListStub{{
    ID: HealthyJob, Type: "service", Status: "running", …
}})
```

A map literal encodes Nomad's field names by hand, and Nomad's are not always
the obvious ones — `DimensionExhausted`, `ConstraintFiltered`,
`ClientDescription`. A typo in a literal produces a zero value and a test that
passes while asserting nothing. Marshalling the same structs the client will
unmarshal removes that possibility entirely.

**It records every request.** A tool that quietly drops the namespace, forgets
to forward a filter, or never sends the pagination token still returns
plausible-looking output. So the assertions that matter most are about what went
*out*:

```go
h.ok("list_jobs", map[string]any{"namespace": "production"})
require.Equal(t, "production", h.nomad.Last("/v1/jobs").Namespace())
```

The default fixture is a small coherent cluster — one server, one client node, a
healthy job and a stuck one whose blocked evaluation carries a real constraint
failure. Those two jobs exist because every troubleshooting tool in this server
has exactly two halves, and both need a subject.

## What the unit tests actually assert

`pkg/tools/handlers_test.go` is grouped by what could go wrong rather than by
tool:

- **Projections** — the placement failure renders as prose naming the task group;
  a stuck deployment is diagnosed as waiting on a human; a node returns its
  allowlisted attributes and not the other three hundred.
- **Disclosure** — `read_job` returns `DATABASE_PASSWORD` as a key and never its
  value; `list_variables` returns paths and never contents; a 500 that echoes a
  token back comes out `[REDACTED]`.
- **Both gates** — a refused `read_variable` never reaches Nomad at all, and a
  namespace outside the allowlist is refused *before* the request rather than
  after.
- **The wire** — namespace, filter, prefix and both pagination arguments are
  asserted on the outgoing request; the token is asserted to be in
  `X-Nomad-Token` and *not* in the query string.

`pkg/tools/tools_test.go` keeps its old job — catalog-wide invariants, pointed at
a dead address so it can never reach a network. The split is deliberate: those
must not have a Nomad behind them, and these must.

## `e2e/` — the built binary, a real agent

Everything here is the code path a user gets. `TestMain` starts a throwaway
`nomad agent -dev` on three ports nobody is using, builds the binary, and each
test drives it as a subprocess over stdio.

Driving the real binary rather than calling `NewServer` in-process is the point.
Flag parsing, environment handling and stdout discipline are exactly what breaks
on the day someone installs this, and none of them are reachable from a Go
function call.

The suite skips, with an explanation, when `nomad` is missing — and when it is an
**Enterprise** build, which refuses `nomad agent -dev` outright with `invalid
license config: empty license`. That error does not obviously point at the
licence, and it cost an afternoon earlier in this project; detecting it turns a
confusing failure into one sentence.

`TestFullTroubleshootingPathOnRealJobs` submits the two real jobspecs from
`examples/` through the `run_job` tool and then walks the exact chain the
`troubleshoot_failing_job` prompt prescribes, asserting it terminates in an
explanation:

> task group "impossible": no nodes were eligible for evaluation at all, which
> usually means the job's datacenters or node_pool match nothing in this cluster.

It also checks that a resource read and the equivalent tool call return
identical bytes — the same invariant the unit tests check against a fake, now
against a real cluster.

## `e2e/http_test.go` — the layer stdio does not have

CORS, per-request Nomad settings lifted out of headers, session identity, and a
refusal to bind anywhere public without TLS. None of it is reachable from the
stdio tests, and all of it is what someone deploying this on a shared box
depends on. The tests cover a full handshake and tool call over `/mcp`, the
health endpoint (asserting it discloses no credentials), a foreign `Origin`
being rejected, `0.0.0.0` without TLS refusing to start, and the read-only gate
still applying.

## Three bugs, and why they are interesting

The suite earned its keep immediately. All three are in the changelog; the
third is the one worth internalising.

**The rate limiter throttled stdio sessions.** A troubleshooting walk is a dozen
tool calls in a couple of seconds; the per-session default of 5 rps with burst 10
refused on the eleventh. And `MCP_RATE_LIMIT_GLOBAL`/`_SESSION` are scoped to the
HTTP subcommands, so a stdio user hitting the limit had no flag to raise it — the
server was enforcing a limit it only let you configure in a mode you were not in.
Rate limiting now applies to the HTTP transport only.

**The exit code vanished from failed tasks.** `taskState` read `ExitCode` from
the last event. But a task that fails often enough for Nomad to give up ends on
`Not Restarting — Exceeded allowed attempts`, which carries no exit code; the
`Terminated` event holding the real one is behind it. `omitempty` then dropped
the zero, so the output looked clean while having lost the single most useful
fact about a failed task. The fix scans backwards for the most recent
`Terminated` event. The fixture now reproduces that event order, so the unit
suite catches it too.

**`nomad_token` walked straight through the query-string guard.** The guard was
a list of literals checked with `url.Values.Get`, which is case-sensitive. It
caught `NOMAD_TOKEN` and `token`; it did not catch `nomad_token`, `nomadToken`
or `x-nomad-token`. The existing unit test listed only the spellings that
already worked, so it had been passing while the hole was open.

The fix normalises instead of enumerating — fold case, drop `-`, `_` and `.`,
compare against a normalised set — so one entry covers the whole family. There
is a matching test for the other direction too, because a normaliser that starts
matching `next_token` or `namespace` would be its own kind of bug.

The lesson generalises past this one guard: **a security check written as a list
of literals is only as good as the imagination of whoever wrote the list, and a
test that reuses the same list proves nothing.** The only reason this was found
is that the e2e suite was written separately and tried spellings the unit test
had not.

## Coverage

52.1% of statements from the unit tests alone, measured with `-coverpkg` across
the whole module. The per-package figure `go test -cover` prints for the tool
subpackages reads 0.0%, which is an artefact: those packages have no test files
of their own, and their tools are exercised from `pkg/tools`. The real figures
are 28–63% per subpackage, and the e2e suite covers more on top.
