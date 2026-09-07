# Internals

[Back to Myriad](../README.md)

Myriad is a Linux-native lifecycle coordinator shipped as one Go binary. It
requires no Python runtime, shell wrapper, or daemon installation.

## Agent integration

Myriad launches agents and validation commands directly with argv. Validation
commands are tokenized once and are never evaluated by a shell. Agent hook
commands follow the upstream CLI's shell contract and use quoted absolute paths.

### Codex

Interactive Codex uses a private App Server over a Unix socket. A trusted
first-prompt hook asks `gpt-5.6-luna` at medium reasoning for one constrained
semantic slug, uses it for the branch and active Codex thread title, and
provisions the reserved worktree synchronously before the prompt reaches the
model. The hook runtime is an immutable, content-addressed copy of the running
binary. Managed Codex keeps its transcript in normal terminal scrollback while
Myriad suppresses only the standard disconnect, reconnect, elapsed-time, and
token-usage tail on exit. The saved Codex thread remains available to resume.

Claude uses a session plugin for [work activity](activity.md#supported-sessions).

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
