package myriad

import "time"

const (
	StatusCreated     = "CREATED"
	StatusRunning     = "RUNNING"
	StatusReady       = "READY_TO_INTEGRATE"
	StatusIntegrating = "INTEGRATING"
	StatusValidating  = "VALIDATING"
	StatusIntegrated  = "INTEGRATED"
	StatusCompleted   = "COMPLETED_NO_CHANGES"
	StatusFailed      = "FAILED"
	StatusRecovery    = "RECOVERY_REQUIRED"

	MemoryName       = ".ai-memory"
	TaskRecordSchema = 1
	SessionSchema    = 1
	InboxSchema      = 1
	ContextSchema    = 1

	maxJSONBytes        = 8 * 1024 * 1024
	maxMemoryBytes      = 1024 * 1024
	maxInboxBytes       = 1024 * 1024
	maxHookInputBytes   = 1024 * 1024
	maxCodexRPCBytes    = 32 * 1024 * 1024
	defaultCheckTimeout = time.Hour

	handoffExitCode = 75

	internalSupervise = "__supervise"
	internalValidate  = "__validate"
	internalLease     = "__lease"
	internalInboxHook = "__inbox-hook"
	internalProvision = "__provision-hook"

	envInheritedLockFDs = "MYRIAD_INHERITED_LOCK_FDS"
	envLockSessionPath  = "MYRIAD_LOCK_SESSION_PATH"
	envLockSessionID    = "MYRIAD_LOCK_SESSION_ID"
	envAgentSessionPath = "MYRIAD_SESSION_PATH"
	envAgentSessionID   = "MYRIAD_SESSION_ID"
	envCodexRecoveryCWD = "MYRIAD_CODEX_RECOVERY_CWD"

	worktreePending  = "pending"
	worktreeCreating = "creating"
	worktreeReady    = "ready"

	codexPendingThreadName = "\u200b"
	codexSlugModel         = "gpt-5.6-luna"
	codexSlugLimit         = 48
	codexSlugPreviewLimit  = 4000

	notificationProtocol = 1
)

var forbiddenLocalPaths = []string{MemoryName}

var genericTaskDescriptions = map[string]struct{}{
	"interactive agent task":       {},
	"interactive task":             {},
	"resume a saved codex session": {},
}
