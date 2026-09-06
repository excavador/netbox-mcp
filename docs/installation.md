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
      --version 1.0.0 \
      --set netbox.url=http://netbox \
      --set netbox.existingSecret=netbox-mcp

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
| `image.registry` / `image.repository` | `ghcr.io` / `excavador/netbox-mcp` | |
| `image.tag` | `""` | Empty means the chart's `appVersion`. Pin it to upgrade deliberately. |
| `replicaCount` | `1` | The server is stateless, so more than one is safe. |
| `service.port` | `8080` | Also the container's listen port. |
| `resources` | 20m / 64Mi, limit 128Mi | |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, all capabilities dropped | |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |

The schema sets `additionalProperties: false`, so a typo like `replicaCounts`
fails to render instead of being silently ignored.

### The server has no authentication

The chart offers nothing that turns authentication on, because the server has
none. It expects a gateway in front of it that validates a token.

A `ClusterIP` Service and no Ingress or HTTPRoute is the safe default the chart
ships. **Do not expose it** until something in front is checking credentials.

### Health

`/healthz` answers liveness and readiness, and deliberately does **not** call
NetBox: a readiness probe that fails when a dependency blips takes the pod out
of service for something restarting cannot fix.

## Configuration reference

| flag | env | default | |
|---|---|---|---|
| `--netbox-url` | `NETBOX_URL` | — | **Required.** Base URL, no `/api`. |
| `--netbox-token` | `NETBOX_TOKEN` | — | **Required.** NetBox API token. |
| `--transport` | `TRANSPORT` | `stdio` | `stdio` or `http`. |
| `--addr` | `ADDR` | `0.0.0.0:8080` | HTTP transport only. |

## When startup fails

The message is `netbox unreachable or token rejected`, and the wrapped error
tells you which: a network error names the host, a rejected token shows
`403 Forbidden` from `/api/status/`.

A third failure is `discover object types`, which means NetBox answered but its
API root did not look like one — usually a URL pointing at a reverse proxy or a
login page rather than at NetBox itself.
