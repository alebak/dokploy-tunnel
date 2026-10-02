# Contributing to dokploy-tunnel

Thanks for your interest in contributing. dokploy-tunnel is a community project and is not affiliated with Dokploy.

## Getting started

### Prerequisites

- Go (the version declared in `go.mod`)
- Git

A ready-to-use [Dev Container](.devcontainer/devcontainer.json) with Go, GoReleaser, and actionlint is included.

### Build, vet, and test

```sh
go vet ./...
go build ./...
go test ./...
go run ./cmd/doktunnel --version
```

To build release archives locally without publishing:

```sh
goreleaser release --snapshot --clean
```

## Development workflow

### Branch naming

```
<type>/<short-description>
```

Use the commit types below as prefixes, for example `feat/port-mapping` or `fix/reconnect-loop`. Lowercase, hyphen-separated.

### Commit convention

This project uses [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<optional-scope>): <description>
```

| Type | Use for | Release effect (before 1.0.0) |
|------|---------|-------------------------------|
| `feat` | New user-facing functionality | minor bump |
| `fix` | Bug fixes | patch bump |
| `perf` | Performance improvements | patch bump |
| `revert` | Reverting a previous commit | patch bump |
| `docs` | Documentation only | patch bump |
| `refactor` | Code restructuring without behavior change | patch bump |
| `test` | Adding or updating tests | none |
| `build` | Build system or dependencies | none |
| `ci` | CI configuration | none |
| `chore` | Maintenance | none |
| `style` | Formatting only | none |

A breaking change is marked with `!` after the type (`feat!: ...`) or a `BREAKING CHANGE:` footer. Before 1.0.0 it bumps the minor version. Types marked "none" are hidden from the changelog and do not trigger a release on their own.

Write commit messages in English, with a lowercase subject and no trailing period. Do not add AI attribution or `Co-Authored-By` trailers for tools.

### Pull requests

- Pull requests are **squash-merged**. The PR title becomes the commit message on `main`, so it must be a valid Conventional Commit; a workflow checks this.
- Keep each PR focused on one logical change.
- CI must pass: `go vet`, `go build`, and `go test` on Linux, macOS, and Windows.

## How releases happen

1. Every push to `main` runs [release-please](https://github.com/googleapis/release-please), which opens or updates a release PR with the next version and the generated `CHANGELOG.md` entries.
2. A maintainer merges the release PR when ready to ship.
3. release-please tags the commit (`vX.Y.Z`) and creates the GitHub release.
4. In the same workflow run, [GoReleaser](https://goreleaser.com) builds `doktunnel` and `doktunnel-companion`, uploads the archives and `checksums.txt`, and appends download and install instructions to the release notes.

Do not edit `CHANGELOG.md` or create tags by hand.

## Code standards

See [AGENTS.md](AGENTS.md) for the code conventions shared by human and AI contributors.
