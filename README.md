# Myriad

Myriad is a Linux-native lifecycle coordinator for running coding agents in
isolated Git worktrees. It is shipped as one Go binary: there is no Python
runtime, shell wrapper, daemon installation, or state-format compatibility
layer.

The ordinary `myriad codex` and `myriad claude` entry points reserve a checkout,
create a task from the repository's integration target, supervise the complete
process tree, validate the committed result, and integrate it back locally.
Interrupted or unsafe results are preserved for recovery instead of being
discarded.

## Requirements

- Linux
- Git
- Go 1.24 or newer to build
- Codex CLI and/or Claude Code, depending on the launcher used

At runtime Myriad invokes commands directly with argv. Validation commands are
tokenized once and are never evaluated by a shell.

## Install

```console
go install github.com/dongho-jung/myriad/cmd/myriad@latest
```

For a reproducible local build:

```console
go build -trimpath \
  -ldflags '-s -w -X github.com/dongho-jung/myriad/internal/myriad.Version=VERSION' \
  -o myriad ./cmd/myriad
```

## Launchers

```console
myriad codex [ARGS...]
myriad claude [ARGS...]
```

Inside a Git repository, an ordinary interactive launch creates or resumes a
managed task rooted at the integration target. Use `--new` for a distinct task
and `--local` only when the current checkout is deliberately required. Review
commands reserve the exact current checkout. Claude's background, cloud, tmux,
remote-control, teleport, and built-in worktree modes remain owned by Claude and
run directly.

Interactive Codex uses a private App Server over a Unix socket. A trusted
first-prompt hook chooses a constrained semantic branch name and provisions the
reserved worktree synchronously before the prompt reaches the model. The hook
runtime is an immutable, content-addressed copy of the running binary.

## Lifecycle commands

```text
myriad open [OPTIONS] [TASK]        resume or create managed work
myriad start [OPTIONS] [TASK]       create a managed task directly
myriad resume [OPTIONS] [SESSION]   recover work or resume a Codex chat
myriad publish [TASK_ID]            publish a live committed checkpoint
myriad attach PATH                  attach a second repository to this session
myriad context --jira KEY...        set display-only Jira contexts
myriad context --pr NUMBER...       set display-only pull-request contexts
myriad list                         list tasks
myriad status [TASK_ID]             inspect task state
myriad diagnose TASK_ID             dump refs, processes, blockers, and logs
myriad inbox                        inspect integration handoff notices
myriad handoff EVENT_ID             release a lease for queued integration
myriad integrate TASK_ID            retry local integration
myriad recover TASK_ID              recover preserved work
myriad cleanup TASK_ID|--all        remove safe inactive worktrees
myriad reconcile [--quiet]          repair interrupted lifecycle state
myriad statusline [--claude]        render active task status
```

An integration blocked by an active repository session remains queued and is
retried automatically after that session exits. Myriad does not interrupt the
foreground agent or request a handoff merely to advance the target sooner.

Task records retain bounded lifecycle, integration, publish, and validation
histories. Validation entries include the command, process identity, outcome,
timeout state, and bounded stdout/stderr tails. `myriad diagnose TASK_ID`
combines those records with live ref ancestry, worktree state, active sessions,
and `/proc` process state for postmortem debugging.

`myriad attach` must be called from an active managed session before modifying
another Git repository. It returns a separate managed worktree, commit history,
validation path, and integration result for that repository.

Jira and pull-request display context is private task runtime state under the
Myriad state directory, not repository memory. Each command accepts
space-separated values, preserves their order, and removes duplicates. Every
Codex TUI launched by Myriad includes Codex's native `thread-title` status item,
so direct and managed fresh chats show their generated titles as soon as Codex
names them. While a managed Codex TUI is connected, Myriad coalesces its own
title changes and writes the final Jira group and branch name after the TUI
exits. The next resume or thread listing therefore sees a title such as
`[COM-12 CER-42] fix-login -> main` without injecting extra `Session renamed`
notices into the active transcript. Myriad also enables Codex's native
`pull-request-number` status item beside it: when Codex discovers an open pull
request for the current checkout, it renders that PR separately as a clickable
terminal hyperlink. Claude and the operator-facing Myriad status line continue
to show the full Jira and PR groups through `myriad statusline --claude`.

Checks are passed as repeatable `--check` values:

```console
myriad start --agent custom \
  --check 'go test ./...' \
  --check 'go vet ./...' \
  -- ./my-agent
```

## Safety model

- A stable per-checkout `flock` prevents two owned sessions from sharing a
  checkout.
- A Linux child-subreaper retains the lease until detached descendants have
  terminated and been reaped.
- Integration validates an isolated candidate and rechecks target ancestry,
  checkout topology, cleanliness, and target identity before advancing a ref.
- Validation must leave candidate HEAD and tracked files unchanged.
- `.ai-memory` is machine-local and forbidden from every new result commit. A
  valid memory proposal is merged only after its code result integrates, or
  after a successful task with no code changes.
- Worktrees with uncommitted data, failed checks, unexpected history, or an
  interrupted launcher remain recoverable.

Myriad coordinates Git checkouts only. It does not isolate filesystems,
networks, databases, deployment targets, ports, queues, or cloud resources.

## State and environment

Current state is stored under `~/.local/state/myriad` with mode `0700`. Set
`MYRIAD_STATE_DIR` to use another private root. Myriad accepts only its current
schema and does not read or migrate older layouts.

Managed agents receive:

- `MYRIAD_HARNESS=myriad`
- `MYRIAD_TASK_ID`, `MYRIAD_TASK_TITLE`
- `MYRIAD_WORKTREE`, `MYRIAD_WORKDIR`
- `MYRIAD_BRANCH`, `MYRIAD_TARGET_BRANCH`
- `MYRIAD_REPO_MEMORY`, `MYRIAD_MEMORY_SOURCE`

These variables describe lifecycle context; they are not a security boundary.

## Development

```console
go test ./...
go test -race ./internal/myriad
go vet ./...
```

The integration tests create real temporary repositories and exercise worktree
creation, concurrent session metadata, process-tree cleanup, candidate
validation, integration, recovery, live Codex App Server RPC, and attached
repositories.
