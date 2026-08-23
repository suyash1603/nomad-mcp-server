# Connecting to a Nomad cluster

This server talks to Nomad over its HTTP API. That is the only thing it needs:
if `curl $NOMAD_ADDR/v1/status/leader` works from wherever this process runs, so
does the server. Everything below is about making that one thing true.

If something is already wrong, start with the diagnostic rather than this
document:

```
"run check_connection"
```

It reports the address, TLS, token, ACL state, edition and per-capability
permissions, and every failing check comes with the specific fix. Most of what
follows is that tool's advice written out longhand.

---

## The variables that matter

These are the standard Nomad variables. An environment that can already run
`nomad status` can run this server with no extra configuration.

| Variable | What it is |
|---|---|
| `NOMAD_ADDR` | The cluster's HTTP API, e.g. `https://nomad.example.com:4646` |
| `NOMAD_TOKEN` | ACL token. **Environment only** — there is deliberately no flag |
| `NOMAD_NAMESPACE` | Default namespace for namespaced tools |
| `NOMAD_REGION` | Only needed on a federated cluster |
| `NOMAD_CACERT` | CA certificate that signed the cluster's server certificates |
| `NOMAD_CLIENT_CERT` / `NOMAD_CLIENT_KEY` | Client certificate, for mTLS clusters |
| `NOMAD_TLS_SERVER_NAME` | SNI name, when the address does not match the certificate |
| `NOMAD_SKIP_VERIFY` | Disables certificate verification. See the warning below |

> **`NOMAD_TOKEN` has no command-line flag, on purpose.** A token passed as an
> argument is visible to every other process on the machine through `ps`, and
> lands in shell history. The `nomad` CLI makes the same choice. There is a test
> asserting the flag stays absent.

---

## Nomad on the same machine

The default case, and the only one where the default address is right.

```bash
export NOMAD_ADDR=http://127.0.0.1:4646
nomad-mcp-server stdio
```

A `nomad agent -dev` has ACLs disabled, so no token is needed. `check_connection`
will warn about that, which is correct: nothing is restricting what the server
can do except `NOMAD_MCP_READ_ONLY`.

---

## Nomad on EC2, or any remote host

```bash
export NOMAD_ADDR=https://nomad.internal.example.com:4646
export NOMAD_TOKEN=...          # never in the command line
export NOMAD_CACERT=/etc/nomad.d/ca.pem
nomad-mcp-server stdio
```

Three things go wrong here, in this order of frequency.

**The port is not reachable.** Nomad's HTTP API is 4646. On EC2 that means a
security group rule allowing your machine to reach 4646 on the Nomad servers,
and a route to get there — a VPC peering, a VPN, or a bastion. `check_connection`
reports this as a reachability failure; from a shell, `nc -vz host 4646` settles
it in one command.

**The certificate does not verify.** Most Nomad clusters use a private CA, which
the system trust store knows nothing about. Set `NOMAD_CACERT` to the CA file.
If the address you connect on differs from the name in the certificate — common
behind a load balancer — set `NOMAD_TLS_SERVER_NAME` to the name the certificate
actually carries.

**The token is the wrong one.** Nomad tokens have an *AccessorID* and a
*SecretID*. `NOMAD_TOKEN` wants the SecretID. Passing the AccessorID produces a
permission error that looks exactly like an under-privileged token, and people
lose a long time to it.

> Do not reach for `NOMAD_SKIP_VERIFY` to get past a certificate error. The
> token is sent in a header on every request; disabling verification means
> anything on the path can read it and impersonate the cluster. Fix the CA
> instead.

### Through an SSH tunnel

When the cluster is only reachable from a bastion:

```bash
ssh -N -L 4646:nomad.internal:4646 bastion.example.com &
export NOMAD_ADDR=http://127.0.0.1:4646
```

The address is `http` and localhost because the tunnel terminates locally; the
TLS, if any, is between the bastion and Nomad.

---

## Nomad in Docker, and this server in Docker

This is the one that catches everyone. **Inside a container, `localhost` is the
container**, not the host. `NOMAD_ADDR=http://127.0.0.1:4646` in a container
means "Nomad is in this container", which it is not.

**macOS and Windows** — Docker Desktop provides a name for the host:

```bash
docker run -i --rm \
  -e NOMAD_ADDR="http://host.docker.internal:4646" \
  -e NOMAD_TOKEN \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

**Linux** — `host.docker.internal` is not available by default. Either add it:

```bash
docker run -i --rm --add-host=host.docker.internal:host-gateway \
  -e NOMAD_ADDR="http://host.docker.internal:4646" \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

or share the host's network namespace, after which `127.0.0.1` is correct again:

