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

Myriad launches agents and validation commands directly with argv. Validation
commands are tokenized once and are never evaluated by a shell. Agent hook
commands follow the upstream CLI's shell contract and use quoted absolute paths.

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
first-prompt hook asks `gpt-5.6-luna` at medium reasoning for one constrained
semantic slug, uses it for the branch and active Codex thread title, and
provisions the reserved worktree synchronously before the prompt reaches the
model. The hook runtime is an immutable, content-addressed copy of the running
binary. Managed Codex keeps its transcript in normal terminal scrollback while
Myriad suppresses only the standard disconnect, reconnect, elapsed-time, and
token-usage tail on exit. The saved Codex thread remains available to resume.

## Lifecycle commands

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

Checks are passed as repeatable `--check` values:

```console
myriad start --agent custom \
  --check 'go test ./...' \
  --check 'go vet ./...' \
  -- ./my-agent
```

## Work activity

Live managed sessions share declared work summaries and observed changed paths
through their private Myriad state directory. Peers match the same Git common
directory, including repositories joined through `myriad attach`; separate
clones or state directories do not exchange activity. New sessions learn what
is already underway, and existing sessions receive new or changed summaries.
Overlapping file or directory scopes get an explicit overlap notice. Related
work in different files remains visible through repository-wide summaries.

Myriad installs automatic hooks for interactive Codex sessions using its private
App Server and for managed Claude sessions with hooks enabled. Noninteractive
Codex and direct launches do not receive this hook setup. Claude receives a
[session plugin](https://code.claude.com/docs/en/plugins#test-your-plugins-locally)
through `--plugin-dir`, preserving the caller's settings and arguments. Hook
runtimes are pinned at launch; already-running sessions retain their runtime.

The prompt hook asks the agent to announce its intent before editing and update
it when scope changes. This is an internal agent step:

```console
myriad activity --summary 'Fix token refresh' --path internal/auth
```

Run the command inside the session's managed worktree or an attached worktree.
Paths are relative to that repository's root, even from a subdirectory; repeat
`--path` for more files or directories. The summary is agent-declared, with the
task's branch name used until the first declaration. Myriad makes no additional
model calls for activity sharing and does not copy prompts, transcripts, or file
contents to peers. Declaring directory scopes makes intent visible before edits
appear.

The [Codex](https://learn.chatgpt.com/docs/hooks#posttooluse) and
[Claude](https://code.claude.com/docs/en/hooks#posttooluse) hooks refresh paths
and add notices at the next `UserPromptSubmit` or `PostToolUse` boundary; Claude's
`PostToolUse` runs after successful tools. Long-running tools or reasoning can
delay receipt. Tool hooks reuse the session's file scan for two seconds after
it completes. Prompts, `Stop`, and explicit activity commands force a refresh;
`Stop` publishes the final snapshot without delivering or blocking on a notice.

Observed paths are net differences between the task's current base commit and
its worktree or index, plus nonignored untracked files. This includes committed,
staged, and unstaged changes, but is not a history of every touched file.
`.ai-memory` is excluded; tracked files still count even if an ignore rule
matches them. Each stored path list is capped at 256 paths and 32 KiB of path
text. Oversized declarations are rejected with a request to use directory
scopes. Notice previews show up to 12 paths per list and counts from the stored
lists. `paths_truncated` indicates missing paths in either session's observation,
the overlap result, or the preview; counts may therefore be lower bounds.

Inbox synchronization coalesces each peer task to its latest activity, suppresses
unchanged notices, and removes departed peers on the next synchronization.
Each delivery includes at most eight notices. Output failures leave notices
pending for a later hook or activity command. `myriad inbox` reads the stored
snapshot without refreshing it. Custom agents can publish and receive notices
through `myriad activity`; automatic receipt requires the hook integration.

Work notices are advisory data. They grant no exclusive ownership and cannot
prevent Git or semantic conflicts. They never request a handoff, start an idle
turn, or veto turn completion.

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
validation, integration, recovery, live Codex App Server RPC, attached
repositories, and work activity delivery. When Claude is installed, its native
plugin validator also checks the generated activity plugin.
