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

chart:
    helm lint charts/netbox-mcp \
        --set netbox.url=http://netbox --set netbox.existingSecret=x {{auth_values}}
    helm template t charts/netbox-mcp \
        --set netbox.url=http://netbox --set netbox.existingSecret=x {{auth_values}} > /dev/null
    @# missing required values
    @! helm template t charts/netbox-mcp >/dev/null 2>&1 \
        || (echo "FAIL: rendered without netbox.url"; exit 1)
    @# url must not carry /api -- the server appends it
    @! helm template t charts/netbox-mcp --set netbox.url=http://netbox/api \
        --set netbox.existingSecret=x {{auth_values}} >/dev/null 2>&1 \
        || (echo "FAIL: accepted a url ending in /api"; exit 1)
    @# unknown keys are typos, not options
    @! helm template t charts/netbox-mcp --set netbox.url=http://netbox \
        --set netbox.existingSecret=x {{auth_values}} --set replicaCounts=2 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an unknown values key"; exit 1)
    @# no gateway fallback any more -- auth.issuerUrl/resourceUrl are required
    @! helm template t charts/netbox-mcp --set netbox.url=http://netbox \
        --set netbox.existingSecret=x >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.issuerUrl / auth.resourceUrl"; exit 1)
    @# reloader reads its annotation on the Deployment, not the pod template.
    @# An annotation that lands in the wrong place looks right and never fires.
    @helm template t charts/netbox-mcp --set netbox.url=http://netbox \
        --set netbox.existingSecret=x {{auth_values}} \
        --set-string 'deploymentAnnotations.reloader\.stakater\.com/auto=true' \
      | awk '/^kind: Deployment/,/^spec:/' | grep -q 'reloader.stakater.com/auto' \
      || (echo "FAIL: deploymentAnnotations did not reach the Deployment"; exit 1)
    @echo "chart ok: renders, refuses missing values (including auth), a /api url and unknown keys, and annotates the Deployment"

# Run against a NetBox instance over stdio (the default transport).
run url token:
    NETBOX_URL={{url}} NETBOX_TOKEN={{token}} go run ./cmd/netbox-mcp

# Serve over HTTP, as it runs in a cluster. Requires an access-roster
# issuer and this server's own external resource URL -- see
# docs/design/cimd-auth.md.
serve url token issuer resource addr="127.0.0.1:8080":
    NETBOX_URL={{url}} NETBOX_TOKEN={{token}} TRANSPORT=http ADDR={{addr}} \
        ISSUER_URL={{issuer}} RESOURCE_URL={{resource}} \
        go run ./cmd/netbox-mcp

# What the release will build, without publishing.
#
# KO_DOCKER_REPO is required even when publishing is skipped -- ko needs a
# repository to name the image it builds, and fails before building without
# one. CI passes the real registry; locally any name will do.
snapshot:
    KO_DOCKER_REPO=ko.local goreleaser release --snapshot --clean --skip=publish
