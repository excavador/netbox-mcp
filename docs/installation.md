# Installation

Three ways to run it: a binary on your own machine, a container, and the Helm
chart for a cluster. Whichever you choose, you need a NetBox API token first.

## Getting an API token

NetBox accepts exactly one credential — its own API token, sent as
`Authorization: Token …`. Note the scheme: **not** `Bearer`, and never an OIDC
token.

1. Log into NetBox.
2. Your profile → **API Tokens** → **Add a token**.
3. Decide on **Write enabled** deliberately. Leaving it off makes every write
   tool fail with a 403, which is a perfectly reasonable way to run this if you
   only want reads.
4. Give it an expiry, and restrict it to the source IPs that will use it if you
   can.

The token's NetBox permissions are the real limit on what this server can do;
see [the architecture notes](architecture.md#the-netbox-token-is-the-real-limit)
for why. Grant it what the work needs and no more.

## 1. As a local binary

Download a release from the
[Releases page](https://github.com/excavador/netbox-mcp/releases), or build it:

    go build ./cmd/netbox-mcp

Run it over stdio, which is the default and what a local MCP client expects:

    NETBOX_URL=https://netbox.example.com \
    NETBOX_TOKEN=... \
    ./netbox-mcp

There is no output on success — stdio transport means the protocol owns
stdout. Startup logs go to stderr; you should see one line naming the NetBox
version and how many object types were discovered.

Then point a client at it — see [clients](clients.md).

## 2. As a container

    docker run --rm -i \
      -e NETBOX_URL=https://netbox.example.com \
      -e NETBOX_TOKEN=... \
      ghcr.io/excavador/netbox-mcp:0.1.0

Multi-arch (`linux/amd64`, `linux/arm64`), built with ko from a distroless
static base: no shell, no package manager, runs as non-root.

## 3. On Kubernetes, with the Helm chart

    helm install netbox-mcp oci://ghcr.io/excavador/charts/netbox-mcp \
      --version 1.3.0 \
      --set netbox.url=http://netbox \
      --set netbox.existingSecret=netbox-mcp \
      --set auth.issuerUrl=https://access.example \
      --set auth.resourceUrl=https://mcp.example/netbox \
      --set proxy.image.tag=<resource-proxy release> \
      --set proxy.image.digest=sha256:<its image index digest> \
      --set-json 'networkPolicy.ingressFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"gateway"}}}]'

The chart always runs the HTTP transport; stdio in a pod with no attached
client exits immediately.

### The token comes from a Secret, always

There is no value that takes the token as a literal — it would end up in a
values file, in git, and in `helm get values` output.

    kubectl create secret generic netbox-mcp --from-literal=token=...

Or with External Secrets Operator:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: netbox-mcp
spec:
  refreshInterval: 1h
  secretStoreRef: {name: aws-parameter-store, kind: ClusterSecretStore}
  target: {name: netbox-mcp}
  data:
    - secretKey: token
      remoteRef: {key: /netbox/mcp-api-token}
```

### Values

| value | default | notes |
|---|---|---|
| `netbox.url` | — | **Required.** Base URL **without** `/api`; the server appends it. The chart refuses a URL ending in `/api`. |
| `netbox.existingSecret` | — | **Required.** Secret holding the API token. |
| `netbox.existingSecretTokenKey` | `token` | Key within that Secret. |
| `auth.issuerUrl` | — | **Required.** The issuer's own URL, exactly as it appears in a token's `iss`. https, no query or fragment. Feeds the proxy's `ISSUER_URL`. |
| `auth.resourceUrl` | — | **Required.** This server's own EXTERNAL URL — the RFC 8707 audience the proxy requires, and the path (`/netbox` in `https://mcp.example/netbox`) it serves the server at. https, **no trailing slash**, with a path. Must match this resource's id in the issuer's policy, byte for byte. Feeds `RESOURCE_URL`. |
| `auth.scope` | `openid` | Advertised in the PRM's `scopes_supported` and the 401 challenge's `scope`; never checked. Empty omits both fields. |
| `proxy.image.repository` | `ghcr.io/truvity/sluis/resource-proxy` | The sidecar image. |
| `proxy.image.tag` | — | **Required, no default.** The release you have verified, without a leading `v` (`1.84.0`). |
| `proxy.image.digest` | — | **Required, no default.** `sha256:<64 hex>`, the multi-arch image index digest. Tag and digest are both pinned, so a moved tag cannot change what guards the server. |
| `proxy.runAsUser` | `65532` | The numeric uid the proxy image runs as. |
| `proxy.resources` | 10m / 32Mi, limit 96Mi | |
| `networkPolicy.enabled` | `true` | |
| `networkPolicy.ingressFrom` | — | **Required while enabled.** NetworkPolicy peers admitted to the proxy's port, verbatim: one peer can combine a `namespaceSelector` and a `podSelector` (the gateway's pods). Empty would admit nobody, so the chart refuses it. |
| `networkPolicy.egress.dns` / `.https` | `[]` | Peers for DNS and for HTTPS (443 and the issuer's port). Empty means any destination. |
| `networkPolicy.egress.netbox.to` / `.ports` | `[]` | Peers and TCP ports towards NetBox. Empty `ports` means the port of `netbox.url`. A NetworkPolicy sees the pod behind a Service, so for an in-cluster NetBox set the port its **pods** listen on. |
| `networkPolicy.extraEgress` | `[]` | Further egress rules, verbatim. |
| `httpRoute.enabled` | `false` | Render one `HTTPRoute` for the server. |
| `httpRoute.name` | the fullname | |
| `httpRoute.parentRefs` | — | **Required while enabled.** The Gateway or ListenerSet the route attaches to, verbatim. |
| `httpRoute.hostnames` | `[]` | Optional; when set, the host of `auth.resourceUrl` must be one of them. |
| `httpRoute.annotations` / `.labels` | `{}` | |
| `image.registry` / `image.repository` | `ghcr.io` / `excavador/netbox-mcp` | |
| `image.tag` | `""` | Empty means the chart's `appVersion`. Pin it to upgrade deliberately. |
| `replicaCount` | `1` | The server is stateless, so more than one is safe. |
| `service.port` | `8080` | The **proxy's** listen port; the Service targets it and no other port of the pod. |
| `resources` | 20m / 64Mi, limit 128Mi | The server container's. |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, all capabilities dropped | The pod meets Pod Security Standard `restricted`. |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `deploymentAnnotations` | empty | |

The schema sets `additionalProperties: false`, so a typo like `replicaCounts`
fails to render instead of being silently ignored. The pod mounts no
ServiceAccount token (`automountServiceAccountToken: false`).

### Authentication

The **resource-proxy sidecar** validates every request: a bearer token,
checked against `auth.issuerUrl`'s JWKS, with `aud` required to equal
`auth.resourceUrl` exactly (RFC 8707). The server container holds no
authentication code and listens on loopback only. Both values and the proxy's
tag and digest are required — the chart refuses to render without them. See
[design/cimd-auth.md](design/cimd-auth.md) for the design and the exact
issuer policy this expects.

### Routing

With `httpRoute.enabled` the chart ships one `HTTPRoute` with the Service. The
gateway rewrites **nothing** for the resource: the proxy maps `/netbox` to the
server's `/mcp` itself and answers `404` for a path outside it. For
`auth.resourceUrl=https://mcp.example/netbox` the rules, all to the proxy's
port, are:

| match | |
|---|---|
| `PathPrefix /netbox` | the MCP endpoint |
| `Exact /.well-known/oauth-protected-resource/netbox` | the Protected Resource Metadata (RFC 9728) |
| `Exact /netbox/.well-known/oauth-protected-resource` | an alias some clients use, rewritten to the PRM above |

No `timeouts` are set, so the gateway's own default applies. Without
`httpRoute.enabled` routing is the deployment's own concern, and must route
the same three paths without rewriting them.

### Health

Two kinds of probe, deliberately different:

- The **server's** `/healthz` (ops listener, `:9090`) answers liveness, and
  deliberately does **not** call NetBox: a probe that fails when a dependency
  blips takes the pod out of service for something restarting cannot fix.
- The **proxy's** `/readyz` is the pod's readiness: it also needs the issuer's
  keys, so a pod that cannot verify tokens takes no traffic. Its `/healthz` is
  the proxy's liveness.

### Upgrading from 1.2

1.3 moves authentication out of the server and into the sidecar. What changes
for a consumer:

- **New required values:** `proxy.image.tag` and `proxy.image.digest` (no
  defaults — pin the resource-proxy release you have verified), and, while
  `networkPolicy.enabled` (the default), `networkPolicy.ingressFrom`.
- **Kept, now feeding the proxy:** `auth.issuerUrl`, `auth.resourceUrl`,
  `auth.scope`. `auth.resourceUrl` is now also checked for shape: https, a
  path, no trailing slash.
- **The pod's port:** `service.port` names the proxy's port. The server no
  longer listens there.
- **Routing:** the gateway must **stop rewriting** `/netbox` to `/mcp`. Route
  `PathPrefix /netbox`, the PRM path and the suffix alias as above, or let the
  chart own the route (`httpRoute.*`) and drop yours. Chart and route change
  together, so expect a short window during the rollout in which requests
  through the old rewriting route get `404`.
- **Probes:** readiness is the proxy's `/readyz`; the server's `/healthz` moved
  to the ops port.
- **The server's flags:** `--issuer-url`, `--resource-url` and `--scope` are
  gone; `--addr` defaults to `127.0.0.1:8081` and must be loopback;
  `--ops-addr` is new. Anything that ran the binary directly with the old
  flags must drop them.
- **Existing access tokens keep working:** same issuer, same `aud`.

## Configuration reference

| flag | env | default | |
|---|---|---|---|
| `--netbox-url` | `NETBOX_URL` | — | **Required.** Base URL, no `/api`. |
| `--netbox-token` | `NETBOX_TOKEN` | — | **Required.** NetBox API token. |
| `--transport` | `TRANSPORT` | `stdio` | `stdio` or `http`. |
| `--addr` | `ADDR` | `127.0.0.1:8081` | HTTP transport only. Where MCP is served, at `/mcp`. **Must be loopback** (`localhost`, `127.0.0.0/8`, `::1`): the server refuses to start otherwise. |
| `--allow-non-loopback` | `ALLOW_NON_LOOPBACK` | `false` | HTTP transport only. Lets `--addr` be a non-loopback address. This process authenticates nobody, so only behind something that does. |
| `--ops-addr` | `OPS_ADDR` | `0.0.0.0:9090` | HTTP transport only. Serves `/healthz` and nothing of MCP, for the kubelet. Empty disables it. |

The server takes no issuer, resource URL or scope: those belong to the
resource-proxy sidecar.

## Verifying auth is wired up

Through the gateway, without a token, the MCP endpoint refuses:

    curl -s -i -X POST https://mcp.example/netbox

answers `401` with a
`WWW-Authenticate: Bearer resource_metadata="...", scope="openid"` header
naming the proxy's protected-resource metadata:

    curl -s https://mcp.example/.well-known/oauth-protected-resource/netbox

answers `{"resource": "<auth.resourceUrl>", "authorization_servers":
["<auth.issuerUrl>"], "scopes_supported": ["<auth.scope>"], ...}`.

The server itself answers neither: on its loopback address it has no
authentication and serves only `/mcp`, so do not expose it
(`--allow-non-loopback` exists for the rare deployment that fronts it with
something else that does).

## When startup fails

The message is `netbox unreachable or token rejected`, and the wrapped error
tells you which: a network error names the host, a rejected token shows
`403 Forbidden` from `/api/status/`.

A third failure is `discover object types`, which means NetBox answered but its
API root did not look like one — usually a URL pointing at a reverse proxy or a
login page rather than at NetBox itself.
