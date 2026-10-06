# Tunnel wire protocol (v1)

This document is the contract between `doktunnel` (the client) and
`doktunnel-companion` (the server installed on each Dokploy server). It is
modeled on Dokploy's own container-terminal WebSocket
(`apps/dokploy/server/wss/docker-container-terminal.ts`): the caller
authenticates with its Dokploy API key, names the service it wants, and the
server asks Dokploy whether that key may read the service before doing
anything else.

## Overview

- One WebSocket carries **one TCP stream**. The client opens a new WebSocket
  for every local TCP connection it accepts, and closes it when that
  connection ends.
- Every upgrade request is authorized on its own, with the caller's key, by
  the Dokploy API. The companion keeps no sessions, tokens or permission
  cache of its own.
- Everything that can fail before data flows (bad input, authorization, the
  wrong companion, an unreachable target) fails **before the upgrade**, as a
  plain HTTP response with a JSON error body. After the `101 Switching
  Protocols` response, the stream is connected and only WebSocket close codes
  are used.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` (WebSocket upgrade) | `/v1/tunnel` | Open one TCP stream to a target |
| `GET` | `/v1/ports` | List the TCP ports a target exposes, for a client that was given no port |
| `GET` | `/healthz` | Liveness: `200` with `{"status":"ok"}` while the companion accepts tunnels, `503` with `{"status":"shutting_down"}` while it drains |

The path is versioned; an incompatible protocol gets a new path.

## Opening a tunnel

```http
GET /v1/tunnel?serviceType=postgres&serviceId=pg_main&port=5432 HTTP/1.1
Host: companion.example.com
Connection: Upgrade
Upgrade: websocket
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: ...
x-api-key: <Dokploy API key>
```

### Headers

| Header | Required | Meaning |
|--------|----------|---------|
| `x-api-key` | yes | The caller's Dokploy API key, exactly as the Dokploy API takes it. It is forwarded only to the Dokploy panel the companion is configured with, and never logged. At most 1024 bytes. |
| `Origin` | no | Browsers send it. When present, its host must equal the request's `Host`, which rejects cross-site pages. `doktunnel` does not send it. |

The Dokploy session cookie is **not** accepted. The CLI only has API keys, a
cookie of the panel's domain is never sent to the companion's host anyway,
and accepting cookies on a WebSocket would expose the endpoint to
cross-site WebSocket hijacking.

### Query parameters

| Parameter | Required | Meaning |
|-----------|----------|---------|
| `serviceType` | yes | `application`, `compose`, `postgres`, `mysql`, `mariadb`, `mongo`, `redis`, `libsql`, or `compose_service` for a service inside a compose stack |
| `serviceId` | yes | The Dokploy ID of the service. For `compose_service`, `<composeId>/<service>`: the ID of the compose stack and the service's name in its compose file, such as `cmp_myapp/postgres`. These are the `type` and `id` that `doktunnel services --json` prints. |
| `port` | yes | The container port to connect to, `1`–`65535`. The companion does not guess ports; the client resolves them. |
| `serverId` | no | The Dokploy server the client expects the target to run on: a server ID, or `local` for the Dokploy server itself. When present, it must match the companion's server. |

IDs are 1–128 characters of `A-Z a-z 0-9 _ -`. Compose service names are
1–128 characters of `A-Z a-z 0-9 . _ -`, as the Compose specification allows.
Unknown parameters are ignored.

A `compose` target names a whole stack, which has no single port; it is
authorized like any service but refused with `target_unreachable`. Forward to
one of its services with `compose_service` instead.

### What the companion checks, in order

1. The request is a `GET` WebSocket upgrade, and the parameters and key are
   well formed.
2. **Authorization, delegated to Dokploy** with the caller's key:
   - a Dokploy service: `GET /api/<serviceType>.one?<idField>=<serviceId>`,
     such as `postgres.one?postgresId=pg_main`;
   - a compose service: `compose.one?composeId=<composeId>`, then
     `compose.loadServices?composeId=<composeId>&type=cache` to check that
     the service name exists in the compose file stored on the server.
     `type=fetch` is never sent: it would make Dokploy clone the compose
     source again.

   Dokploy answering the service is the authorization: read access is enough
   to forward to a service, just as read access is enough for Dokploy's own
   container terminal. The companion never calls organization-wide or
   Docker-wide procedures.
3. The service runs on this companion's Dokploy server (the `serverId` of the
   `.one` response; empty means the Dokploy server itself), and matches the
   optional `serverId` parameter.
4. The tunnel fits the companion's limits: by default 64 tunnels open at
   once per API key and 512 in all. Only authorized requests take a slot,
   and the slot is freed when the tunnel ends.
5. The stream to the target is opened: through a repeater container on the
   target's own Docker network, which connects to the target's IP address
   there, as Docker reports it. The target is found only from sources tied
   to the service Dokploy authorized: the Swarm service of that exact name
   and its running tasks, or, for a `docker-compose` stack, the containers
   Compose labelled with the stack's appName and service, deployed from
   Dokploy's directory for that appName and carrying no Swarm labels. The
   companion waits until the connection to the target is established, so a
   target that refuses it is reported as `target_unreachable` before the
   upgrade.
6. The connection is upgraded.

### Error responses (before the upgrade)

Every rejection is an HTTP response with `Content-Type: application/json` and
a body of this shape:

```json
{"code":"permission_denied","message":"the API key cannot read this service"}
```

| HTTP status | `code` | When |
|-------------|--------|------|
| 400 | `invalid_argument` | A parameter or header is missing or malformed |
| 401 | `unauthenticated` | No `x-api-key` header |
| 403 | `permission_denied` | Dokploy rejected the key for this target: the key is invalid, belongs to another organization, cannot read the service, or the service does not exist. These cases are deliberately indistinguishable. |
| 403 | `forbidden_origin` | A browser `Origin` that does not match `Host` |
| 404 | `not_found` | The caller can read the compose stack, but its stored compose file has no such service, or no compose file is stored on the server yet |
| 405 | `method_not_allowed` | Not a `GET` |
| 409 | `network_not_attachable` | The target's network cannot be joined to reach it; enable "attachable" on that network in Dokploy |
| 421 | `wrong_server` | The target runs on another Dokploy server; the body names it in `expected_server_id` (see below) |
| 426 | `upgrade_required` | A `GET` without a WebSocket upgrade |
| 429 | `too_many_tunnels` | The API key, or the companion as a whole, has as many tunnels open as allowed; close some and retry |
| 502 | `unreachable` | The Dokploy API could not be reached, or did not answer like Dokploy |
| 502 | `target_unreachable` | The stream to the target could not be opened: nothing runs for it, its containers are ambiguous, the connection was refused, or the companion refuses the target (see below) |
| 503 | `unavailable` | The companion is shutting down |
| 504 | `timeout` | Dokploy or the target did not answer in time |
| 500 | `internal` | Unexpected failure |

The companion refuses some targets with `target_unreachable` whatever the
caller may read: services whose appName is `dokploy` or starts with
`dokploy-`, which Dokploy's own panel, database, cache and proxy use (and
which Dokploy lets users pick for their own services too), and containers
that run privileged, on the host's network, or with the Docker socket
mounted. Such containers are host infrastructure; a tunnel to them would
give whoever can read the service control of the host.

`wrong_server` is only returned to a caller that can read the service, and
carries the server the target belongs to, `local` for the Dokploy server
itself, so the client can pick the right companion:

```json
{"code":"wrong_server","message":"the service runs on Dokploy server srv_edge, not on this companion's server local","expected_server_id":"srv_edge"}
```

Clients must branch on `code`, not on `message`. New codes may be added;
existing codes keep their meaning.

## Listing a target's ports

A client that was not given a port asks the companion which ports the
target exposes. This is a plain HTTP request, not a WebSocket:

```http
GET /v1/ports?serviceType=compose_service&serviceId=cmp_myapp%2Fpostgres HTTP/1.1
Host: companion.example.com
x-api-key: <Dokploy API key>
```

The headers are those of a tunnel request, and so are the query parameters,
without `port` (one is ignored): `serviceType`, `serviceId` and the optional
`serverId`.

The companion checks the request as it checks a tunnel request: steps 1 to 3
above, with the same authorization through Dokploy and the caller's key,
except that no WebSocket upgrade is expected. Then it **inspects** the
target's running container, found the same way as for a tunnel, and answers
the TCP ports its image exposes (Docker's `Config.ExposedPorts`), sorted by
number:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"ports":[{"port":5432,"protocol":"tcp"}]}
```

