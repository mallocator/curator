# curator: agent instructions

Go service that cleans up and archives a self-hosted media library after watching (README.md has the behaviour and
every setting). Runs as the image `mallox/curator` on the owner's NAS; the stack lives in his private ops repo.

## Commands
- Check (the hook runs it on commit, CI on every Go change): `golangci-lint run`. Fix formatting: `golangci-lint fmt`.
- Full suite (hook on push, CI): `go test -race ./...`. One test: `go test -run TestName ./...`.
- Image: `docker build -t curator .`, then `docker run` it and `docker exec <container> /app/curator healthcheck`.
- Release: push a `vX.Y.Z` tag; CI tests, builds binaries and the multi-platform image with the tag as the version.
  The NAS pulls `latest` on its weekly update. A tag publishes: only with the owner's OK.

## Differences from the global standards
- Public repo, self-contained (owner 2026-11-09): no paths into the owner's Mac, no secrets. Configs
  (.golangci.yml, .gitleaks.toml) are copies from his standards repo; CI is the gate, local hooks stay untracked
  (`.git/hooks`).
- Layout: one flat `package main`, one file per concern with its `_test.go` beside it, not `pkg/`. Moving to
  `pkg/` is the owner's call.
- Commit titles name the version for releases: `vX.Y.Z: <what changed>`.

## Rules
- The binary stays at `/app/curator`: the NAS compose healthcheck runs `/app/curator healthcheck`, and a moved
  binary makes the watcher restart the container in a loop.
- Nothing is ever deleted outside `DELETE_ROOTS` (guard.go): the archive folders are the owner's and only he
  deletes there.
- Every client has its own `http.Client` timeout; requests carry a context (noctx). There is no shutdown context
  yet, so they use `context.Background()`.
- Never commit a built binary: `curator` and the old `plex-clean` (the name until v2.14) are ignored; an 11 MB
  `plex-clean` was tracked until 2026-11-10 and is still in the history.
