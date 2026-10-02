# AGENTS.md — dokploy-tunnel Code Standards

## Project context

dokploy-tunnel forwards services running on Dokploy servers to stable local hostnames and ports. It ships two binaries: `doktunnel` (CLI for Linux, macOS, and Windows on amd64/arm64) and `doktunnel-companion` (server-side service for Linux amd64/arm64, installed by a Dokploy administrator). It is a community project and not affiliated with Dokploy.

Code clarity and idiomatic patterns take priority over clever abstractions.

## Go

- Follow Effective Go and the Go Code Review Comments guide; code must be `gofmt`-clean.
- Prefer the standard library. Adding a dependency requires a clear justification.
- No global mutable state, except link-time build metadata in `internal/version`.
- Every exported identifier has a godoc comment starting with its name.
- Handle every error; wrap with context: `fmt.Errorf("dialing companion: %w", err)`.
- No `panic()` outside `main()`.
- `cmd/*/main.go` only wires things together; logic lives in `internal/`.
- Code must build and behave correctly on Linux, macOS, and Windows (CI runs all three). Use `filepath`, `os.UserConfigDir()`, and similar instead of hardcoded paths.

## Testing

- Tests live next to the code (`foo.go` and `foo_test.go`) and use the standard `testing` package.
- Use table-driven tests; name them `TestFunction_Scenario`.
- Prefer real implementations with test data over mocks.

## Commits and pull requests

- Conventional Commits in English: `feat`, `fix`, `perf`, `revert`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `style`.
- One logical change per commit. No AI attribution or `Co-Authored-By` trailers.
- PRs are squash-merged; the PR title is the commit message and is validated in CI.
- Releases are automated by release-please and GoReleaser; do not hand-edit `CHANGELOG.md`, `.release-please-manifest.json`, or tags.

## Security

- Never build shell commands from unvalidated input.
- Treat all data from the network (Dokploy API, companion) as untrusted and validate it.
- No hardcoded secrets, tokens, or hostnames.
- Install scripts must verify checksums before installing anything.
