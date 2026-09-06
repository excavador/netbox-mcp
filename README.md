# netbox-mcp

[Model Context Protocol](https://modelcontextprotocol.io) server for
[NetBox](https://netboxlabs.com/docs/netbox/), in Go. Reads **and writes**
DCIM and IPAM: ask what is in a rack, then record the switch you just cabled.

```
ghcr.io/excavador/netbox-mcp              image, multi-arch amd64 + arm64
oci://ghcr.io/excavador/charts/netbox-mcp chart
```

## Quick start

Create a NetBox API token, then point a client at the binary:

```
claude mcp add netbox \
  -e NETBOX_URL=https://netbox.example.com \
  -e NETBOX_TOKEN=... \
  -- /path/to/netbox-mcp
```

## Documentation

| | |
|---|---|
| [Installation](docs/installation.md) | binary, container and Helm chart; every value and env var |
| [Connecting a client](docs/clients.md) | Claude Code over stdio and HTTP; how to drive the tools |
| [Architecture](docs/architecture.md) | the discovered registry, the safety boundary, why the tools are generic |
| [Development](docs/development.md) | devbox, `just check`, testing, releasing |

## Generic tools, not one per model

NetBox serves several hundred models through one uniform REST shape, so this
exposes **seven** tools that take an object type rather than seven hundred that
each hardcode one. A client that can read `dcim.device` can read
`ipam.ipaddress` without a new tool.

| tool | |
|---|---|
| `netbox_object_types` | what this NetBox serves, dotted names, optionally filtered by app |
| `netbox_get_objects` | list or filter one type |
| `netbox_get_object` | one object by id |
| `netbox_search` | search across every type when the type is not yet known |
| `netbox_changelog` | who changed what, when, and the before and after |
| `netbox_create_object` | create one object |
| `netbox_update_object` | change some fields of one object (a `PATCH`) |
| `netbox_delete_object` | delete one object |

The object type registry is **discovered from the instance at startup**, not
hardcoded, so it matches that NetBox's version and picks up its plugins. Both
spellings work: `dcim.device` and `dcim/devices`.

## What it will not do

The `users` app is unreachable — not even for reading. Tokens, permissions and
group membership are credentials, and a server holding one token has no
business enumerating the others.

These can be read but never written, because each is a way to make NetBox
execute something or call out somewhere:

- **`extras.script`** — scripts are arbitrary Python that NetBox runs. Writing
  one is remote code execution.
- **`extras.webhook`** and **`extras.eventrule`** — both make NetBox issue HTTP
  requests of the author's choosing.
- **`core.datasource`** and **`core.datafile`** — sync config and scripts from a
  remote repo, so writing one is code execution a step removed.
- **`extras.configtemplate`** — rendered against real device data and pushed to
  devices.

## Two things to know before deploying it

**NetBox accepts one credential: its own API token**, sent as `Authorization:
Token …` — not `Bearer`, and never an OIDC token. The token's NetBox
permissions are the real limit on what this server can do; a read-only token
makes every write tool fail with a 403, which is a perfectly good way to run it.

**The HTTP transport authenticates nothing.** It is built to sit behind a
gateway that validates a token. Exposed directly to a network, it is NetBox,
writable. There is no setting that turns authentication on, because there is
none to turn on.

## Why this exists rather than the upstream server

[`netboxlabs/netbox-mcp-server`](https://github.com/netboxlabs/netbox-mcp-server)
is NetBox Labs' own, Apache-2.0, actively maintained, and good. It is also
**read-only by design** — a pull request adding opt-in create, update and
delete was closed unmerged, and its README says so plainly:

> The server is intentionally simple: easy to get started with, hard to misuse
> (read-only by default, no plugin surface), and easy to fork and adapt.
> Forking under Apache 2.0 is a first-class path for users who need
> capabilities beyond the project's scope.

We need writes, and Go rather than Python to match the rest of the estate. The
generic-tools-over-an-object-type design is theirs and it is the right one;
the implementation here is our own, and the registry is discovered rather than
hardcoded.

## Licence

MIT, © 2026 Oleg Tsarev.