- Only TCP ports are listed, each once; `protocol` is always `tcp`.
- The list is empty, never `null`, when the ports are unknown: the image
  exposes none, the target's only running Swarm task is on another node
  (whose containers the companion cannot inspect), or the companion cannot
  forward at all (`DOKTUNNEL_COMPANION_BRIDGE=none`). The client then needs a port
  from the user; it must not guess one.
- A listing takes no tunnel slot and creates nothing: no repeater container
  is started, no network is joined, no connection to the target is made.
  Exposed ports are what the image declares, not proof that something
  listens on them.

Rejections have the JSON body and status codes of the tunnel's
[error responses](#error-responses-before-the-upgrade). Those a ports
request can get:

| HTTP status | `code` | When |
|-------------|--------|------|
| 400 | `invalid_argument` | A parameter or header is missing or malformed |
| 401 | `unauthenticated` | No `x-api-key` header |
| 403 | `permission_denied` | Dokploy rejected the key for this target, as for a tunnel |
| 403 | `forbidden_origin` | A browser `Origin` that does not match `Host` |
| 404 | `not_found` | The compose stack has no such service, as for a tunnel |
| 405 | `method_not_allowed` | Not a `GET` |
| 421 | `wrong_server` | The target runs on another Dokploy server, named in `expected_server_id` |
| 502 | `unreachable` | The Dokploy API could not be reached |
| 502 | `target_unreachable` | Nothing runs for the target, its containers are ambiguous, or the companion refuses the target, as for a tunnel |
| 503 | `unavailable` | The companion is shutting down |
| 504 | `timeout` | Dokploy or Docker did not answer in time |
| 500 | `internal` | Unexpected failure |

## The stream (after the upgrade)

- Data travels as **binary** messages in both directions; each message is a
  chunk of the TCP byte stream, and message boundaries carry no meaning.
- Messages must not exceed **64 KiB**. The companion sends at most 32 KiB per
  message.
- Text messages are a protocol error.
- There is no greeting, no framing inside messages, and no in-band error
  frame. Errors after the upgrade are reported only in the close frame.
- There is no half-close. When either side's TCP connection ends, its peer
  closes the WebSocket and the whole stream ends.
- The companion sends a ping every 30 seconds so that proxies do not close
  idle streams; WebSocket libraries answer pings automatically. A late pong
  is not an error, since a slow target delays reading it. Idle streams are
  kept open indefinitely; dead peers are detected by TCP keepalive.

A target that accepts the connection and then closes it appears as an
upgrade followed by a normal close.

### Close codes

| Code | Meaning |
|------|---------|
| 1000 normal closure | The stream ended: the client closed it, or the target closed its connection |
| 1001 going away | The companion is shutting down |
| 1003 unsupported data | The peer sent a text message |
| 1009 message too big | The peer sent a message over 64 KiB |
| 1011 internal error | Reading from or writing to the target or the peer failed |

Codes 4000–4999 are reserved for future use by this protocol.

## Transport security

The companion speaks plain HTTP and expects a TLS-terminating proxy in front
of it, such as the Traefik instance Dokploy already runs. The API key travels
in a header on every tunnel, so it must only cross untrusted networks over
`wss://`.
