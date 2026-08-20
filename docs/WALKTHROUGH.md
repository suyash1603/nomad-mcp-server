# Walkthrough

A narrated tour of the codebase, file by file: what each piece does and why it
is the way it is. Appended to at every phase, so it stays a tour rather than a
changelog.

Covers the skeleton.

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
pkg/tools/              the tool catalog, one package per domain
pkg/utils/              shared helpers
version/                version string, injected at build time
docs/                   this file and its siblings
```

This mirrors `hashicorp/vault-mcp-server` deliberately — same layout, same
libraries, same flag names — so a HashiCorp maintainer reading it recognises
everything. `pkg/config/` is the one addition; upstream reads `os.Getenv` inline
at each use site, which does not satisfy the "flag beats env, for every setting"
precedence rule.

---

## `version/`

**`version/VERSION`** holds `0.1.0-dev`, and nothing else. It is a plain file so
that a release process can bump the version without touching Go source.

**`version/version.go`** embeds that file with `//go:embed` and splits it on the
first `-` into `Version` (`0.1.0`) and `VersionPrerelease` (`dev`). A trailing
prerelease marker means "not a final release".

`GitCommit` and `BuildDate` are declared empty and filled in **by the linker** —
see `LDFLAGS` in the `Makefile`. That is why `--version` can report the exact
commit a binary came from. `BuildDate` is the HEAD commit's date rather than the
wall clock, so building the same commit twice produces the same output.

---

## `pkg/config/config.go` — one table, everything derived

This is the most opinionated file in the skeleton, so it gets the most space.

The requirement is that every setting is reachable three ways — a CLI flag, an
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

A command called `http` would be the obvious name. Upstream actually named it
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

## `pkg/tools/tools.go` — the catalog

Currently a stub: `InitTools` registers nothing yet. Two decisions are already
encoded in its doc comment, because they shape every later phase:

- **One subpackage per domain, one file per tool**, matching what
  `vault-mcp-server` actually does. One file per *domain* is the obvious split; jobs alone
  is 17 tools, which would be a ~1500-line file.
- **Mutating tools are registered even in read-only mode**, and refused at call
  time. Hiding them would make `tools/list` lie about the server's shape, and the
  model would get "no such tool" — indistinguishable from a bug — instead of an
  explanation telling the operator which flag to flip.

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

## What this layer does not do yet

No Nomad client is constructed, no tools are registered, and the HTTP transport
has no middleware — CORS, rate limiting, request logging and per-session clients
all arrive with the client layer. `tools/list` currently returns `[]`, which is correct: the
skeleton is a working MCP server with an empty catalog.
