# Everything goes through here; CI runs the same recipes.

# Build and vet.
#
# Builds the COMMAND explicitly, not just ./... -- a repo whose main package is
# missing still passes `go build ./...`, because the library packages compile
# on their own.
check:
    go vet ./...
    go test ./...
    go build ./...
    go build -o /dev/null ./cmd/netbox-mcp
    just chart

# Lint and render the chart, and prove its guards still refuse bad input.
#
# A chart exercised only with correct values has not been tested: the schema
# and the required-value guards exist to REJECT things, so the checks that
# matter are the ones expecting failure.
auth_values := "--set auth.issuerUrl=https://access.example --set auth.resourceUrl=https://mcp.example/netbox"
# The proxy's tag and digest have no default: the consumer pins them. These are
# placeholders that only have to satisfy the schema.
proxy_values := "--set proxy.image.tag=1.0.0 --set proxy.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
# The gateway peer, and the route.
net_values := "--set-json 'networkPolicy.ingressFrom=[{\"namespaceSelector\":{\"matchLabels\":{\"kubernetes.io/metadata.name\":\"gateway\"}},\"podSelector\":{\"matchLabels\":{\"app.kubernetes.io/name\":\"envoy\"}}}]'"
route_values := "--set httpRoute.enabled=true --set-json 'httpRoute.parentRefs=[{\"name\":\"mcp\",\"namespace\":\"gateway\"}]' --set-json 'httpRoute.hostnames=[\"mcp.example\"]'"
base_values := "--set netbox.url=http://netbox --set netbox.existingSecret=x " + auth_values + " " + proxy_values + " " + net_values

chart:
    helm lint charts/netbox-mcp {{base_values}} {{route_values}}
    helm template t charts/netbox-mcp {{base_values}} {{route_values}} > /dev/null
    @# missing required values
    @! helm template t charts/netbox-mcp >/dev/null 2>&1 \
        || (echo "FAIL: rendered without netbox.url"; exit 1)
    @# url must not carry /api -- the server appends it
    @! helm template t charts/netbox-mcp {{base_values}} --set netbox.url=http://netbox/api >/dev/null 2>&1 \
        || (echo "FAIL: accepted a url ending in /api"; exit 1)
    @# unknown keys are typos, not options
    @! helm template t charts/netbox-mcp {{base_values}} --set replicaCounts=2 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an unknown values key"; exit 1)
    @# the proxy checks every token: refuse to render without the issuer or the
    @# resource URL, and without the proxy pinned by tag AND digest
    @! helm template t charts/netbox-mcp --set netbox.url=http://netbox \
        --set netbox.existingSecret=x {{proxy_values}} {{net_values}} >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.issuerUrl / auth.resourceUrl"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set proxy.image.tag= >/dev/null 2>&1 \
        || (echo "FAIL: rendered without proxy.image.tag"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set proxy.image.digest= >/dev/null 2>&1 \
        || (echo "FAIL: rendered without proxy.image.digest"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set proxy.image.digest=sha256:abc >/dev/null 2>&1 \
        || (echo "FAIL: accepted a malformed proxy digest"; exit 1)
    @# the audience is compared byte for byte: no trailing slash, https only, a path
    @! helm template t charts/netbox-mcp {{base_values}} --set auth.resourceUrl=https://mcp.example/netbox/ >/dev/null 2>&1 \
        || (echo "FAIL: accepted a resourceUrl ending in /"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set auth.resourceUrl=http://mcp.example/netbox >/dev/null 2>&1 \
        || (echo "FAIL: accepted a non-https resourceUrl"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set auth.resourceUrl=https://mcp.example >/dev/null 2>&1 \
        || (echo "FAIL: accepted a resourceUrl with no path"; exit 1)
    @# an empty NetworkPolicy ingress would admit nobody; a route without a parent attaches nowhere
    @! helm template t charts/netbox-mcp --set netbox.url=http://netbox --set netbox.existingSecret=x \
        {{auth_values}} {{proxy_values}} >/dev/null 2>&1 \
        || (echo "FAIL: rendered a NetworkPolicy that admits nobody"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} --set httpRoute.enabled=true >/dev/null 2>&1 \
        || (echo "FAIL: rendered an HTTPRoute with no parentRefs"; exit 1)
    @! helm template t charts/netbox-mcp {{base_values}} {{route_values}} --set-json 'httpRoute.hostnames=["other.example"]' >/dev/null 2>&1 \
        || (echo "FAIL: accepted a route whose hostnames exclude the resource host"; exit 1)
    @# reloader reads its annotation on the Deployment, not the pod template.
    @# An annotation that lands in the wrong place looks right and never fires.
    @helm template t charts/netbox-mcp {{base_values}} \
        --set-string 'deploymentAnnotations.reloader\.stakater\.com/auto=true' \
      | awk '/^kind: Deployment/,/^spec:/' | grep -q 'reloader.stakater.com/auto' \
      || (echo "FAIL: deploymentAnnotations did not reach the Deployment"; exit 1)
    @# auth.scope defaults to openid and reaches the proxy's SCOPE, and can be overridden.
    @helm template t charts/netbox-mcp {{base_values}} \
      | grep -A1 'name: SCOPE' | grep -q '"openid"' \
      || (echo "FAIL: auth.scope did not default to openid on SCOPE"; exit 1)
    @helm template t charts/netbox-mcp {{base_values}} --set auth.scope=openid+mcp \
      | grep -A1 'name: SCOPE' | grep -q '"openid+mcp"' \
      || (echo "FAIL: auth.scope did not override SCOPE"; exit 1)
    @echo "chart ok: renders, refuses missing or malformed values (issuer, resource URL, pinned proxy, empty ingress, route without parent), a /api url and unknown keys, annotates the Deployment, wires auth.scope, and keeps the sidecar the only listener"

# Run against a NetBox instance over stdio (the default transport).
run url token:
    NETBOX_URL={{url}} NETBOX_TOKEN={{token}} go run ./cmd/netbox-mcp

# Serve over HTTP, as it runs in a cluster: MCP on loopback at /mcp, with NO
# authentication (the resource-proxy sidecar does that in a pod), and /healthz
# on the ops listener. See docs/design/cimd-auth.md.
serve url token addr="127.0.0.1:8081":
    NETBOX_URL={{url}} NETBOX_TOKEN={{token}} TRANSPORT=http ADDR={{addr}} \
        go run ./cmd/netbox-mcp

# What the release will build, without publishing.
#
# KO_DOCKER_REPO is required even when publishing is skipped -- ko needs a
# repository to name the image it builds, and fails before building without
# one. CI passes the real registry; locally any name will do.
snapshot:
    KO_DOCKER_REPO=ko.local goreleaser release --snapshot --clean --skip=publish
