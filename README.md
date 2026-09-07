# Myriad

Run Codex and Claude Code in isolated Git worktrees.

Myriad sets up each task, preserves interrupted work, and integrates completed
commits locally. Supported sessions share work summaries and flag overlapping files.

## Quick start

Requires Linux, Git, Go 1.24+ to install, and Codex CLI or Claude Code.

```sh
go install github.com/dongho-jung/myriad/cmd/myriad@latest
cd /path/to/repo
myriad codex  # or: myriad claude
```

## Documentation

- [Usage](docs/usage.md) — commands, recovery, and validation.
- [Session activity](docs/activity.md) — work summaries and overlap notices.
- [Internals](docs/internals.md) — agent integration, safety, and state.
- [Development](docs/development.md) — building and testing.
