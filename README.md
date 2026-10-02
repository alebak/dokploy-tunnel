# dokploy-tunnel

> **Community project, not affiliated with Dokploy.** "Dokploy" is used only to describe compatibility.

`dokploy-tunnel` forwards services running on a [Dokploy](https://dokploy.com) server to stable local hostnames and ports, so you can reach a remote database, cache, or internal API from your machine as if it were running locally, much like the port-forward workflows of managed platforms. It has two parts:

- **`doktunnel`**: the command-line client you run on your workstation (Linux, macOS, Windows).
- **`doktunnel-companion`**: a server-side service that a Dokploy administrator installs on each Dokploy server (Linux).

## Status

**Early development.** The release pipeline is in place, but the binaries do not implement tunneling yet; they only report their version (`--version`). Expect breaking changes before 1.0.0.

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

## Releases and versioning

- Versions follow [Semantic Versioning](https://semver.org) and are derived from [Conventional Commits](https://www.conventionalcommits.org) by [release-please](https://github.com/googleapis/release-please).
- Merges to `main` keep a release pull request up to date with the next version and the generated `CHANGELOG.md`.
- Merging that pull request tags the release; [GoReleaser](https://goreleaser.com) then builds both binaries and attaches the archives and `checksums.txt` to the GitHub release.
- Before 1.0.0, breaking changes bump the minor version.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
