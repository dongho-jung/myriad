# Work activity

[Back to Myriad](../README.md)

Live managed sessions share declared work summaries and observed changed paths
through their private Myriad state directory. Peers match the same Git common
directory, including repositories joined through `myriad attach`; separate
clones or state directories do not exchange activity. New sessions learn what
is already underway, and existing sessions receive new or changed summaries.
Overlapping file or directory scopes get an explicit overlap notice. Related
work in different files remains visible through repository-wide summaries.

## Supported sessions

Myriad installs automatic hooks for interactive Codex sessions using its private
App Server and for managed Claude sessions with hooks enabled. Noninteractive
Codex and direct launches do not receive this hook setup. Claude receives a
[session plugin](https://code.claude.com/docs/en/plugins#test-your-plugins-locally)
through `--plugin-dir`, preserving the caller's settings and arguments. Hook
runtimes are pinned at launch; already-running sessions retain their runtime.

## Announce work

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

## Delivery timing

The [Codex](https://learn.chatgpt.com/docs/hooks#posttooluse) and
[Claude](https://code.claude.com/docs/en/hooks#posttooluse) hooks refresh paths
and add notices at the next `UserPromptSubmit` or `PostToolUse` boundary; Claude's
`PostToolUse` runs after successful tools. Long-running tools or reasoning can
delay receipt. Tool hooks reuse the session's file scan for two seconds after
it completes. Prompts, `Stop`, and explicit activity commands force a refresh;
`Stop` publishes the final snapshot without delivering or blocking on a notice.

## File scopes and limits

Observed paths are net differences between the task's current base commit and
its worktree or index, plus nonignored untracked files. This includes committed,
staged, and unstaged changes, but is not a history of every touched file.
`.ai-memory` is excluded; tracked files still count even if an ignore rule
matches them. Each stored path list is capped at 256 paths and 32 KiB of path
text. Oversized declarations are rejected with a request to use directory
scopes. Notice previews show up to 12 paths per list and counts from the stored
lists. `paths_truncated` indicates missing paths in either session's observation,
the overlap result, or the preview; counts may therefore be lower bounds.

## Inbox behavior

Inbox synchronization coalesces each peer task to its latest activity, suppresses
unchanged notices, and removes departed peers on the next synchronization.
Each delivery includes at most eight notices. Output failures leave notices
pending for a later hook or activity command. `myriad inbox` reads the stored
snapshot without refreshing it. Custom agents can publish and receive notices
through `myriad activity`; automatic receipt requires the hook integration.

Work notices are advisory data. They grant no exclusive ownership and cannot
prevent Git or semantic conflicts. They never request a handoff, start an idle
turn, or veto turn completion.
