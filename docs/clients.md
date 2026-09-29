# Connecting a client

## Claude Code, locally over stdio

    claude mcp add netbox \
      -e NETBOX_URL=https://netbox.example.com \
      -e NETBOX_TOKEN=... \
      -- /path/to/netbox-mcp

Then `/mcp` should list `netbox` as connected. Ask for something that needs a
read — "what device roles exist?" — to confirm it reaches NetBox rather than
merely starting.

Add `-s user` to make it available in every project rather than the current
directory.

## Claude Code, over HTTP

    claude mcp add --transport http netbox https://mcp.example.com/netbox

Claude Code follows the MCP authorization spec: it requests the endpoint,
gets a `401` naming this server's own protected-resource metadata, follows
that to discover access-roster as the authorization server, and opens a
browser for you to log in. It identifies itself with a Client ID Metadata
Document it already serves at a fixed URL of its own
(`https://claude.ai/oauth/claude-code-client-metadata`) — nothing to
register, nothing to configure on the client side, provided access-roster's
policy allow-lists `claude.ai` under `client_documents.origins` (see
[design/cimd-auth.md](design/cimd-auth.md)).

If the deployment predates that policy change, or the client you are using
does not support CIMD, connect with a manually-minted token instead:

    claude mcp add --transport http netbox https://mcp.example.com/netbox \
      --header "Authorization: Bearer ..."

## Driving the tools

**Start with `netbox_object_types`** if you are unsure what something is
called. It lists what this instance actually serves, which is not the same on
every NetBox — plugins add their own.

**Always pass `fields`.** A NetBox object has dozens of fields and most
responses need three of them. `fields: ["id", "name"]` is the difference
between a readable answer and a context window full of nested URLs.

**Filters are single-hop.** `{"site_id": 1}` works; `{"device__site_id": 1}`
does not. Do it in two calls: find the site, then filter by its id.

**For "any of these", pass a list** — `{"id": [1, 2, 3]}` — which becomes the
parameter repeated. The `__in` suffix looks like it should do this and NetBox
ignores it, returning *everything*; this server refuses `__in` rather than let
that look like a match.

**`netbox_update_object` is a `PATCH`.** Fields you do not mention keep their
values. There is no full-replace tool, on purpose: NetBox's `PUT` clears
everything omitted, which is almost never what a partial change means.

**Creating objects means ids.** Related fields take numeric ids, not names:
`{"name": "sw1", "site": 3, "device_type": 12, "role": 4}`. Read an existing
object of the same type first if you are unsure what a type requires.

## What it will not do

`users` is unreachable entirely. Scripts, webhooks, event rules, data sources
and config templates can be read but not written — each is a way to make NetBox
execute something or call out somewhere. The refusal explains itself when you
hit it.

## Reading the annotations

Every tool declares whether it reads or writes, so a client can decide what to
confirm before calling:

| annotation | tools |
|---|---|
| `readOnlyHint` | `netbox_object_types`, `netbox_get_objects`, `netbox_get_object`, `netbox_search`, `netbox_changelog` |
| `destructiveHint: false` | `netbox_create_object` |
| `destructiveHint: true` | `netbox_update_object`, `netbox_delete_object` |

If your client offers per-tool approval, gate the destructive pair. Note that
NetBox **cascades** deletes: removing a site takes its racks and devices with
it, and the changelog records that but cannot undo it.