```bash
docker run -i --rm --network host \
  -e NOMAD_ADDR="http://127.0.0.1:4646" \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

**`-i` is required.** Without it the container has no stdin, and the MCP protocol
never starts — the client reports a server that connected and then said nothing.

**Both on the same Docker network** — use the service name:

```bash
docker run -i --rm --network nomad-net \
  -e NOMAD_ADDR="http://nomad:4646" \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

**Mounting a CA certificate** into the container:

```bash
docker run -i --rm \
  -v /etc/nomad.d/ca.pem:/ca.pem:ro \
  -e NOMAD_ADDR="https://nomad.example.com:4646" \
  -e NOMAD_CACERT=/ca.pem \
  -e NOMAD_TOKEN \
  ghcr.io/suyash1603/nomad-mcp-server:latest stdio
```

Note `-e NOMAD_TOKEN` with no value: that forwards the variable from your shell
rather than baking the token into the command line, where it would be visible in
`ps` and in your shell history.

---

## Nomad in Kubernetes

If Nomad is running in the cluster, port-forward and treat it as local:

```bash
kubectl port-forward svc/nomad 4646:4646 &
export NOMAD_ADDR=http://127.0.0.1:4646
```

To run this server *in* Kubernetes, use the HTTP transport rather than stdio,
put the token in a Secret, and never expose the endpoint outside the cluster
without TLS — the server refuses to bind a non-loopback address in plaintext
precisely so this cannot be done by accident.

---

## One server, many users

The HTTP transport lets several clients share one deployment, each with their
own credentials:

```bash
nomad-mcp-server streamable-http \
  --transport-host 0.0.0.0 --transport-port 8080 \
  --mcp-tls-cert-file /certs/tls.crt \
  --mcp-tls-key-file /certs/tls.key
```

Each request may carry `X-Nomad-Token`, `X-Nomad-Namespace` and `X-Nomad-Region`,
so one server can serve callers with different permissions — the Nomad client is
cached per MCP session and keyed on a hash of the token, so two sessions never
share a credential.

Two refusals are deliberate and worth knowing about in advance:

- **A non-loopback bind without TLS fails at startup.** A server holding a Nomad
  token should not be reachable off-box in plaintext, and failing immediately
  beats a warning nobody reads.
- **Credentials in query strings are rejected with a 400**, not ignored. A token
  in a URL ends up in every access log between the client and here.

---

## Which token to give it

The token is the only limit Nomad itself enforces. `NOMAD_MCP_READ_ONLY` is
enforced *in this server* — it stops the server issuing writes, but it does not
narrow what the token could do if something else used it.

A read-only policy scoped to the namespaces you care about:

```hcl
namespace "production" {
  policy       = "read"
  capabilities = ["read-job", "read-logs", "alloc-exec"]
}

node {
  policy = "read"
}
```

Then:

```bash
nomad acl policy apply -description "nomad-mcp-server, read only" \
  mcp-readonly mcp-readonly.hcl
nomad acl token create -name "nomad-mcp-server" -policy mcp-readonly
```

Use the **SecretID** from the output.

`check_connection` probes what the token can actually do and reports each
capability, so you can confirm the policy before anyone relies on it.

Do not use a management token. `check_connection` warns when you have, because a
management token can read every Variable in the cluster and delete every
namespace, and Variables commonly hold secrets.

---

## Community Edition and Enterprise

Both work. The server probes the cluster at startup and only offers the
Enterprise-only tools — quotas, Sentinel policies, licence and Dynamic
Application Sizing recommendations — where they can work. See
[ENTERPRISE.md](ENTERPRISE.md).

---

## Quick reference: symptom to cause

| Symptom | Usual cause |
|---|---|
| `connection refused` | Nomad is not running, or `NOMAD_ADDR` is localhost from inside a container |
| `no such host` | DNS name not resolvable from where this process runs |
| Hangs, then times out | Firewall or security group dropping packets rather than refusing them |
| `x509: certificate signed by unknown authority` | Private CA; set `NOMAD_CACERT` |
| `x509: certificate is valid for X, not Y` | Set `NOMAD_TLS_SERVER_NAME` to the name in the certificate |
| `Permission denied` on everything | AccessorID used instead of SecretID, or the token is revoked |
| `Permission denied` on some tools | The policy is missing that capability; `check_connection` says which |
| `ACL support disabled` | ACLs are off on the cluster. Nothing restricts the server but read-only mode |
| Job "not found" that clearly exists | Wrong namespace, or `NOMAD_MCP_ALLOWED_NAMESPACES` excludes it |
| Enterprise tool returns "requires Nomad Enterprise" | The cluster is Community Edition |
| Client connects, then silence | Missing `-i` on `docker run` |
