# dokploy-tunnel

> **Community project, not affiliated with Dokploy.** "Dokploy" is used only to describe compatibility.

`dokploy-tunnel` forwards services running on a [Dokploy](https://dokploy.com) server to stable local hostnames and ports, so you can reach a remote database, cache, or internal API from your machine as if it were running locally, much like the port-forward workflows of managed platforms. It has two parts:

- **`doktunnel`**: the command-line client you run on your workstation (Linux, macOS, Windows).
- **`doktunnel-companion`**: a server-side service that a Dokploy administrator installs on each Dokploy server (Linux).

## Status

**Early development.** The release pipeline is in place, but the binaries do not implement tunneling yet. `doktunnel` can register Dokploy panels as [contexts](#contexts) and [list their services](#services); its other commands report `not_implemented`. Expect breaking changes before 1.0.0.

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

`doktunnel-companion` archives are attached to each release for Linux amd64 and arm64. A supported installation method (a container image) will be documented once the companion is functional.

## Usage

`doktunnel --help` lists the command groups: `context`, `services`, `forward`, `status`, and `hosts`. Only `context` and `services` are implemented so far; the others exit with the `not_implemented` error. `doktunnel <command> --help` shows a command's flags, and `doktunnel --version` prints the build version.

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

`doktunnel services` lists the projects, environments and services the context's API key can see, grouped by project and environment, with each service's type, name, status, default port, and ID:

```sh
doktunnel services
doktunnel services --project shop --context staging
```

```text
shop (prj_shop)
  production (default)
    TYPE         NAME           STATUS   PORT  ID
    application  web            done     -     app_web
    postgres     main-db        done     5432  pg_main
```

Dokploy decides what a key can see: owner and admin keys see the whole organization, member keys only the projects and services they were granted. doktunnel applies no filter of its own. `--project <name or ID>` shows only matching projects and fails with `not_found` when none matches.

The default port is the fixed port Dokploy deploys a database with (postgres 5432, mysql and mariadb 3306, mongo 27017, redis 6379, libsql 8080). Applications and compose services listen wherever their image does, so their port is unknown and shown as `-`.

With `--json`, the result is a stable tree:

```json
{"context":"prod","projects":[{"id":"prj_shop","name":"shop","environments":[{"id":"env_shop_prod","name":"production","default":true,"services":[{"id":"pg_main","type":"postgres","name":"main-db","status":"done","default_port":5432}]}]}]}
```

| Field | Meaning |
|-------|---------|
| `context` | The context that was used |
| `projects[].id`, `.name` | The Dokploy project |
| `environments[].id`, `.name`, `.default` | An environment of the project; `default` marks the one Dokploy opens the project with |
| `services[].id` | The service ID, unique within its type |
| `services[].type` | `application`, `compose`, `postgres`, `mysql`, `mariadb`, `mongo`, `redis`, or `libsql` |
| `services[].name`, `.status` | Display name and deployment status (`idle`, `running`, `done`, or `error`); an empty string when unknown |
| `services[].default_port` | Container port forwarding targets by default, or `null` when Dokploy does not define one |

Lists are always present, possibly empty. New fields may be added; existing fields keep their meaning.

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

## Releases and versioning

- Versions follow [Semantic Versioning](https://semver.org) and are derived from [Conventional Commits](https://www.conventionalcommits.org) by [release-please](https://github.com/googleapis/release-please).
- Merges to `main` keep a release pull request up to date with the next version and the generated `CHANGELOG.md`.
- Merging that pull request tags the release; [GoReleaser](https://goreleaser.com) then builds both binaries and attaches the archives and `checksums.txt` to the GitHub release.
- Before 1.0.0, breaking changes bump the minor version.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
