# Install the companion on a Dokploy server

This guide is for the **Dokploy administrator**. It installs `doktunnel-companion` and its Docker socket proxy as one Docker Compose service inside Dokploy, published at `https://<panel-domain>/doktunnel` through the Traefik instance Dokploy already runs. That is the address `doktunnel` looks for by default, so users only need `doktunnel context add`.

What you end up with:

| Service | Role | Networks | Published |
|---------|------|----------|-----------|
| `socket-proxy` | Holds the Docker socket and forwards only the calls the companion makes ([Socket proxy](../README.md#socket-proxy)) | `internal` only | Never |
| `companion` | Serves tunnels, checks every request against Dokploy, reaches the proxy over `internal` | `internal`, `dokploy-network` | Through Traefik, at `/doktunnel` |

## Quick path

1. [Check the prerequisites](#prerequisites).
2. [Create an admin-only project](#1-create-an-admin-only-project).
3. [Create the compose service](#2-create-the-compose-service) with [`deploy/dokploy/docker-compose.yml`](../deploy/dokploy/docker-compose.yml).
4. [Set its variables and deploy](#3-set-the-variables-and-deploy).
5. [Verify](#4-verify): `https://<panel-domain>/doktunnel/healthz` answers `{"status":"ok"}`.
6. [Tell users to add a context](#5-connect-doktunnel).

## Prerequisites

- You are an owner or admin of the Dokploy organization and can run `docker` on the server.
- The panel is served on a domain over HTTPS (Dokploy's web server settings, with a Let's Encrypt certificate), such as `https://dokploy.example.com`. A panel reached only as `http://<ip>:3000` works too, with a [LAN port or another domain](#other-addresses).
- The group ID of the Docker socket on the server:

  ```sh
  stat -c %g /var/run/docker.sock
  ```

- A doktunnel release to install, such as `0.5.0` (see [Releases](https://github.com/alebak/dokploy-tunnel/releases)). Use the same version for the CLI and the companion where you can.

## 1. Create an admin-only project

Create a Dokploy project for the companion, such as `doktunnel`, and do not give members access to it.

Why: anyone who can edit a compose service can already control the host's Docker daemon (see [Trust model](../README.md#trust-model)), and this one holds the Docker socket. Read access to a service is also enough to open a tunnel to it.

## 2. Create the compose service

In the project, create a **Docker Compose** service (compose type *Docker Compose*, not *Stack*):

| Setting | Value |
|---------|-------|
| Server | The Dokploy server itself (for a remote server, see [Remote servers](#remote-servers)) |
| Provider | **Raw**, with the contents of [`deploy/dokploy/docker-compose.yml`](../deploy/dokploy/docker-compose.yml) from the release tag you install |
| Isolated Deployments | **Off** |
| Randomize Compose | **Off** |
| Domains tab | **Empty**: the file carries its own Traefik labels |

With no domain and no isolated deployment, Dokploy deploys the file exactly as written. Isolated Deployments would add a network to *every* service and connect Traefik to it, putting the socket proxy on a network Traefik reaches. Use **Preview Compose** to check that `socket-proxy` still lists only the `internal` network.

<details>
<summary>The compose file, explained</summary>

```yaml
services:
  socket-proxy:
    image: ghcr.io/alebak/doktunnel-companion:${DOKTUNNEL_VERSION}
    entrypoint: ["/usr/local/bin/doktunnel-socket-proxy"]  # same image, other binary
    group_add: ["${DOCKER_GID}"]          # nonroot user + the socket's group, not root
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    networks: [internal]                  # never dokploy-network, never published

  companion:
    image: ghcr.io/alebak/doktunnel-companion:${DOKTUNNEL_VERSION}
    environment:
      DOKTUNNEL_COMPANION_DOKPLOY_URL: ${DOKPLOY_URL:-http://dokploy:3000}
      DOKTUNNEL_COMPANION_SERVER_ID: ${SERVER_ID:-}
      DOCKER_HOST: tcp://socket-proxy:2375  # the proxy, not the socket
    volumes:
      - repeater-key:/var/lib/doktunnel    # the repeater key survives upgrades
    networks: [internal, dokploy-network]
    labels:                                # Host(panel) && PathPrefix(/doktunnel), prefix stripped
      - traefik.http.routers.doktunnel-companion.rule=Host(`${PANEL_DOMAIN}`) && PathPrefix(`/doktunnel`)
      # ... entrypoint websecure, certresolver letsencrypt, stripprefix, port 8080

networks:
  internal: { internal: true }             # no route out, only these two services
  dokploy-network: { external: true }
```

Both containers also run with a read-only root filesystem, no capabilities and `no-new-privileges`. The full file has every label and comment.

</details>

## 3. Set the variables and deploy

In the service's **Environment** tab, set:

```sh
DOKTUNNEL_VERSION=0.5.0           # image tag, without the "v"
PANEL_DOMAIN=dokploy.example.com  # the panel's domain, no scheme, no path
DOCKER_GID=988                    # from stat -c %g /var/run/docker.sock
```

Dokploy writes them to the `.env` file next to the compose file, where Compose reads them. A missing variable fails the deployment with `required variable ... is missing a value`.

Then **Deploy**. Both containers should be running; the companion logs a warning at startup that the Docker API is reached over plain TCP on a non-loopback address. That is expected: the `internal` network is what keeps that endpoint private.

## 4. Verify

Run these on your workstation and on the server. `<appName>` is the service's app name shown in Dokploy; Compose names the containers `<appName>-socket-proxy-1` and `<appName>-companion-1`.

**The companion answers through Traefik:**

```sh
curl https://dokploy.example.com/doktunnel/healthz
# {"status":"ok"}
```

**The proxy is only on the internal network:**

```sh
docker inspect --format '{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}' <appName>-socket-proxy-1
# <appName>_internal

docker network inspect --format '{{.Internal}}' <appName>_internal
# true

docker inspect --format '{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}' <appName>-companion-1
# <appName>_internal dokploy-network
```

If the proxy lists `dokploy-network` or any other network, stop the service and fix the deployment settings in [step 2](#2-create-the-compose-service).

**The proxy denies calls outside its allow-list.** The allow-list is tested in CI: the `docker integration` job runs the repeater suite through the proxy against a real Docker daemon and asserts that privileged containers, host mounts, other images, other containers and other endpoints are refused. The companion image is distroless, so there is no shell to repeat that from inside it. To see it on your server, optionally run a throwaway client on the internal network:

```sh
docker run --rm --network <appName>_internal curlimages/curl -s http://socket-proxy:2375/_ping
# OK
docker run --rm --network <appName>_internal curlimages/curl -s -w ' %{http_code}\n' http://socket-proxy:2375/version
# {"message":"doktunnel-socket-proxy: GET /version denied: ..."} 403
docker logs <appName>-socket-proxy-1 2>&1 | grep 'request denied'
```

During normal use, the proxy log has no `request denied` lines from the companion.

## 5. Connect doktunnel

Users add the panel as usual. `doktunnel context add` stores the companion URL `<panel URL>/doktunnel` and checks its `/healthz`, warning (without failing) if it does not answer:

```sh
doktunnel context add --url https://dokploy.example.com --name prod
doktunnel context list   # shows each context's companion URL
```

When the companion is published elsewhere, pass `--companion-url <URL>` to `context add`, or change an existing context with `doktunnel context set-companion <name> <URL>`. Contexts added before companion discovery have no companion URL until `set-companion` adds one. These commands land with [#70](https://github.com/alebak/dokploy-tunnel/pull/70); see [Contexts](../README.md#contexts).

## Other addresses

The default route needs the panel's own domain behind Traefik. If you prefer another address, edit the `companion` service and give users the matching `--companion-url`:

| Address | Change in the compose file | Companion URL |
|---------|----------------------------|---------------|
| Another domain, such as `tunnel.example.com` | Rule ``Host(`tunnel.example.com`)``, and remove the `middlewares` label | `https://tunnel.example.com` |
| A LAN port | Add `ports: ["8081:8080"]` to `companion` (and drop the labels if Traefik is not used) | `http://<server-ip>:8081` |

A LAN port is **plain HTTP**: every tunnel request carries the user's API key in clear. Use it only on a trusted network, such as a LAN or a VPN. Never publish a port on `socket-proxy`.

## Remote servers

Install one companion per Dokploy server whose services should be reachable. For a remote server, create the compose service on that server and also set:

```sh
DOKPLOY_URL=https://dokploy.example.com  # the panel, as reachable from the remote server
SERVER_ID=<the remote server's ID in Dokploy>
```

The remote server's Traefik needs a domain that points at it, so use the [another-domain](#other-addresses) variant. A context stores one companion URL, which by default is the Dokploy server's.

## Upgrade

1. Change `DOKTUNNEL_VERSION` in the Environment tab.
2. **Deploy**.

Both services use the same variable, so the companion and the proxy always run the same version, which their allow-list expects. The `repeater-key` volume persists, so the new companion recognizes and cleans up the repeater containers of the old one. Check [Verify](#4-verify) again afterwards.

## Uninstall

1. Delete the compose service in Dokploy.
2. Remove its key volume, if Dokploy left it:

   ```sh
   docker volume rm <appName>_repeater-key
   ```

3. Remove leftover repeater containers, after checking the list:

   ```sh
   docker ps -a --filter label=dev.doktunnel.repeater=1 --format '{{.ID}} {{.Names}}'
   docker rm -f <id>...
   ```

Repeaters are named `doktunnel-repeater-*` and run `alpine/socat`; anything else in the list was not created by the companion.

## Troubleshooting

| Symptom | Likely cause | Check |
|---------|--------------|-------|
| `404` at `/doktunnel/healthz` | Traefik has no route for it, or the prefix is not stripped | `PANEL_DOMAIN` matches the panel's domain exactly; the `traefik.*` labels and the `middlewares` label are present; the companion is on `dokploy-network`; Traefik's log (`docker logs dokploy-traefik`) |
| `502` or `504` at `/doktunnel/healthz` | The companion is down or restarting, or Traefik dials it on the wrong network | `docker logs <appName>-companion-1`; the `traefik.docker.network=dokploy-network` label |
| Companion exits at startup: Docker does not answer | The proxy is down or cannot open the socket | `docker logs <appName>-socket-proxy-1`; a `permission denied` on the socket means `DOCKER_GID` is wrong |
| Proxy log shows `request denied` for the companion's calls; tunnels fail | The companion and the proxy run different versions, or a different repeater image | Same `DOKTUNNEL_VERSION` for both; if you set `DOKTUNNEL_COMPANION_REPEATER_IMAGE`, set `DOKTUNNEL_SOCKET_PROXY_REPEATER_IMAGE` to the same value |
| Tunnels fail with `permission_denied` | The API key cannot read the service | The key and its access in Dokploy |
| Tunnels fail with `unreachable` or `timeout` | The companion cannot reach the Dokploy API | `DOKPLOY_URL`, as reachable from the companion; the companion's log |
| Tunnels fail with `wrong_server` | `SERVER_ID` does not match the server the companion runs on | Empty on the Dokploy server; the remote server's ID elsewhere |
| `context add` warns that the companion did not answer | Not installed yet, published at another address, or unreachable from the workstation | `curl <companion URL>/healthz`; `doktunnel context set-companion` |
