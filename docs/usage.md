# Usage

[Back to Myriad](../README.md)

## Launch an agent

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

## Commands

```text
myriad open [OPTIONS] [TASK]        resume or create managed work
myriad start [OPTIONS] [TASK]       create a managed task directly
myriad resume [OPTIONS] [SESSION]   recover work or resume a Codex chat
myriad publish [TASK_ID]            publish a live committed checkpoint
myriad attach PATH                  attach a second repository to this session
myriad context --jira KEY...        set display-only Jira contexts
myriad context --pr NUMBER...       set display-only pull-request contexts
myriad activity --summary TEXT     share work intent; repeat --path PATH
myriad list                         list tasks
myriad status [TASK_ID]             inspect task state
myriad diagnose TASK_ID             dump refs, processes, blockers, and logs
myriad inbox                        inspect work and integration notices
myriad handoff EVENT_ID             release a lease for queued integration
myriad integrate TASK_ID            retry local integration
myriad recover TASK_ID              recover preserved work
myriad cleanup TASK_ID|--all        remove safe inactive worktrees
myriad reconcile [--quiet]          repair interrupted lifecycle state
myriad statusline [--claude]        render active task status
```

## Integration

An integration blocked by an active repository session remains queued and is
retried automatically after that session exits. Myriad does not interrupt the
foreground agent or request a handoff merely to advance the target sooner.

`myriad publish` replays non-conflicting committed work onto an advanced target
without operator involvement. If that replay has a content conflict, Myriad
prepares the same history-safe replay directly in the active managed worktree
and tells the agent which paths need judgment. The agent can resolve and add
those paths and rerun `myriad publish`; Myriad continues the prepared rebase and
reports the next conflict, if any. The target remains unchanged until the
completed replay passes validation and the normal publish checks. Rewritten
target history is not reintroduced by a merge commit.

## Diagnostics

Task records retain bounded lifecycle, integration, publish, and validation
histories. Validation entries include the command, process identity, outcome,
timeout state, and bounded stdout/stderr tails. `myriad diagnose TASK_ID`
combines those records with live ref ancestry, worktree state, active sessions,
and `/proc` process state for postmortem debugging.

## Attachments

`myriad attach` must be called from an active managed session before modifying
another Git repository. It returns a separate managed worktree, commit history,
validation path, and integration result for that repository.

## Display context

Jira and pull-request display context is private task runtime state under the
Myriad state directory, not repository memory. Each command accepts
space-separated values, preserves their order, and removes duplicates. The
stored context uses `jira_issues` and `pull_request_numbers` arrays; explicit
clears store empty arrays to suppress task-description inference. Context records
accept only schema metadata and these arrays; other layouts are rejected without
conversion.

Every Codex TUI launched by Myriad includes Codex's native `thread-title` status
item. Direct chats use Codex's generated title; a managed fresh chat replaces Codex's
provisional prompt prefix with the same Luna-generated semantic branch title as
soon as its checkout is provisioned. Later managed title changes are coalesced,
and Myriad writes the final Jira group and branch name after the TUI exits. The
next resume or thread listing therefore sees a title such as
`[COM-12 CER-42] fix-login -> main` without injecting repeated `Session renamed`
notices into the active transcript. Myriad also enables Codex's native
`pull-request-number` status item beside it: when Codex discovers an open pull
request for the current checkout, it renders that PR separately as a clickable
terminal hyperlink. Claude and the operator-facing Myriad status line continue
to show the full Jira and PR groups through `myriad statusline --claude`.

## Validation

Checks are passed as repeatable `--check` values:

```console
myriad start --agent custom \
  --check 'go test ./...' \
  --check 'go vet ./...' \
  -- ./my-agent
```
