# Contributing to dokploy-tunnel

Thanks for your interest in contributing. dokploy-tunnel is a community project and is not affiliated with Dokploy.

## Before you start

| You want to | Do this |
|-------------|---------|
| Report a security vulnerability | Follow [SECURITY.md](SECURITY.md). Never open a public issue for it. |
| Add a feature or make a larger change | [Open an issue](https://github.com/alebak/dokploy-tunnel/issues/new/choose) first, using the issue forms, and agree on the approach. |
| Fix a typo, a doc or a small bug | Open a pull request directly. |
| Report a problem with Dokploy itself | Report it [upstream](https://github.com/Dokploy/dokploy/issues). |

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

### Tests that only run in CI

Unit tests (`go test ./...`) run everywhere and are all you need locally. Two suites sit behind build tags and only run in CI:

| Suite | Build tag | What it touches | Locally |
|-------|-----------|-----------------|---------|
| Platform | `platform` | The real hosts file (with sudo or Administrator) and loopback addresses | Never run it. It also skips unless `DOKTUNNEL_PLATFORM_TESTS=1` on GitHub Actions. |
| Docker integration | `integration` | A real Docker daemon; it creates containers and needs `DOKTUNNEL_INTEGRATION=1` | Do not run it against your own daemon unless you accept that it creates and removes containers there. |

You can still compile-check both:

```sh
go vet -tags platform ./internal/platformtest/
go vet -tags integration ./...
```

### Agent skill golden file

`internal/cli/testdata/skill.golden` holds the expected `doktunnel --skill` output. When you add or change commands, flags or error codes, regenerate it and review the diff before committing:

```sh
go test ./internal/cli -run TestRun_SkillGolden -update
git diff internal/cli/testdata/skill.golden
```

### Wire protocol

The contract between `doktunnel` and `doktunnel-companion` is [docs/protocol.md](docs/protocol.md). Any change to how they talk to each other must update it in the same pull request.

### Release archives

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
| `docs` | Documentation only | none |
| `refactor` | Code restructuring without behavior change | patch bump |
| `test` | Adding or updating tests | none |
| `build` | Build system or dependencies | none |
| `ci` | CI configuration | none |
| `chore` | Maintenance | none |
| `style` | Formatting only | none |

A breaking change is marked with `!` after the type (`feat!: ...`) or a `BREAKING CHANGE:` footer. Before 1.0.0 it bumps the minor version. Types marked "none" are hidden from the changelog and do not trigger a release on their own.

Write commit messages in English, with a lowercase subject and no trailing period. Do not add AI attribution or `Co-Authored-By` trailers for tools.

### Pull requests

1. Fork the repository and create a branch named as above.
2. Commit, then push the branch to your fork.
3. Open a pull request against `main` and fill in the template.

`main` is protected: changes land only through pull requests, with linear history and required checks, and nobody can bypass that.

- Pull requests are **squash-merged** by a maintainer. The PR title becomes the commit message on `main`, so it must be a valid Conventional Commit; a workflow checks this.
- Keep each PR focused on one logical change.
- CI must pass: `go vet`, `go build`, and `go test` on Linux, macOS, and Windows, plus the platform, Docker integration, image, GoReleaser and PR title checks.
- If this is your first contribution, a maintainer must approve the workflows before CI runs on your pull request. Expect a short delay.

## How releases happen

1. Every push to `main` runs [release-please](https://github.com/googleapis/release-please), which opens or updates a release PR with the next version and the generated `CHANGELOG.md` entries.
2. A maintainer merges the release PR when ready to ship.
3. release-please tags the commit (`vX.Y.Z`) and creates the GitHub release.
4. In the same workflow run, [GoReleaser](https://goreleaser.com) builds `doktunnel`, `doktunnel-companion` and `doktunnel-socket-proxy`, uploads the archives and `checksums.txt`, publishes the multi-arch `ghcr.io/alebak/doktunnel-companion` image with the release version tag (and `latest` for non-prereleases), and appends download and install instructions to the release notes.

Do not edit `CHANGELOG.md` or create tags by hand.

## Code standards

See [AGENTS.md](AGENTS.md) for the code conventions shared by human and AI contributors.
