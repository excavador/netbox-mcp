# Architecture

A single Go binary that translates MCP tool calls into NetBox REST calls.
No database, no cache, no state beyond one map built at startup: every tool
call is one HTTP request, so the process can be restarted or scaled freely.

## The request path

    Claude Code (or any MCP client)
      │  presents client_id = an HTTPS URL to its own Client ID Metadata
      │  Document (CIMD) -- no pre-registration, no DCR
      │  gets back an access token: iss=access-roster, aud=this resource
      │  MCP over streamable HTTP, Authorization: Bearer <access token>
      ▼
    netbox-mcp  (TRANSPORT=http, listening on :8080/mcp)
      │  validates the token itself: signature against access-roster's
      │  JWKS, iss, and aud == RESOURCE_URL (RFC 8707) -- see auth.go
      │  Authorization: Token <NetBox API token>
      ▼
    NetBox  /api/...

Two different credentials, and conflating them is the mistake this design
exists to prevent:

- The **caller's** token is an access-roster access token identifying a
  person, bound to this server's own resource URL as its audience.
  `netbox-mcp` verifies it itself, in-process — see
  [design/cimd-auth.md](design/cimd-auth.md) for why authentication moved
  from a perimeter gateway into the binary.
- The **server's** token is a NetBox API token, held in a Kubernetes Secret and
  read once at startup. NetBox accepts nothing else — note the scheme is
  `Token`, not `Bearer`.

### The server validates its own bearer tokens

`ISSUER_URL` and `RESOURCE_URL` are both required for the http transport --
there is no gateway fallback. The server verifies a request's bearer token
against access-roster's own JWKS, requires `aud` to equal `RESOURCE_URL`
exactly (RFC 8707), and serves its own OAuth 2.0 Protected Resource Metadata
at `/.well-known/oauth-protected-resource` (RFC 9728) so a compliant client
can discover access-roster without being told out of band. See
[design/cimd-auth.md](design/cimd-auth.md).

Exposed with no `ISSUER_URL`/`RESOURCE_URL` set, the process refuses to
start the http transport at all -- there is no way to accidentally run it
unauthenticated.

### The NetBox token is the real limit

Every caller access-roster admits still shares one NetBox token as far as
NetBox itself is concerned, so NetBox attributes every change to it rather
than to the person who asked. The changelog records *what* changed and
*when*, but not *who* beyond that token's user -- access-roster identifies
*who may reach this server*, not who NetBox believes made a given change.

That token's NetBox permissions are therefore the meaningful boundary, and
they are worth setting deliberately. A read-only token makes every write tool
fail with a 403 — a perfectly good way to run this if you only want reads.

## Generic tools over an object type

NetBox serves several hundred models through one uniform REST shape:
`/api/{app}/{endpoint}/` with the same list, detail, create, update and delete
verbs. So there are seven tools that take an `objectType`, not seven hundred
that each hardcode one.

The alternative — a tool per model — would bury a client in names it will never
call, and every NetBox release would add more. A client that can read
`dcim.device` can read `ipam.ipaddress` without anyone writing a new tool.

### The registry is discovered, not hardcoded

NetBox's API root lists its apps, and each app lists its endpoints. Both are
read at startup and turned into a map from object type to path.

This is the one place this server deliberately differs from upstream, which
keeps a hand-written table of several hundred entries. Discovery means the
mapping matches the version being talked to, picks up whatever plugins the
instance has, and cannot go stale.

The cost is ~10 requests at startup and a hard dependency on NetBox being
reachable at boot — which is already true, because the token is checked there
anyway.

**Both spellings resolve.** `dcim.device` is derived from the endpoint
`devices` by de-pluralising, which is a guess: `ip-addresses` becomes
`ipaddress` correctly, but no rule is right for every English plural. So the
endpoint form `dcim/devices` is accepted too, and a caller is never stuck when
the guess is wrong. An unknown type is answered with the near misses rather
than a listing of everything.

## The safety boundary

Two layers, for two different reasons.

**The `users` app is never indexed**, so nothing under it resolves — not even
for reading. Tokens, permissions and group membership are credentials, and a
server holding one token has no business enumerating the others.

**Some types are readable but not writable**, because their contents are things
NetBox *acts on* rather than records:

| type | what writing one actually does |
|---|---|
| `extras.script`, `extras.scriptmodule` | NetBox runs these. It is remote code execution. |
| `extras.webhook`, `extras.eventrule` | makes NetBox issue HTTP requests of the author's choosing |
| `core.datasource`, `core.datafile` | syncs scripts and config from a remote repo — code execution, one step removed |
| `extras.configtemplate` | rendered against real device data and pushed to devices |

The refusal names the reason, so a caller that hits it understands the
boundary rather than reading it as a bug.

## Details that are load-bearing

**`structuredContent` must be an object.** MCP requires it, and a non-object is
wrapped under `results` — the same key NetBox uses for its own paged responses,
so a caller reading one shape does not have to learn a second.

**Updates are `PATCH`, never `PUT`.** NetBox's `PUT` requires the whole object
and clears anything omitted. "Change some fields" must not silently blank the
rest, so the update tool only ever issues a `PATCH`.

**`limit` is bounded.** It defaults to 25 and is capped at 200. NetBox's own
default is 50, and a model paging through objects of forty fields each fills a
context window with data nobody asked for. `fields` is offered on every read
for the same reason, and the tool descriptions push callers towards it.

**`__in` is refused, loudly.** It looks like it should mean "any of these" and
NetBox ignores it, answering with *everything* — which reads as a filter that
matched broadly rather than one that was silently dropped. The refusal says to
pass a list instead, which is how NetBox actually expresses OR: the parameter
repeated.

## Nothing is modelled

Request and response bodies pass through as `any`. No Go structs mirror
NetBox's types.

A duplicate set of structs would be a second source of truth that silently
drops fields NetBox adds — drift that is invisible, because a missing field
looks like a NetBox that did not return one. Passing bodies through means a
NetBox upgrade exposes new fields immediately.

The cost is that a malformed body is rejected by NetBox rather than by the
tool. Given the caller is a language model, an error from NetBox naming the
offending field beats a schema error naming a Go type.
