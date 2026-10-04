# dokploy-tunnel

> **Community project, not affiliated with Dokploy.** "Dokploy" is used only to describe compatibility.

`dokploy-tunnel` forwards services running on a [Dokploy](https://dokploy.com) server to stable local hostnames and ports, so you can reach a remote database, cache, or internal API from your machine as if it were running locally, much like the port-forward workflows of managed platforms. It has two parts:

- **`doktunnel`**: the command-line client you run on your workstation (Linux, macOS, Windows).
- **`doktunnel-companion`**: a server-side service that a Dokploy administrator installs on each Dokploy server (Linux).

## Status

**Early development.** The release pipeline is in place, but the binaries do not implement tunneling yet. `doktunnel` can register Dokploy panels as [contexts](#contexts), [list their services](#services) and manage the [hosts file section](#hostnames); its other commands report `not_implemented`. `doktunnel-companion` serves the tunnel endpoint and authorizes requests, but cannot reach services yet (see [Companion](#companion-server)). Expect breaking changes before 1.0.0.

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

`doktunnel-companion` archives are attached to each release for Linux amd64 and arm64. A supported installation method (a container image) will be documented once the companion is functional; see [Companion server](#companion-server) for what it does and how it is configured.

## Usage

`doktunnel --help` lists the command groups: `context`, `services`, `forward`, `status`, and `hosts`. Only `context`, `services` and `hosts` are implemented so far; the others exit with the `not_implemented` error. `doktunnel <command> --help` shows a command's flags, and `doktunnel --version` prints the build version.

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
| `doktunnel context add --url <URL> --name <name>` | Validate an API key and register the panel and organization. The first context becomes the current one. |
| `doktunnel context list` | List contexts; the current one is marked with `*`. |
| `doktunnel context use <name>` | Make a context current. |
| `doktunnel context remove <name>` | Remove a context and delete its key from the keyring. Revoke the key in Dokploy too if you no longer need it. |

Every other command uses the current context; `--context <name>` picks another one for a single invocation.

**Where things are stored.** The API key is stored only in the operating system's keyring: the Secret Service on Linux (GNOME Keyring, KWallet, KeePassXC), the Keychain on macOS, and the Credential Manager on Windows. It is never written to a file. The panel URL, name, and organization are kept in `doktunnel/config.json` inside your user config directory (`$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows). On a Linux machine without a Secret Service, `context add` fails rather than store the key in plain text.

**Scripts and agents.** The key is never accepted as a flag value, because flag values end up in shell history and in process listings visible to other users. Without a terminal, pass it on stdin or in the environment:

```sh
pass show dokploy/prod | doktunnel context add --url https://dokploy.example.com --name prod --api-key-stdin --json
DOKTUNNEL_API_KEY="$(pass show dokploy/prod)" doktunnel context add --url https://dokploy.example.com --name prod --no-input --json
```

**Plain HTTP.** With an `http://` URL the API key and all traffic cross the network unencrypted, and `context add` prints a warning (in `--json` mode, the warning is in the result's `warnings` list instead). Use `https://` unless the network is trusted, such as a LAN or a VPN.

**Proxies.** Requests to Dokploy honor the `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` environment variables.

With `--json`, `add` and `use` print the context as `{"name","url","organization_id","organization_name","current"}` (plus `warnings` when there are any), `list` prints `{"current_context":"...","contexts":[...]}`, and `remove` prints `{"name":"...","removed":true}`. An invalid key fails with `permission_denied`; a panel that cannot be reached, or that does not answer like Dokploy, fails with `unreachable`.

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

### Hostnames

Every forwarded service gets its own loopback address from `127.77.0.0/16` and a stable hostname under `.internal`, a top-level domain reserved for private use:

| Target | Hostname |
|--------|----------|
| Dokploy service | `<service>.<project>.<org>.<context>.internal`, e.g. `main-db.shop.acme.prod.internal` |
| Service inside a compose stack | `<service>.<compose>.<project>.<org>.<context>.internal`, e.g. `postgres.myapp.shop.acme.prod.internal` |

**Labels.** Each part is built from a display name: lowercased, every character other than `a`–`z` and `0`–`9` (including dots, spaces and accented letters) becomes a hyphen, repeated hyphens collapse, and leading and trailing hyphens are dropped, so `Main DB` becomes `main-db`. A part longer than 63 characters is cut and ends in a short hash of the full name; a name with no usable character becomes `x-` and a hash. Whole hostnames never exceed 253 characters.

**Collisions.** Two services can end up with the same hostname, for example `Main DB` and `main_db`, or a service with the same name in two environments of one project. doktunnel never maps two services to one name: the service registered first keeps the plain hostname, and each later one gets a suffix of 6 hex characters derived from its Dokploy instance, organization and service ID, as in `main-db-1a2b3c.shop.acme.prod.internal` (longer when that is taken too). The result is deterministic, and an existing hostname never changes when a newer service with the same name appears.

**The hosts file section.** doktunnel writes the hostnames to the system hosts file (`/etc/hosts` on Linux and macOS, `%SystemRoot%\System32\drivers\etc\hosts` on Windows) inside one marked block:

```text
# BEGIN doktunnel (managed block, do not edit; remove with 'doktunnel hosts clean')
127.77.0.1	postgres.myapp.shop.acme.prod.internal
127.77.0.2	main-db.shop.acme.prod.internal
# END doktunnel
```

Only that block is ever parsed and rewritten: every other line stays byte for byte, the block uses the line ending the file already has (CRLF on Windows), and the file is replaced atomically where the system allows it. If the markers are malformed (a begin without an end, an end without a begin, or two blocks), doktunnel refuses to edit the file and asks you to run `doktunnel hosts clean`.

```sh
doktunnel hosts sync --dry-run   # show what would change
doktunnel hosts sync             # write the section, only if it changed
doktunnel hosts list             # the entries currently in the section
doktunnel hosts clean            # remove the section, nothing else
```

`hosts sync` builds the section from the services registered in the address registry and writes it only when it differs from the file; services are registered when you forward them. `hosts clean` also repairs malformed markers: a begin and end pair is removed with everything between them, and a marker without a partner is removed alone.

With `--json`, `hosts sync` prints `{"hosts_file","dry_run","changed","added","removed","aliases"}`, `hosts list` prints `{"hosts_file","entries"}` and `hosts clean` prints `{"hosts_file","changed","removed"}`, where every entry is `{"ip","hostname"}` and `aliases` lists the macOS loopback aliases added (or, with `--dry-run`, to add):

```json
{"hosts_file":"/etc/hosts","entries":[{"ip":"127.77.0.1","hostname":"postgres.myapp.shop.acme.prod.internal"}]}
```

**Administrator privileges.** The hosts file belongs to root (Administrator on Windows), but doktunnel never runs as root: your API keys live in *your* OS keyring, and your config and address registry in *your* home directory, which a root process would not see. Instead, when the section must change, doktunnel re-runs only the privileged step, the same binary with an internal helper command that reads the new entries from a file in its state directory and accepts nothing but loopback addresses and `.internal` hostnames:

- **Linux and macOS:** through `sudo`, which asks for your password.
- **macOS** also needs every address added to `lo0` (`ifconfig lo0 alias <ip> up`); the aliases are lost on reboot, so `hosts sync` checks them and re-adds missing ones in the same step.
- **Windows:** through a UAC prompt (PowerShell `Start-Process -Verb RunAs`).

When nothing changed, nothing is elevated. With `--no-input`, or when stdin is not a terminal, doktunnel never prompts: it fails with `elevation_required`, and the hint is the exact command to run yourself, for example:

```text
doktunnel: updating /etc/hosts needs administrator privileges, and prompting is not allowed [elevation_required]
hint: run: sudo /usr/local/bin/doktunnel hosts privileged-apply --entries-file /home/me/.local/state/doktunnel/pending-hosts
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

> **Work in progress.** The companion authorizes tunnels, but forwarding to services is not implemented yet: authorized requests are refused with `target_unreachable`. A supported installation method will be documented once it is functional.

It is configured with flags or environment variables; flags win:

| Flag | Environment variable | Default | Purpose |
|------|----------------------|---------|---------|
| `--dokploy-url` | `DOKTUNNEL_COMPANION_DOKPLOY_URL` | required | URL of the Dokploy panel, as reachable from the companion, such as `http://dokploy:3000` |
| `--server-id` | `DOKTUNNEL_COMPANION_SERVER_ID` | empty | ID of the Dokploy server the companion runs on; empty or `local` for the Dokploy server itself |
| `--listen` | `DOKTUNNEL_COMPANION_LISTEN` | `:8080` | TCP address to serve on |
| `--version` | | | Print the version and exit |

`GET /healthz` answers `200` with `{"status":"ok"}` while the companion accepts tunnels. On `SIGINT` or `SIGTERM` it stops accepting tunnels, closes open ones with the WebSocket "going away" code, and exits within 30 seconds. Logs are written to stderr; they never contain API keys.

The companion serves plain HTTP. Put it behind a TLS-terminating proxy, such as the Traefik instance Dokploy already runs, because every tunnel request carries the caller's API key.

### Security and permissions

The companion holds no credentials and keeps no permissions of its own. Every tunnel request carries the caller's own Dokploy API key, and the companion asks the Dokploy API, with that key, whether it can read the target service (`<type>.one`, such as `postgres.one`; for a service inside a compose stack, `compose.one` and then `compose.loadServices` with `type=cache` to check the service exists). Only if Dokploy answers is the tunnel opened. It never calls organization-wide or Docker-wide Dokploy endpoints.

**Read access to a service is enough to forward to it.** Dokploy's own container terminal, which opens a root shell inside the container, only requires read access to the service (`canAccessDockerOverWss` in Dokploy's `apps/dokploy/server/wss/authorize.ts`). A TCP tunnel grants less than a shell, so requiring more would be stricter than Dokploy without a real security gain. Owner and admin keys can forward to every service in their organization; member keys only to the services they were granted.

Invalid keys, keys of another organization, services the key cannot read, and services that do not exist are all rejected alike, with `permission_denied`, before anything else happens. A companion only forwards to services deployed on its own Dokploy server and refuses others with `wrong_server`, naming the server the service runs on.

## Releases and versioning

- Versions follow [Semantic Versioning](https://semver.org) and are derived from [Conventional Commits](https://www.conventionalcommits.org) by [release-please](https://github.com/googleapis/release-please).
- Merges to `main` keep a release pull request up to date with the next version and the generated `CHANGELOG.md`.
- Merging that pull request tags the release; [GoReleaser](https://goreleaser.com) then builds both binaries and attaches the archives and `checksums.txt` to the GitHub release.
- Before 1.0.0, breaking changes bump the minor version.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
