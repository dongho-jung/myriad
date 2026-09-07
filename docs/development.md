# Development

[Back to Myriad](../README.md)

## Build

Requires Linux, Git, and Go 1.24 or newer.

For a reproducible local build:

```console
go build -trimpath \
  -ldflags '-s -w -X github.com/dongho-jung/myriad/internal/myriad.Version=VERSION' \
  -o myriad ./cmd/myriad
```

## Test

```console
go test ./...
go test -race ./internal/myriad
go vet ./...
```

The integration tests create real temporary repositories and exercise worktree
creation, concurrent session metadata, process-tree cleanup, candidate
validation, integration, recovery, live Codex App Server RPC, attached
repositories, and work activity delivery. When Claude is installed, its native
plugin validator also checks the generated activity plugin.
