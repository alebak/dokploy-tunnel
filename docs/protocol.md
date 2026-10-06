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
4. The stream to the target is opened: through a repeater container on the
   target's own Docker network, which connects to the port. The companion
   waits until the connection to the target is established, so a target
   that refuses it is reported as `target_unreachable` before the upgrade.
5. The connection is upgraded.

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
| 502 | `unreachable` | The Dokploy API could not be reached, or did not answer like Dokploy |
| 502 | `target_unreachable` | The stream to the target could not be opened |
| 503 | `unavailable` | The companion is shutting down |
| 504 | `timeout` | Dokploy or the target did not answer in time |
| 500 | `internal` | Unexpected failure |

`wrong_server` is only returned to a caller that can read the service, and
carries the server the target belongs to, `local` for the Dokploy server
itself, so the client can pick the right companion:

```json
{"code":"wrong_server","message":"the service runs on Dokploy server srv_edge, not on this companion's server local","expected_server_id":"srv_edge"}
```

Clients must branch on `code`, not on `message`. New codes may be added;
existing codes keep their meaning.

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
