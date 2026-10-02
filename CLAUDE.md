# CLAUDE.md — dokploy-tunnel

dokploy-tunnel forwards Dokploy services to stable local hostnames and ports. Community project, not affiliated with Dokploy.

## Project rules

**Read the full code standards in `AGENTS.md`.** Key rules:

- Idiomatic Go, stdlib first; no new dependency without justification.
- All artifacts (code, comments, docs, commits) in English.
- Conventional Commits: `feat(scope):`, `fix(scope):`, `docs:`, etc. No AI attribution or `Co-Authored-By` trailers.
- PRs are squash-merged; the PR title must be a Conventional Commit.
- Never edit `CHANGELOG.md` or create tags by hand; release-please owns them.

## Layout

```
cmd/doktunnel/            CLI entrypoint (Linux, macOS, Windows)
cmd/doktunnel-companion/  Server-side companion entrypoint (Linux)
internal/version/         Build metadata injected by GoReleaser ldflags
scripts/                  install.sh / install.ps1 (doktunnel only)
```

## Commands

```bash
go vet ./...
go build ./...
go test ./...
goreleaser check
goreleaser release --snapshot --clean
```
