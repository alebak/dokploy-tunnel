# Doktunnel (dokploy-tunnel)

> **Community project, not affiliated with Dokploy.** "Dokploy" is used only to describe compatibility.

`dokploy-tunnel` forwards services running on a [Dokploy](https://dokploy.com) server to stable local hostnames and ports, so you can reach a remote database, cache, or internal API from your machine as if it were running locally, much like the port-forward workflows of managed platforms. It has two parts:

- **`doktunnel`**: the command-line client you run on your workstation (Linux, macOS, Windows).
- **`doktunnel-companion`**: a server-side service that a Dokploy administrator installs on each Dokploy server (Linux).

## Status

**Early development.** `doktunnel` can register Dokploy panels as [contexts](#contexts), [list their services](#services), [forward them](#forward) through the companion, [show the active forwards](#status), and manage the [hosts file section](#hostnames). `doktunnel-companion` serves the tunnel endpoint, authorizes requests and reaches services through repeater containers (see [Companion](#companion-server)). Forwarding has not been verified end to end against a real Dokploy server yet. Expect breaking changes before 1.0.0.

## Install

### Linux and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/alebak/dokploy-tunnel/main/scripts/install.sh | bash
```

Installs `doktunnel` to `~/.local/bin` without `sudo`. Optional environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `DOKTUNNEL_VERSION` | latest release | Version to install, e.g. `0.1.0` |
| `DOKTUNNEL_INSTALL_DIR` | `~/.local/bin` | Install directory |

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/alebak/dokploy-tunnel/main/scripts/install.ps1 | iex
```

Installs `doktunnel.exe` to `%LOCALAPPDATA%\Programs\doktunnel` and adds it to your user `PATH`. The same `DOKTUNNEL_VERSION` and `DOKTUNNEL_INSTALL_DIR` variables apply.

Both scripts verify the SHA-256 checksum of the download against the release's `checksums.txt` before installing.

### Manual download

Download the archive for your platform from [Releases](https://github.com/alebak/dokploy-tunnel/releases), verify it against `checksums.txt`, extract it, and place `doktunnel` (or `doktunnel.exe`) on your `PATH`.

### Companion

Each release publishes a container image for linux/amd64 and linux/arm64:

```
ghcr.io/alebak/doktunnel-companion:<version>
```

`<version>` is the release version without the `v`, such as `0.5.0`; `latest` follows the newest stable release. Pin a version in deployments.

The image is based on distroless `static` and runs as its `nonroot` user (65532). It holds both server-side binaries in `/usr/local/bin`:

- `doktunnel-companion`, the default entrypoint, listening on port 8080 (see [Companion server](#companion-server)).
- `doktunnel-socket-proxy`, run from the same image with the entrypoint `doktunnel-socket-proxy` (see [Socket proxy](#socket-proxy)). To open the Docker socket, which is usually owned by `root:docker`, give its container the socket's group by numeric ID (Compose `group_add`) rather than running it as root.

The image sets `DOKTUNNEL_COMPANION_REPEATER_KEY_FILE=/var/lib/doktunnel/repeater.key` and declares `/var/lib/doktunnel` as a volume. Mount a named volume there, so the repeater key survives a recreated container.

The same two binaries are also attached to each release as `doktunnel-companion_<version>_linux_<arch>.tar.gz` archives. To install the companion and its socket proxy on a Dokploy server, follow [Install the companion on a Dokploy server](docs/install-companion.md).

## Usage

`doktunnel --help` lists the command groups: `context`, `services`, `forward`, `status`, and `hosts`. `doktunnel <command> --help` shows a command's flags, and `doktunnel --version` prints the build version.

### Contexts

A context is one Dokploy panel plus one organization in it. doktunnel authenticates with a Dokploy **API key**, and every key is bound to a single organization, so you add one context per panel and organization you work with. Logging in with a username and password is not supported.

1. In the Dokploy panel, open **Settings → API keys** and create a key, choosing the organization it belongs to. Create one dedicated key per machine, named after it, so you can revoke a single machine without affecting the others.
2. Register it:

   ```sh
   doktunnel context add --url https://dokploy.example.com --name prod
   ```

   doktunnel asks for the key with a hidden prompt, checks it against the Dokploy API, and discovers the organization's ID and name. Any scheme and port work, such as `http://192.168.1.20:3000`.

| Command | Purpose |
|---------|---------|
| `doktunnel context add --url <URL> --name <name>` | Validate an API key and register the panel and organization. The first context becomes the current one. `--companion-url <URL>` sets the [companion URL](#companion-url) when it is not the default. |
| `doktunnel context list` | List contexts with their companion URL; the current one is marked with `*`. |
| `doktunnel context use <name>` | Make a context current. |
| `doktunnel context remove <name>` | Remove a context and delete its key from the keyring. Revoke the key in Dokploy too if you no longer need it. |
| `doktunnel context set-companion <name> <URL>` | Change a context's companion URL without adding it again. |

Every other command uses the current context; `--context <name>` picks another one for a single invocation.

**Where things are stored.** The API key is stored only in the operating system's keyring: the Secret Service on Linux (GNOME Keyring, KWallet, KeePassXC), the Keychain on macOS, and the Credential Manager on Windows. It is never written to a file. The panel URL, name, and organization are kept in `doktunnel/config.json` inside your user config directory (`$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows). On a Linux machine without a Secret Service, `context add` fails rather than store the key in plain text.

**Scripts and agents.** The key is never accepted as a flag value, because flag values end up in shell history and in process listings visible to other users. Without a terminal, pass it on stdin or in the environment:

```sh
pass show dokploy/prod | doktunnel context add --url https://dokploy.example.com --name prod --api-key-stdin --json
DOKTUNNEL_API_KEY="$(pass show dokploy/prod)" doktunnel context add --url https://dokploy.example.com --name prod --no-input --json
```

**Plain HTTP.** With an `http://` URL the API key and all traffic cross the network unencrypted, and `context add` prints a warning (in `--json` mode, the warning is in the result's `warnings` list instead). Use `https://` unless the network is trusted, such as a LAN or a VPN.

<a id="companion-url"></a>**Companion URL.** Each context also stores the address of the server's [companion](#companion-server). By convention the companion is published under the panel's own address, so the default is the panel URL followed by `/doktunnel`: `https://dokploy.example.com/doktunnel`, or `http://192.168.1.20:3000/doktunnel` for a panel on a port. Once the API key is accepted, `context add` checks the companion with a `GET <companion URL>/healthz` request that carries no API key and expects `{"status":"ok"}`. If the companion does not answer (not installed yet, published elsewhere, or unreachable), the URL is stored anyway and `context add` prints a warning. Pass `--companion-url` when the companion lives at another address, or fix it later with `set-companion`, which runs the same check:

```sh
doktunnel context set-companion prod https://tunnel.example.com
```

A plain `http://` companion URL gets the same warning as a plain `http://` panel, since tunnels send the API key to the companion. Contexts added by older versions have no companion URL; `context list` shows `-` for them, and `set-companion` adds one.

**Proxies.** Requests to Dokploy and to the companion honor the `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` environment variables.

With `--json`, `add`, `use` and `set-companion` print the context as `{"name","url","organization_id","organization_name","companion_url","current"}` (plus `warnings` when there are any), `list` prints `{"current_context":"...","contexts":[...]}`, and `remove` prints `{"name":"...","removed":true}`. An invalid key fails with `permission_denied`; a panel that cannot be reached, or that does not answer like Dokploy, fails with `unreachable`.

### Services

`doktunnel services` lists the projects, environments and services the context's API key can see, grouped by project and environment, with each service's type, name, status, default port, and ID. The services inside a compose stack are listed indented under it, named `<compose>/<service>`:

```sh
doktunnel services
doktunnel services --project shop --context staging
```

```text
shop (prj_shop)
  production (default)
    TYPE               NAME            STATUS  PORT  ID
    application        web             done    -     app_web
    compose            myapp           done    -     cmp_myapp
      compose_service  myapp/postgres  -       -     cmp_myapp/postgres
      compose_service  myapp/pgadmin   -       -     cmp_myapp/pgadmin
    postgres           main-db         done    5432  pg_main
```

**Visibility.** What you see is what the user behind the API key can access. Dokploy enforces it per service, and doktunnel only calls endpoints that check the key's access to each project or service it reads; it never uses organization-wide or Docker-wide listings.

Dokploy decides what a key can see: owner and admin keys see the whole organization, member keys only the projects and services they were granted. doktunnel applies no filter of its own. For owner and admin keys, Dokploy's project list leaves out database names and statuses, so doktunnel reads them from each database (a few requests at a time); if one of those requests fails, the service is still listed by its ID and a warning is printed to stderr. `--project <name or ID>` shows only matching projects and fails with `not_found` when none matches.

The services inside a compose stack come from the compose file Dokploy has stored on the server for that stack (`compose.loadServices` with `type=cache`; doktunnel never asks Dokploy to fetch the source again). A stack the key cannot read, or one that has no compose file on the server yet because it was never deployed, is still listed, with a warning on stderr (or in its `warning` field with `--json`), and the command does not fail.

The default port is the fixed port Dokploy deploys a database with (postgres 5432, mysql and mariadb 3306, mongo 27017, redis 6379, libsql 8080). Applications, compose stacks and the services inside them listen wherever their image does, so their port is unknown and shown as `-`. Compose services also have no status of their own (`-`); see their stack's status instead.

With `--json`, the result is a stable tree:

```json
{"context":"prod","projects":[{"id":"prj_shop","name":"shop","environments":[{"id":"env_shop_prod","name":"production","default":true,"services":[{"id":"cmp_myapp","type":"compose","kind":"service","name":"myapp","status":"done","default_port":null},{"id":"cmp_myapp/postgres","type":"compose_service","kind":"compose_service","name":"myapp/postgres","status":"","default_port":null,"parent":"cmp_myapp","service":"postgres"},{"id":"pg_main","type":"postgres","kind":"service","name":"main-db","status":"done","default_port":5432}]}]}]}
```

| Field | Meaning |
|-------|---------|
| `context` | The context that was used |
| `projects[].id`, `.name` | The Dokploy project |
| `environments[].id`, `.name`, `.default` | An environment of the project; `default` marks the one Dokploy opens the project with |
| `services[].id` | The service ID, unique within its type; `<compose ID>/<service>` for a compose service |
| `services[].type` | `application`, `compose`, `postgres`, `mysql`, `mariadb`, `mongo`, `redis`, or `libsql`; `compose_service` for a service inside a compose stack |
| `services[].kind` | `service` for a service Dokploy manages, `compose_service` for a service inside a compose stack; compose services follow their stack in the list |
| `services[].name`, `.status` | Display name and deployment status (`idle`, `running`, `done`, or `error`); an empty string when unknown. A compose service is named `<compose name>/<service>` and has no status of its own |
| `services[].default_port` | Container port forwarding targets by default, or `null` when Dokploy does not define one, as for compose services |
| `services[].parent` | The ID of the compose stack a compose service belongs to; present only for compose services |
| `services[].service` | The service's name in the compose file; present only for compose services |
| `services[].warning` | What is unknown and why: `name` and `status` that could not be read, or, on a compose stack, internal services that could not be read; present only then |

Lists are always present, possibly empty. New fields may be added; existing fields keep their meaning.

### Forward

`doktunnel forward` forwards one or more services to their [hostnames](#hostnames) and real ports, and runs in the foreground until you press Ctrl+C:

```console
$ doktunnel forward myapp/postgres main-db
Updated /etc/hosts: 2 added, 0 removed.
postgres.shop-myapp-x1y2z3.internal (127.77.0.1:5432) → myapp/postgres
shop-maindb-a1b2c3.internal (127.77.0.2:5432) → main-db
Forwarding; press Ctrl+C to stop.

$ psql -h postgres.shop-myapp-x1y2z3.internal -U app   # in another terminal
```

Name services as [`services`](#services) lists them: by name, as `<compose>/<service>` for a service inside a compose stack, or by ID. IDs always win; a name shared by several services fails with `invalid_argument` listing them, so pass the ID instead. A compose stack itself is not forwardable, only the services inside it. `--project <name or ID>` limits the search to one project, `--all` forwards every service in scope, and with no service and no `--all` doktunnel asks which ones to forward (with `--no-input`, it fails with `missing_input`).

**Ports.** Each service keeps its own port on its own address, so two databases can both use 5432. The port is, in order:

1. `--port <n>`, which only applies when a single service is selected;
2. the fixed port of a Dokploy database (postgres 5432, mysql and mariadb 3306, mongo 27017, redis 6379, libsql 8080);
3. the ports the companion reports for the container, from its image's exposed ports (`GET /v1/ports`).

doktunnel never guesses: a container that exposes no port fails with `missing_input` naming `--port`, and one that exposes several (such as RabbitMQ's 5672 and 15672) fails the same way unless you pass `--all-ports` to forward each of them. With `--all`, every selected service must resolve; narrow the selection or name the services otherwise.

**Addresses and hosts file.** Each service is leased its address in the [address registry](#hostnames), its appName is read (one details call per service or compose stack whose appName `doktunnel` does not know yet) and recorded, and the hosts file is synced exactly as `doktunnel hosts sync` does: it asks for administrator privileges once, and only when something changed. On macOS the same step adds the `lo0` aliases, which are lost on reboot. With `--no-input` and a change to make, `forward` fails with `elevation_required`, and the hint is the command to run first. Listeners are bound to the leased address and port only, never `0.0.0.0`; if any of them cannot be bound (for example, the service is already forwarded by another `doktunnel forward`), nothing is forwarded.

**Connections.** Every local connection opens its own tunnel through the context's [companion](#companion-url). Connection events go to stderr, one line each. Ctrl+C or `SIGTERM` (on Windows, Ctrl+C, Ctrl+Break or closing the console) closes every open tunnel, so the companion removes its repeaters once their grace period ends, and exits with code 0.

**Stopping.** On the way out, `forward` also removes its hostnames from the hosts file, except those another running `forward` still uses, so the section always lists the services being forwarded. If the hosts file needs administrator privileges, it asks for them again, as it did on start (on Linux and macOS, `sudo` may still remember your password). With `--no-input` or without a terminal it does not ask; if it may not, you refuse, or nobody answers within a minute, the entries stay and a warning tells you to run `doktunnel hosts sync` (which removes the names of stopped forwards) or `doktunnel hosts clean`. Either way the exit code stays 0. Leases are kept, so a service gets the same address and hostname the next time, and so are the macOS `lo0` aliases.

With `--json`, `forward` prints one object once every forward listens, and nothing else afterwards:

```json
{"context":"prod","pid":4242,"started_at":"2026-10-06T12:00:00Z","companion_url":"https://dokploy.example.com/doktunnel","forwards":[{"target":{"type":"compose_service","id":"cmp_myapp/postgres","name":"myapp/postgres"},"hostname":"postgres.shop-myapp-x1y2z3.internal","ip":"127.77.0.1","port":5432}]}
```

`target.type` is the Dokploy service type or `compose_service`, and `target.id` and `target.name` are as `services` reports them. While it runs, each `forward` process records the same data in `forwards/<pid>.json` in the doktunnel state directory (next to the address registry), which [`doktunnel status`](#status) lists; the file is removed on exit.

### Status

`doktunnel status` lists the forwards of every running `doktunnel forward` process, from any terminal:

```console
$ doktunnel status
CONTEXT  HOSTNAME                             ADDRESS          TARGET          PID   SINCE
prod     postgres.shop-myapp-x1y2z3.internal  127.77.0.1:5432  myapp/postgres  4242  2026-10-06 07:00:00
prod     shop-maindb-a1b2c3.internal          127.77.0.2:5432  main-db         4242  2026-10-06 07:00:00
```

Rows are sorted by context, hostname and port, and `SINCE` is when the process started forwarding, in local time. With no running forward it prints `No active forwards.` to stderr. `status` shows every context unless `--context <name>` is given explicitly; the current context does not filter it. It needs no network access and no configured context.

It reads the files `forward` processes keep in `forwards/<pid>.json` in the doktunnel state directory. A file whose process is no longer running (for example, one that was killed) is stale and removed. A file of a running process that cannot be read, such as one written by another doktunnel version, is left in place and reported as a warning on stderr; it does not fail the command. doktunnel cannot portably tell a killed `forward` process from an unrelated program that later got the same PID, so such a stale file is listed until that program exits.

With `--json`:

```json
{"forwards":[{"pid":4242,"started_at":"2026-10-06T12:00:00Z","context":"prod","companion_url":"https://dokploy.example.com/doktunnel","target":{"type":"postgres","id":"pg_main","name":"main-db"},"hostname":"shop-maindb-a1b2c3.internal","ip":"127.77.0.2","port":5432}],"warnings":[]}
```

| Field | Meaning |
|-------|---------|
| `forwards[].pid`, `.started_at` | The `forward` process and when it started forwarding (RFC 3339) |
| `forwards[].context`, `.companion_url` | The context and companion the process uses |
| `forwards[].target`, `.hostname`, `.ip`, `.port` | The forward, as `forward --json` prints it |
| `warnings` | Files that could not be read or removed, one string each |

Both lists are always present, possibly empty. New fields may be added; existing fields keep their meaning.

### Hostnames

Every forwarded service gets its own loopback address from `127.77.0.0/16` and a stable hostname under `.internal`, a top-level domain reserved for private use:

| Target | Hostname |
|--------|----------|
| Application or database | `<appName>.internal`, e.g. `acme-postgres-a1b2c3.internal` |
| Service inside a compose stack | `<service>.<appName>.internal`, e.g. `postgres.acme-billing-x1y2z3.internal` |

The **appName** is the name Dokploy deploys an application, database or compose stack under: the one shown in the panel, which Dokploy builds from the name you chose plus 6 random characters, and also the directory of a compose stack in `/etc/dokploy/compose/<appName>/`. `<service>` is the service name in the compose file. doktunnel reads the appName from the service's details when you forward it; if they cannot be read (for example, the API key may not read that service's details), the hostname uses the service's Dokploy ID instead, and `forward` prints a warning.

**Labels.** Each part is lowercased, every character other than `a`–`z` and `0`–`9` (including the dots and underscores an appName may hold, spaces and accented letters) becomes a hyphen, repeated hyphens collapse, and leading and trailing hyphens are dropped, so `Acme_Billing.x1y2z3` becomes `acme-billing-x1y2z3`. A part longer than 63 characters is cut and ends in a short hash of the full name; a name with no usable character becomes `x-` and a hash. Whole hostnames never exceed 253 characters.

**Collisions.** appNames are unique within one Dokploy instance, but two contexts (two panels) can both hold one, and two names can differ only in characters that are turned into hyphens. doktunnel never maps two services to one name: the service registered first keeps the plain hostname, and a later one gets its context as an extra label, as in `postgres.acme-billing-x1y2z3.acme-staging.internal`. When that is taken too, its first label also gets a suffix of 6 hex characters derived from its Dokploy instance, organization and service ID, as in `postgres-1a2b3c.acme-billing-x1y2z3.acme-staging.internal` (longer when that is taken as well). The result is deterministic, and an existing hostname never changes when a newer service with the same name appears. doktunnel never uses names other software resolves under `.internal`, such as `host.docker.internal` or `metadata.google.internal`, nor any name below `docker.internal`, `containers.internal`, `google.internal`, `lima.internal`, `orb.internal` or `rancher-desktop.internal`; such a name is disambiguated the same way.

**Upgrading from hostnames built from display names.** doktunnel versions before this one named services `<service>.<project>.<org>.<context>.internal`. Upgrading keeps every service's address; the next `forward` of a service records its appName, and the sync it runs replaces the old names in the hosts file with the names of the running forwards; a plain `hosts sync` does the same.

**The hosts file section.** doktunnel writes the hostnames to the system hosts file (`/etc/hosts` on Linux and macOS, `%SystemRoot%\System32\drivers\etc\hosts` on Windows) inside one marked block:

```text
# BEGIN doktunnel (managed block, do not edit; remove with 'doktunnel hosts clean')
127.77.0.1	postgres.acme-billing-x1y2z3.internal
127.77.0.2	acme-postgres-a1b2c3.internal
# END doktunnel
```

Only that block is ever parsed and rewritten: every other line stays byte for byte, the block uses the line ending the file already has (CRLF on Windows), and the file is replaced atomically where the system allows it. If the markers are malformed (a begin without an end, an end without a begin, or two blocks), doktunnel refuses to edit the file and asks you to run `doktunnel hosts clean`.

```sh
doktunnel hosts sync --dry-run   # show what would change
doktunnel hosts sync             # write the section, only if it changed
doktunnel hosts list             # the entries currently in the section
doktunnel hosts clean            # remove the section, nothing else
```

`hosts sync` builds the section from the forwards running now: it holds the hostnames of the services every running `doktunnel forward` process listens for, as recorded in their `forwards/<pid>.json` files, and nothing else, so names of stopped forwards are removed, and with no forward running the section is removed too. It writes the file only when it differs. `forward` runs the same sync when it starts and when it stops. A `forward` process that was killed cannot remove its names; the next sync does. A state file of a running process that cannot be read (written by another doktunnel version) keeps no names. `hosts clean` also repairs malformed markers: a begin and end pair is removed with everything between them, and a marker without a partner is removed alone.

With `--json`, `hosts sync` prints `{"hosts_file","dry_run","changed","added","removed","aliases"}`, `hosts list` prints `{"hosts_file","entries"}` and `hosts clean` prints `{"hosts_file","changed","removed"}`, where every entry is `{"ip","hostname"}` and `aliases` lists the macOS loopback aliases added (or, with `--dry-run`, to add):

```json
{"hosts_file":"/etc/hosts","entries":[{"ip":"127.77.0.1","hostname":"postgres.acme-billing-x1y2z3.internal"}]}
```

**Administrator privileges.** The hosts file belongs to root (Administrator on Windows), but doktunnel never runs as root: your API keys live in *your* OS keyring, and your config and address registry in *your* home directory, which a root process would not see. Instead, when the section must change, doktunnel re-runs only the privileged step, the same binary with an internal helper command that accepts nothing but addresses in `127.77.0.0/16` and doktunnel's own `.internal` hostnames (never `host.docker.internal` and the other names listed under **Collisions**), and never echoes what it rejects:

- **Linux and macOS:** through `sudo`, which asks for your password on the terminal; the new entries reach the helper on its standard input, so root never opens a file you point it at.
- **macOS** also needs every address added to `lo0` (`/sbin/ifconfig lo0 alias <ip> up`); the aliases are lost on reboot, so `hosts sync` checks them and re-adds missing ones in the same step.
- **Windows:** through a UAC prompt (PowerShell `Start-Process -Verb RunAs`). UAC cannot pass a standard input, so the entries go in a file in your state directory, which the helper reads only if it is a regular file, not a link.

The hosts file is rewritten in place rather than replaced, so its SELinux label, extended attributes and Windows ACL are kept.

When nothing changed, nothing is elevated. With `--no-input`, or when stdin is not a terminal, doktunnel never prompts: it fails with `elevation_required`, and the hint is the exact command to run yourself. The entries wait in a file in your state directory, which your own shell redirects into the helper, for example:

```text
doktunnel: updating /etc/hosts needs administrator privileges, and prompting is not allowed [elevation_required]
hint: run: sudo /usr/local/bin/doktunnel hosts privileged-apply --entries-file - < /home/me/.local/state/doktunnel/pending-hosts
```

### Global flags

Every command accepts:

| Flag | Purpose |
|------|---------|
| `--json` | Print machine-readable JSON on stdout, including errors. |
| `--no-input` | Never prompt for missing values; fail with `missing_input` naming the flag to pass. Implied when stdin is not a terminal. |
| `--context <name>` | Use the named context instead of the current one. |

### Errors and exit codes

Every error has a stable code with its own exit code, so scripts and agents can branch on either. Codes never change meaning; new codes may be added.

| Exit code | Error code | Meaning |
|-----------|------------|---------|
| 0 | — | Success |
| 1 | `internal` | Unexpected failure |
| 2 | `invalid_argument` | Unknown or malformed command, flag, or argument |
| 3 | `missing_input` | A required value is missing and prompting is not allowed |
| 4 | `not_implemented` | The command exists but is not implemented yet |
| 5 | `permission_denied` | The caller lacks permission for the operation |
| 6 | `network_not_attachable` | The target network cannot be attached |
| 7 | `elevation_required` | The operation needs administrator privileges |
| 8 | `unreachable` | A remote endpoint could not be reached |
| 9 | `not_found` | A named resource, such as a project or service, does not exist or is not visible |

Without `--json`, errors are printed to stderr as `doktunnel: <message> [<code>]`, followed by a `hint:` line when there is one.

With `--json`, the error is a single JSON object on **stdout** and nothing is written to stderr, so a caller parses one stream and uses the exit code to tell a result from an error:

```json
{"code":"missing_input","message":"missing value for --context","hint":"pass --context <value>"}
```

`hint` is omitted when empty.

### Agent skill

`doktunnel --skill` prints a `SKILL.md` that teaches AI agents how to use the CLI: commands, global flags, output conventions, error codes, and safe workflows. It is generated from the installed binary, so it always matches it; it needs no context or network, and ignores `--json`.

Install it into your agent's skills directory, and regenerate it after upgrading doktunnel:

```bash
# Claude Code
mkdir -p ~/.claude/skills/doktunnel
doktunnel --skill > ~/.claude/skills/doktunnel/SKILL.md

# Any agent that reads SKILL.md files
doktunnel --skill > <agent skills dir>/doktunnel/SKILL.md
```

## Companion server

`doktunnel-companion` is the server-side half of dokploy-tunnel. A Dokploy administrator installs one on each Dokploy server whose services should be reachable: on the Dokploy server itself, and on each remote server Dokploy deploys to. `doktunnel` opens one WebSocket to it for every local TCP connection; the [wire protocol](docs/protocol.md) is documented separately.

> **Install:** see [Install the companion on a Dokploy server](docs/install-companion.md): one Dokploy compose service with the companion and its [socket proxy](#socket-proxy), published at `<panel-domain>/doktunnel`.

It is configured with flags or environment variables; flags win:

| Flag | Environment variable | Default | Purpose |
|------|----------------------|---------|---------|
| `--dokploy-url` | `DOKTUNNEL_COMPANION_DOKPLOY_URL` | required | URL of the Dokploy panel, as reachable from the companion, such as `http://dokploy:3000` |
| `--server-id` | `DOKTUNNEL_COMPANION_SERVER_ID` | empty | ID of the Dokploy server the companion runs on; empty or `local` for the Dokploy server itself |
| `--listen` | `DOKTUNNEL_COMPANION_LISTEN` | `:8080` | TCP address to serve on |
| `--bridge` | `DOKTUNNEL_COMPANION_BRIDGE` | `docker` | `docker` forwards through repeater containers; `none` refuses every authorized tunnel with `target_unreachable` |
| `--docker-host` | `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon: `unix:///path` or plain `tcp://host:port`, such as a socket proxy |
| `--repeater-image` | `DOKTUNNEL_COMPANION_REPEATER_IMAGE` | `alpine/socat:1.8.1.1@sha256:7f9a…` | Repeater image; it must provide `socat` and `sleep` |
| `--repeater-grace` | `DOKTUNNEL_COMPANION_REPEATER_GRACE` | `30s` | How long a repeater outlives its last tunnel |
| `--reaper-ttl` | `DOKTUNNEL_COMPANION_REAPER_TTL` | `1m` | Age after which an orphaned repeater is removed |
| `--max-tunnels` | `DOKTUNNEL_COMPANION_MAX_TUNNELS` | `512` | Tunnels open at once; more are refused with `too_many_tunnels` |
| `--max-tunnels-per-key` | `DOKTUNNEL_COMPANION_MAX_TUNNELS_PER_KEY` | `64` | Tunnels one API key may have open at once |
| `--max-repeaters` | `DOKTUNNEL_COMPANION_MAX_REPEATERS` | `128` | Repeater containers running at once; idle ones are removed early to make room |
| `--repeater-key-file` | `DOKTUNNEL_COMPANION_REPEATER_KEY_FILE` | `doktunnel-companion/repeater.key` in the user's configuration directory | Key that proves which repeaters this companion created; generated on first start if missing. Keep it on persistent storage, such as a volume when the companion runs in a container |
| `--version` | | | Print the version and exit |

`GET /healthz` answers `200` with `{"status":"ok"}` while the companion accepts tunnels. On `SIGINT` or `SIGTERM` it stops accepting tunnels, closes open ones with the WebSocket "going away" code, and exits within 30 seconds. Logs are written to stderr; they never contain API keys.

The companion serves plain HTTP. Put it behind a TLS-terminating proxy, such as the Traefik instance Dokploy already runs, because every tunnel request carries the caller's API key.

### How it reaches services

Docker networks are segmented, so the companion never joins tenant networks itself. With the `docker` bridge it talks to the Docker Engine API (the companion exits at startup if Docker does not answer) and, for each target, runs one idle **repeater** container from a small socat image:

- The target is found from sources tied to the service Dokploy authorized, never from names or labels another tenant could copy. Applications, databases and services of `stack` deployments are Swarm services: the companion inspects the service by its exact name (`<appName>`, or `<appName>_<service>` with `com.docker.stack.namespace=<appName>` on the service) and takes a running task's container, networks and addresses from the Swarm API. Services of `docker-compose` stacks are found by their `com.docker.compose.project` and `com.docker.compose.service` labels, which Compose sets itself, and only accepted when their project directory is Dokploy's for that appName (`/etc/dokploy/compose/<appName>`). Containers carrying any `com.docker.swarm.*` label are refused there, and containers that match but disagree on their project or networks make the tunnel fail rather than guess.
- Some targets are refused with `target_unreachable`: appNames `dokploy` and `dokploy-*`, which Dokploy's own services use while users may pick such names too, and containers that can reach the host: privileged, sharing the host's network, PID, IPC or UTS namespace, given host devices or GPUs, adding capabilities such as `SYS_ADMIN` or `NET_ADMIN`, setting kernel parameters outside their own namespaces, running with a weakened security profile (`seccomp=unconfined`, `apparmor=unconfined`, `label=disable` or `systempaths=unconfined`, also in the legacy `key:value` form), or mounting a daemon socket, a directory that may hold one, a daemon's data root (`/var/lib/docker`, `/var/lib/containerd`, `~/.local/share/docker`, or the custom `data-root` the Docker daemon reports in `/info`) or a host device, directly or through a `local` volume with bind options. Swarm services are also judged by their spec, which covers tasks on other nodes, including its `Privileges` (seccomp `unconfined`, AppArmor `disabled`, SELinux labeling disabled). The data root is read again at most once a minute; if the daemon cannot report it, every target is refused with `target_unreachable` rather than judged without it.
- The repeater joins exactly one of the target's own networks, preferring the stack's own network over custom networks over `dokploy-network`. It listens on no port, runs as `nobody` with no capabilities, a read-only root filesystem, an init, and limits of 256 processes and 64 MiB of memory, and is labeled `dev.doktunnel.repeater=1`. A target reachable only on overlay networks that are not attachable is refused with `network_not_attachable`.
- Every tunnel runs `socat STDIO TCP:<ip>:<port>` in the repeater through `docker exec`, dialing the address Docker reports for the target on that network, so no DNS name is involved.
- Each dial is bound to the target's identity, not only to its address, because Docker may hand the address of a stopped container to another one on the same network. Just before socat runs, the companion checks that the container it resolved still runs and holds that address, and records its start time and network endpoint, which Docker renews on every start and reconnection. Once socat has connected, and before the tunnel carries any byte, it checks them again and closes the tunnel with `target_unreachable` if they changed: an unchanged container held the address throughout, so the connection reached it. A Swarm task on another node cannot be inspected and is checked by its task ID and address instead; tasks never restart, so this holds as long as the Swarm manager's view is current, which can lag the node by a few seconds. That lag is the remaining window.
- Concurrent tunnels to one target share its repeater, which is removed when the last tunnel has been closed for the grace period. Repeaters left behind by a stopped companion are removed at startup and every minute once older than the reaper TTL, without their volumes. Since anyone who can create containers can copy a repeater's label, name and image, the companion also labels each repeater with `dev.doktunnel.ownership`, an HMAC-SHA256 of its name keyed by the repeater key, and only removes containers whose label verifies with that key; look-alikes are kept and logged once. The key outlives restarts in the repeater key file. If the default location is unusable, the companion warns and uses a key of its own process, so repeaters left behind before a restart are kept instead of removed; a key file given explicitly must be usable or the companion exits.

The companion therefore needs Docker access, and the Docker socket is root-equivalent on the host. Give it the [socket proxy](#socket-proxy) instead of the socket. The companion warns at startup when `--docker-host` is a `tcp://` address outside the loopback interface: keep such an endpoint on a network only the companion joins.

### Socket proxy

`doktunnel-socket-proxy` holds the Docker socket in place of the companion and forwards only the Docker API calls the companion makes, so a compromised companion cannot run arbitrary containers. Run it as its own service on a network only the companion joins, and point the companion at it with `--docker-host tcp://<proxy>:2375`. It serves plain HTTP with no authentication; never publish its port.

| Flag | Environment variable | Default | Purpose |
|------|----------------------|---------|---------|
| `--listen` | `DOKTUNNEL_SOCKET_PROXY_LISTEN` | `:2375` | TCP address to serve on |
| `--docker-host` | `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon: `unix:///path` or plain `tcp://host:port` |
| `--repeater-image` | `DOKTUNNEL_SOCKET_PROXY_REPEATER_IMAGE` | `alpine/socat:1.8.1.1@sha256:7f9a…` | The only image repeaters may run; it must equal the companion's `--repeater-image` |
| `--version` | | | Print the version and exit |

Allowed calls, with or without a `/vX.Y` version prefix:

| Call | Restrictions |
|------|--------------|
| `GET`/`HEAD /_ping`, `GET /info` | No query parameters |
| `GET /containers/json` | Only `all` and `filters` with `label` keys |
| `GET /containers/{id}/json`, `/networks/{id}`, `/volumes/{name}`, `/services/{id}`, `/tasks/{id}` | No query parameters |
| `GET /tasks` | Only `filters` with `service` and `desired-state` keys |
| `POST /containers/create` | Name `doktunnel-repeater-*`; the body must be exactly a repeater's: the repeater image running `sleep infinity` as `65534:65534`, the repeater labels and no others, and a host configuration of one network (not `host`, `none` or `container:*`), a read-only root filesystem, `CapDrop: ALL`, `no-new-privileges`, an init, and limits of at most 256 processes and 64 MiB. The network is inspected through the daemon: a missing one gets the daemon's `404`, one with the `host` or `null` driver is refused under any name or ID, and the container is created on its full ID. Unknown fields are refused at every level, and the checked configuration is re-encoded before it is forwarded |
| `POST /images/create` | Only `fromImage` and `tag` of the repeater image |
| `POST /containers/{id}/start`, `DELETE /containers/{id}` | Only containers labeled `dev.doktunnel.repeater=1`, named `doktunnel-repeater-*`, running the repeater image and confined like a repeater (unprivileged, as `65534:65534`, on a network rather than `host`, `none` or `container:*`, with `CapDrop: ALL` and no added capabilities, `no-new-privileges`, a read-only root filesystem and no binds or mounts), as the daemon reports them; removal takes only `force`, never `v` |
| `POST /containers/{id}/exec` | Same containers; the command must be `socat -d -d STDIO TCP:<ip>:<port>,connect-timeout=<seconds>` with a literal IP address, attached, without a TTY |
| `POST /exec/{id}/start` | Only execs created through the proxy, once, attached and upgraded to a raw stream |

Every other call is refused with `403` and a Docker-style `{"message": "..."}` body, and logged. A client gets 10 seconds to send a request's headers, 30 more for its body, and 2 minutes between requests on a kept-alive connection; exec streams and the daemon's answers are not bounded. Errors from the daemon are passed through unchanged. Whoever reaches the proxy can still read every container's configuration, including its environment variables, and connect to any address on the networks repeaters join.

### Security and permissions

The companion holds no credentials and keeps no permissions of its own. Every tunnel request carries the caller's own Dokploy API key, and the companion asks the Dokploy API, with that key, whether it can read the target service (`<type>.one`, such as `postgres.one`; for a service inside a compose stack, `compose.one` and then `compose.loadServices` with `type=cache` to check the service exists). Only if Dokploy answers is the tunnel opened. It never calls organization-wide or Docker-wide Dokploy endpoints.

**Read access to a service is enough to forward to it.** Dokploy's own container terminal, which opens a root shell inside the container, only requires read access to the service (`canAccessDockerOverWss` in Dokploy's `apps/dokploy/server/wss/authorize.ts`). A TCP tunnel grants less than a shell, so requiring more would be stricter than Dokploy without a real security gain. Owner and admin keys can forward to every service in their organization; member keys only to the services they were granted.

Invalid keys, keys of another organization, services the key cannot read, and services that do not exist are all rejected alike, with `permission_denied`, before anything else happens. A companion only forwards to services deployed on its own Dokploy server and refuses others with `wrong_server`, naming the server the service runs on.

### Trust model

**doktunnel's isolation between tenants is bounded by Dokploy's.** Anyone who can edit a Dokploy compose service can already control the host's Docker daemon, so the companion cannot protect a server from them. Grant compose edit permissions only to users you would trust with root on that server.

What the companion verifies for every tunnel:

| Check | What it guarantees |
|-------|--------------------|
| Dokploy authorization | The caller's API key can read the target service, checked against Dokploy on every tunnel request (see [Security and permissions](#security-and-permissions)). |
| Authoritative target resolution | Swarm targets come from the Swarm API by exact service name. Compose targets must carry Compose's own `com.docker.compose.project` and `com.docker.compose.service` labels, no `com.docker.swarm.*` label, and a project directory (or compose files) under `/etc/dokploy/compose/<appName>`. |
| Reserved names | AppNames `dokploy` and `dokploy-*` are refused, so a tunnel cannot reach Dokploy's own panel, database, cache or proxy. |
| Unsafe targets | Containers that run privileged, use the host's network or PID namespace, are given host devices, add capabilities that reach past the container, or mount a daemon socket (or a directory that may hold one) are refused. |
| Dialing | The repeater dials the IP address the Docker daemon reports for the target, never a DNS name. |
| Resource limits | Repeaters run as `nobody` with no capabilities, a read-only root filesystem and limits of 256 processes and 64 MiB; tunnels are capped globally and per API key. |

The compose check relies on two facts that hold under Dokploy's default deployment:

- Docker Compose sets `com.docker.compose.project`, `service`, `working_dir` and `config_files` itself ([`pkg/compose/loader.go`](https://github.com/docker/compose/blob/main/pkg/compose/loader.go)) and merges them over the service's own labels ([`pkg/compose/executor_ops.go`](https://github.com/docker/compose/blob/main/pkg/compose/executor_ops.go)), so a compose file cannot forge them.
- Dokploy deploys a compose service with `docker compose -p <appName> --project-directory <path> ... up -d --build --remove-orphans` ([`packages/server/src/utils/builders/compose.ts`](https://github.com/Dokploy/dokploy/blob/canary/packages/server/src/utils/builders/compose.ts)).

**Dokploy's custom compose command breaks both.** A compose service can replace the default command with its own. Dokploy only rejects shell metacharacters in it (`sanitizeCommand` in the same file) and runs it as `docker <command>`. A user who can edit that field can therefore deploy under another project name or directory, or run any `docker` command, such as a privileged container with the host's root filesystem mounted. No check in the companion can stop that, because the user never needed a tunnel to reach the host.

## Releases and versioning

- Versions follow [Semantic Versioning](https://semver.org) and are derived from [Conventional Commits](https://www.conventionalcommits.org) by [release-please](https://github.com/googleapis/release-please).
- Merges to `main` keep a release pull request up to date with the next version and the generated `CHANGELOG.md`.
- Merging that pull request tags the release; [GoReleaser](https://goreleaser.com) then builds the binaries, attaches the archives and `checksums.txt` to the GitHub release, and pushes the companion image to `ghcr.io/alebak/doktunnel-companion`.
- Before 1.0.0, breaking changes bump the minor version.

## Platform verification

The `platform` workflow (`.github/workflows/platform.yml`) checks on GitHub's Linux, macOS and Windows runners what unit tests can only fake:

- **Loopback addresses:** listeners on addresses in `127.77.0.0/16` bind and accept connections natively on Linux and Windows, and on macOS only after `ifconfig lo0 alias <ip> up` (the probe adds and removes the aliases).
- **Hosts file round trip:** the real `doktunnel` binary runs `hosts sync` against the runner's hosts file with real privileges (sudo on Linux and macOS, on a pseudo-terminal as a person would; on Windows the runner is already an elevated Administrator, so the file is written directly), the entries resolve through `getent`, `dscacheutil` or `Resolve-DnsName` and accept connections by name, and `hosts clean` leaves the unrelated lines around the block byte for byte. On Windows it also runs the privileged helper through a real UAC elevation (`Start-Process -Verb RunAs`) to check that it completes unattended.
- **Install scripts:** GoReleaser builds snapshot release artifacts on the runner, which are served from `127.0.0.1`; `install.sh` (Linux, macOS) and `install.ps1` (Windows, under PowerShell 7 and Windows PowerShell 5.1) must refuse an archive whose checksum does not match, then install the snapshot, and the installed `doktunnel --version` must report it. The scripts read the local URL from `DOKTUNNEL_TEST_DOWNLOAD_BASE`, a test-only variable that accepts nothing but `http://127.0.0.1:<port>` or `http://localhost:<port>` and that installing doktunnel never needs.

A disproved assumption shows up as a *Platform finding* warning on the run and in its job summary. These tests live in `internal/platformtest`, need the `platform` build tag, and skip unless `DOKTUNNEL_PLATFORM_TESTS=1` is set on GitHub Actions: they run sudo and rewrite the system hosts file, so never run them on your own machine.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
