# dokploy-tunnel

> **Community project, not affiliated with Dokploy.** "Dokploy" is used only to describe compatibility.

`dokploy-tunnel` forwards services running on a [Dokploy](https://dokploy.com) server to stable local hostnames and ports, so you can reach a remote database, cache, or internal API from your machine as if it were running locally, much like the port-forward workflows of managed platforms. It has two parts:

- **`doktunnel`**: the command-line client you run on your workstation (Linux, macOS, Windows).
- **`doktunnel-companion`**: a server-side service that a Dokploy administrator installs on each Dokploy server (Linux).

## Status

**Early development.** The release pipeline is in place, but the binaries do not implement tunneling yet; `doktunnel` has its command tree and error model in place, and its commands report `not_implemented`. Expect breaking changes before 1.0.0.

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

`doktunnel --help` lists the command groups: `context`, `services`, `forward`, `status`, and `hosts`. They are not implemented yet and exit with the `not_implemented` error. `doktunnel --version` prints the build version.

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
